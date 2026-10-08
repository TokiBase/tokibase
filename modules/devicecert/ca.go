//go:build !no_devicecert

package devicecert

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"strings"
	"time"

	"github.com/tokibase/tokibase/kernel"
)

// caWrapInfo is the HKDF info (and the label that is signed) of the key that
// wraps the CA private key in `_devicecert_state`.
const caWrapInfo = "toki_devicecert/ca/v1"

const maxIPs = 8

var (
	// ErrNotHub is returned when a CA operation runs on a node that is not the hub.
	ErrNotHub = errors.New("devicecert: this node is not the hub (the CA lives on the hub only)")
	// ErrBadKey is returned for a public key that is not ECDSA P-256.
	ErrBadKey = errors.New("devicecert: the public key must be an ECDSA P-256 key")
)

// CA is the hub certificate authority: an ECDSA P-256 root.
type CA struct {
	Cert *x509.Certificate
	Key  *ecdsa.PrivateKey
	PEM  []byte
}

// Fingerprint is the SHA-256 of the DER root certificate, hex with colons.
func (c *CA) Fingerprint() string { return Fingerprint(c.Cert.Raw) }

// Fingerprint formats the SHA-256 of a DER certificate as AA:BB:...
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	h := strings.ToUpper(hex.EncodeToString(sum[:]))
	parts := make([]string, 0, len(h)/2)
	for i := 0; i < len(h); i += 2 {
		parts = append(parts, h[i:i+2])
	}
	return strings.Join(parts, ":")
}

func randSerial() (*big.Int, error) {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, err
	}
	return n.Add(n, big.NewInt(1)), nil
}

// SerialHex renders a serial as lower case hex.
func SerialHex(n *big.Int) string { return n.Text(16) }

// NewCA creates a 10-year self-signed ECDSA P-256 root for the hub.
func NewCA(hubID string, now time.Time) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := randSerial()
	if err != nil {
		return nil, err
	}
	cn := "Tokibase Edge CA"
	if hubID != "" {
		cn += " " + hubID
	}
	tpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn, Organization: []string{"Tokibase"}},
		NotBefore:             now.Add(-Backdate),
		NotAfter:              now.Add(CADays * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            1, // room for the v1.1 edge intermediate
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &CA{Cert: cert, Key: key, PEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}, nil
}

// wrapKey derives the AES-256 key that protects the CA key from the secret.
// The secret is the hub's Ed25519 signature over [caWrapInfo]: it is
// deterministic and only the hub key can produce it, so a copy of data.db
// without the hub key does not open the CA (the same property the sync
// session secret has).
func wrapKey(secret []byte) ([]byte, error) {
	return hkdf.Key(sha256.New, secret, nil, caWrapInfo, 32)
}

func wrapAAD(certPEM []byte) []byte {
	sum := sha256.Sum256(certPEM)
	return append([]byte(caWrapInfo+"|"), sum[:]...)
}

// WrapCAKey encrypts the DER (PKCS#8) CA key with AES-256-GCM. The nonce is
// prepended; the root PEM is authenticated as additional data.
func WrapCAKey(secret []byte, ca *CA) ([]byte, error) {
	k, err := wrapKey(secret)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(ca.Key)
	if err != nil {
		return nil, err
	}
	gcm, err := newGCM(k)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, der, wrapAAD(ca.PEM)), nil
}

// UnwrapCA decrypts a wrapped CA key and returns the CA for the root PEM. It
// fails when the secret is not the one that wrapped the key.
func UnwrapCA(secret, wrapped, certPEM []byte) (*CA, error) {
	k, err := wrapKey(secret)
	if err != nil {
		return nil, err
	}
	gcm, err := newGCM(k)
	if err != nil {
		return nil, err
	}
	if len(wrapped) < gcm.NonceSize()+gcm.Overhead() {
		return nil, errors.New("devicecert: the wrapped CA key is truncated")
	}
	der, err := gcm.Open(nil, wrapped[:gcm.NonceSize()], wrapped[gcm.NonceSize():], wrapAAD(certPEM))
	if err != nil {
		return nil, errors.New("devicecert: the CA key cannot be unwrapped (wrong hub key?)")
	}
	pk, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, err
	}
	key, ok := pk.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("devicecert: the CA key is not ECDSA")
	}
	cert, err := ParseCertPEM(certPEM)
	if err != nil {
		return nil, err
	}
	pub, _ := cert.PublicKey.(*ecdsa.PublicKey)
	if pub == nil || !pub.Equal(&key.PublicKey) {
		return nil, errors.New("devicecert: the CA key does not match the root certificate")
	}
	return &CA{Cert: cert, Key: key, PEM: certPEM}, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

// ParseCertPEM parses the first certificate of a PEM block.
func ParseCertPEM(p []byte) (*x509.Certificate, error) {
	b, _ := pem.Decode(p)
	if b == nil || b.Type != "CERTIFICATE" {
		return nil, errors.New("devicecert: no certificate in PEM")
	}
	return x509.ParseCertificate(b.Bytes)
}

// LeafParams describes a certificate to issue.
type LeafParams struct {
	Name string
	Kind kernel.DeviceCertKind
	// SPKI is the DER SubjectPublicKeyInfo to certify (ECDSA P-256).
	SPKI []byte
	DNS  []string
	IPs  []net.IP
	Days int
}

// ParseSPKI parses and checks a DER public key.
func ParseSPKI(der []byte) (*ecdsa.PublicKey, error) {
	pk, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, ErrBadKey
	}
	pub, ok := pk.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		return nil, ErrBadKey
	}
	return pub, nil
}

// Issue signs a leaf. The NotBefore is backdated by [Backdate] so a device
// with a bad clock accepts it. A server leaf is valid for serverAuth only; a
// client leaf for clientAuth only.
func (c *CA) Issue(now time.Time, p LeafParams) (*x509.Certificate, []byte, error) {
	pub, err := ParseSPKI(p.SPKI)
	if err != nil {
		return nil, nil, err
	}
	if p.Name == "" {
		return nil, nil, errors.New("devicecert: a name is required")
	}
	days, limit := p.Days, MaxLeafDays
	if p.Kind == kernel.DeviceCertClient {
		limit = MaxClientDays
	}
	if days <= 0 {
		days = DefaultLeafDays
	}
	days = min(days, limit)
	serial, err := randSerial()
	if err != nil {
		return nil, nil, err
	}
	// the usages are split: a server leaf can never act as a client
	// certificate of a LAN peer, and a client certificate cannot serve TLS
	eku := []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	if p.Kind != kernel.DeviceCertClient {
		eku = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	}
	notAfter := now.Add(time.Duration(days) * 24 * time.Hour)
	if notAfter.After(c.Cert.NotAfter) {
		notAfter = c.Cert.NotAfter
	}
	tpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: p.Name, Organization: []string{"Tokibase"}},
		NotBefore:             now.Add(-Backdate),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           eku,
		BasicConstraintsValid: true,
		DNSNames:              p.DNS,
		IPAddresses:           p.IPs,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, c.Cert, pub, c.Key)
	if err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, err
	}
	return cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

// VerifyLeaf checks a certificate against the root at now for the usage.
func VerifyLeaf(root *x509.Certificate, leaf *x509.Certificate, now time.Time, usage x509.ExtKeyUsage) error {
	pool := x509.NewCertPool()
	pool.AddCert(root)
	_, err := leaf.Verify(x509.VerifyOptions{Roots: pool, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{usage}})
	return err
}

// SplitSANs separates a list of SAN strings into DNS names and IPs, dropping
// duplicates and unusable entries.
func SplitSANs(sans []string) (dns []string, ips []net.IP) {
	seen := map[string]bool{}
	for _, s := range sans {
		s = strings.TrimSpace(s)
		if s == "" || seen[strings.ToLower(s)] {
			continue
		}
		seen[strings.ToLower(s)] = true
		if ip := net.ParseIP(s); ip != nil {
			if ip.IsUnspecified() || ip.IsMulticast() {
				continue
			}
			ips = append(ips, ip)
			continue
		}
		if validDNS(s) {
			dns = append(dns, strings.ToLower(s))
		}
	}
	return dns, ips
}

func validDNS(s string) bool {
	if len(s) == 0 || len(s) > 253 {
		return false
	}
	for _, l := range strings.Split(s, ".") {
		if len(l) == 0 || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for i := 0; i < len(l); i++ {
			c := l[i]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

// KeyPEM encodes an ECDSA key as PKCS#8 PEM.
func KeyPEM(k *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// ParseKeyPEM parses a PKCS#8 or SEC1 ECDSA key.
func ParseKeyPEM(p []byte) (*ecdsa.PrivateKey, error) {
	b, _ := pem.Decode(p)
	if b == nil {
		return nil, errors.New("devicecert: no key in PEM")
	}
	if k, err := x509.ParsePKCS8PrivateKey(b.Bytes); err == nil {
		if ek, ok := k.(*ecdsa.PrivateKey); ok {
			return ek, nil
		}
		return nil, fmt.Errorf("devicecert: key is %T, want ECDSA", k)
	}
	return x509.ParseECPrivateKey(b.Bytes)
}

// retireHeader is the PEM header that marks a rotated-out root in a bundle:
// the root stays in the trust pool until that time (RFC 3339).
const retireHeader = "Toki-Retire-At"

// Root is one certificate of a root bundle.
type Root struct {
	Cert *x509.Certificate
	// PEM is the root as stored (without the retire header).
	PEM []byte
	// RetireAt is when the root leaves the trust pool; zero for the current root.
	RetireAt time.Time
}

// Active reports whether the root is still trusted at now.
func (r Root) Active(now time.Time) bool { return r.RetireAt.IsZero() || now.Before(r.RetireAt) }

// EncodeRoot returns the bundle block of a root; retireAt adds the retire header.
func EncodeRoot(certPEM []byte, retireAt time.Time) []byte {
	b, _ := pem.Decode(certPEM)
	if b == nil {
		return nil
	}
	blk := &pem.Block{Type: b.Type, Bytes: b.Bytes}
	if !retireAt.IsZero() {
		blk.Headers = map[string]string{retireHeader: retireAt.UTC().Format(time.RFC3339)}
	}
	return pem.EncodeToMemory(blk)
}

// ParseBundle parses every certificate of a PEM bundle (newest root first).
func ParseBundle(p []byte) ([]Root, error) {
	var out []Root
	for {
		var b *pem.Block
		b, p = pem.Decode(p)
		if b == nil {
			break
		}
		if b.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			return nil, err
		}
		r := Root{Cert: c, PEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: b.Bytes})}
		if v := b.Headers[retireHeader]; v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return nil, fmt.Errorf("devicecert: invalid %s header: %w", retireHeader, err)
			}
			r.RetireAt = t
		}
		out = append(out, r)
	}
	if len(out) == 0 {
		return nil, errors.New("devicecert: no certificate in PEM")
	}
	return out, nil
}

// PoolOf returns a pool of the roots that are active at now.
func PoolOf(roots []Root, now time.Time) *x509.CertPool {
	p := x509.NewCertPool()
	for _, r := range roots {
		if r.Active(now) {
			p.AddCert(r.Cert)
		}
	}
	return p
}

// hasUsage reports whether the certificate lists the extended key usage.
func hasUsage(c *x509.Certificate, u x509.ExtKeyUsage) bool {
	for _, e := range c.ExtKeyUsage {
		if e == u {
			return true
		}
	}
	return false
}
