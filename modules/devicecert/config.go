//go:build !no_devicecert

package devicecert

import (
	"github.com/tokibase/tokibase/apis"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultLeafDays is the lifetime of an edge server leaf.
	DefaultLeafDays = 14
	// MaxLeafDays is the longest lifetime of an edge server leaf
	// (TOKI_DEVICECERT_LEAF_DAYS).
	MaxLeafDays = 90
	// MaxClientDays is the longest lifetime of a client certificate.
	MaxClientDays = 365
	// MaxOverlapDays bounds --overlap-days and TOKI_DEVICECERT_CA_OVERLAP_DAYS.
	MaxOverlapDays = 365
	// EnvAcceptRotation lets a node accept a hub bundle with a root it does not
	// know yet (a deliberate CA rotation). Off: a node keeps the root it got first.
	EnvAcceptRotation = "TOKI_DEVICECERT_ACCEPT_ROTATION"
	// CADays is the lifetime of the root (10 years).
	CADays = 3650
	// Backdate is how far NotBefore lies in the past, so a node with a bad RTC
	// still accepts a fresh certificate.
	Backdate = time.Hour

	// RenewFraction: a leaf is renewed when less than 1/RenewFraction of its
	// life is left.
	RenewFraction = 3

	// LeafKeyFile, LeafCertFile and CAFile live in the data dir of a node.
	LeafKeyFile  = "devicecert_leaf.key"
	LeafCertFile = "devicecert_leaf.pem"
	CAFile       = "devicecert_ca.pem"

	// BundleFile holds the leaf and the root bundle in ONE file, written with a
	// single atomic rename (LeafCertFile and CAFile are derived copies).
	BundleFile = "devicecert_bundle.pem"

	// DefaultCAOverlapDays is how long a rotated-out root stays trusted
	// (TOKI_DEVICECERT_CA_OVERLAP_DAYS).
	DefaultCAOverlapDays = 30

	// HeaderDevice is the request header that carries the name of a trusted
	// mTLS device. The server strips it from every inbound request and sets it
	// only for a verified client certificate whose route_scope covers the path.
	HeaderDevice = apis.DeviceHeader

	// NodeDNSSuffix is the DNS name suffix of a node: <node_id>.edge.toki.local.
	NodeDNSSuffix = ".edge.toki.local"
)

// MTLS is the client certificate policy of the listener.
type MTLS string

// Client certificate policies (TOKI_DEVICECERT_MTLS).
const (
	MTLSOff      MTLS = "off"
	MTLSOptional MTLS = "optional"
	MTLSRequire  MTLS = "require"
)

func envOn(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "on", "true", "1", "yes":
		return true
	}
	return false
}

// Enabled reports whether the module is switched on (TOKI_DEVICECERT=on).
func Enabled() bool { return envOn("TOKI_DEVICECERT") }

// ListenAddr is TOKI_DEVICECERT_LISTEN (for example ":8443"), "" when unset.
func ListenAddr() string { return strings.TrimSpace(os.Getenv("TOKI_DEVICECERT_LISTEN")) }

// ListenEnabled reports whether the module is on and the TLS listener is
// configured. modules/tlscheck uses the same rule through the environment.
func ListenEnabled() bool { return Enabled() && ListenAddr() != "" }

// LeafDays is TOKI_DEVICECERT_LEAF_DAYS (default 14, at most 90).
func LeafDays() int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("TOKI_DEVICECERT_LEAF_DAYS"))); err == nil && n > 0 {
		return min(n, MaxLeafDays)
	}
	return DefaultLeafDays
}

// CAOverlapDays is TOKI_DEVICECERT_CA_OVERLAP_DAYS (default 30).
func CAOverlapDays() int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("TOKI_DEVICECERT_CA_OVERLAP_DAYS"))); err == nil && n >= 0 {
		return min(n, MaxOverlapDays)
	}
	return DefaultCAOverlapDays
}

// MTLSMode is TOKI_DEVICECERT_MTLS (default off).
func MTLSMode() MTLS {
	switch MTLS(strings.ToLower(strings.TrimSpace(os.Getenv("TOKI_DEVICECERT_MTLS")))) {
	case MTLSOptional:
		return MTLSOptional
	case MTLSRequire:
		return MTLSRequire
	}
	return MTLSOff
}

// ExtraSANs are the names and addresses of TOKI_DEVICECERT_SANS (comma
// separated) that go into the leaf next to the node name and the LAN addresses.
func ExtraSANs() []string {
	var out []string
	for _, s := range strings.Split(os.Getenv("TOKI_DEVICECERT_SANS"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// lanIPs lists the unicast addresses of this host that clients can reach it
// on: loopback and every global or private address; link-local ones are left out.
func lanIPs() []string {
	out := []string{"127.0.0.1"}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return out
	}
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipn.IP
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
			continue
		}
		out = append(out, ip.String())
		if len(out) >= maxIPs {
			break
		}
	}
	return out
}
