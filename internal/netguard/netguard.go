// Package netguard is the SSRF guard shared by every module that makes
// outgoing HTTP requests on behalf of untrusted configuration or code
// (webhooks, wasm guests). It is an internal package, not a module.
package netguard

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"syscall"
	"time"
)

var extraBlocked = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // CGNAT
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),    // reserved + broadcast
	netip.MustParsePrefix("192.88.99.0/24"), // 6to4 relay anycast
	netip.MustParsePrefix("64:ff9b::/96"),   // NAT64: embeds any IPv4, incl. metadata/private
	netip.MustParsePrefix("64:ff9b:1::/48"), // local-use NAT64
	netip.MustParsePrefix("2002::/16"),      // 6to4
	netip.MustParsePrefix("2001::/32"),      // Teredo
	netip.MustParsePrefix("::/96"),          // IPv4-compatible
	netip.MustParsePrefix("fec0::/10"),      // site-local
}

// BlockedIP reports whether ip is a loopback, private, link-local,
// unspecified, multicast or otherwise non-public address.
func BlockedIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	for _, p := range extraBlocked {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// EnvTrue reports whether the env var is "1" or "true" (read on every call).
func EnvTrue(name string) bool {
	v := strings.TrimSpace(os.Getenv(name))
	return v == "1" || strings.EqualFold(v, "true")
}

// ControlFunc returns a net.Dialer Control that rejects connections to
// non-public IPs. It runs with the already resolved address, so it also
// defeats DNS rebinding and redirects to internal hosts. The guard is lifted
// when the env var allowEnv is true; errBlocked is returned on a block.
func ControlFunc(allowEnv string, errBlocked error) func(network, address string, c syscall.RawConn) error {
	return func(network, address string, _ syscall.RawConn) error {
		if allowEnv != "" && EnvTrue(allowEnv) {
			return nil
		}
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip, err := netip.ParseAddr(host)
		if err != nil {
			return fmt.Errorf("cannot parse resolved address %q", host)
		}
		if BlockedIP(ip) {
			if errBlocked == nil {
				errBlocked = errors.New("target resolves to a private, loopback or link-local address")
			}
			return errBlocked
		}
		return nil
	}
}

// NewClient returns an HTTP client with the guard installed: no proxy (it
// would bypass the guard), no keep-alive, redirects are not followed.
func NewClient(timeout time.Duration, allowEnv string, errBlocked error) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: ControlFunc(allowEnv, errBlocked)}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:               nil,
			DialContext:         dialer.DialContext,
			DisableKeepAlives:   true,
			TLSHandshakeTimeout: 10 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}
