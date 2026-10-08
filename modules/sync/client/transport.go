//go:build !no_sync

package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"

	"github.com/tokibase/tokibase/modules/sync/proto"
)

// insecureHostOK reports whether plain http is acceptable for the host:
// loopback and private (RFC 1918 / ULA) addresses and localhost only.
func insecureHostOK(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(strings.Trim(host, "[]"))
	return err == nil && (ip.IsLoopback() || ip.IsPrivate())
}

// checkURL enforces https unless insecure and returns the base URL. Insecure
// only allows loopback and private network hosts: the enroll code, the
// certificate and the session token must not cross the internet in clear.

func checkURL(raw string, insecure bool, pin string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("sync: invalid hub url %q", raw)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !insecure {
			return "", errors.New("sync: the hub url must be https (TOKI_SYNC_INSECURE=1 allows http for loopback and private hosts)")
		}
		if !insecureHostOK(u.Hostname()) {
			return "", fmt.Errorf("sync: TOKI_SYNC_INSECURE only allows http for loopback and private network hosts, not %q", u.Hostname())
		}
		if pin != "" {
			return "", errors.New("sync: a hub pin needs an https hub url")
		}
	default:
		return "", fmt.Errorf("sync: unsupported hub url scheme %q", u.Scheme)
	}
	return strings.TrimRight(u.Scheme+"://"+u.Host+u.Path, "/"), nil
}

// parsePin accepts a sha256 as hex (64 chars) or base64.
func parsePin(s string) ([]byte, error) {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "sha256/"))
	if b, err := hex.DecodeString(s); err == nil && len(b) == sha256.Size {
		return b, nil
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil && len(b) == sha256.Size {
			return b, nil
		}
	}
	return nil, errors.New("sync: TOKI_SYNC_HUB_PIN must be the sha256 of the hub certificate public key (hex or base64)")
}

func newHTTP(pin string) (*http.Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if pin != "" {
		want, err := parsePin(pin)
		if err != nil {
			return nil, err
		}
		tr.TLSClientConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
			// the normal chain verification stays on; the pin is an addition
			VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
				if len(raw) == 0 {
					return errors.New("sync: no hub certificate")
				}
				c, err := x509.ParseCertificate(raw[0])
				if err != nil {
					return err
				}
				got := sha256.Sum256(c.RawSubjectPublicKeyInfo)
				if !bytes.Equal(got[:], want) {
					return errors.New("sync: hub certificate does not match TOKI_SYNC_HUB_PIN")
				}
				return nil
			},
		}
	}
	return noRedirect(&http.Client{Timeout: RequestTimeout, Transport: tr}), nil
}

// noRedirect returns a copy of hc that never follows a redirect: a redirect
// would re-send the one-time enroll code or a signed handshake to a host the
// operator did not configure.
func noRedirect(hc *http.Client) *http.Client {
	cp := *hc
	cp.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &cp
}

func (c *Client) do(ctx context.Context, method, path string, hdr map[string]string, body []byte) (*http.Response, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer res.Body.Close()
	if res.StatusCode/100 == 3 {
		return res, nil, fmt.Errorf("sync: the hub answered with a redirect (%d) which is refused", res.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return nil, nil, err
	}
	if res.StatusCode/100 != 2 {
		he := &Error{Status: res.StatusCode}
		if ra := res.Header.Get("Retry-After"); ra != "" {
			if n, err := strconv.Atoi(strings.TrimSpace(ra)); err == nil && n > 0 {
				he.RetryAfter = time.Duration(n) * time.Second
			} else if t, err := http.ParseTime(ra); err == nil {
				he.RetryAfter = max(time.Until(t), 0)
			}
		}
		var eb proto.ErrorBody
		if json.Unmarshal(b, &eb) == nil {
			he.Message, he.Data = eb.Message, eb.Data
			he.Code, _ = eb.Data["code"].(string)
		}
		return res, b, he
	}
	return res, b, nil
}
