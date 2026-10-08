//go:build !no_devicecert

package devicecert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// leafStore holds the edge server certificate of this node and the root that
// signed it. GetCertificate reads it on every handshake, so a renewed leaf is
// served without restarting the listener.
type leafStore struct {
	dir string

	mu   sync.RWMutex
	cert *tls.Certificate
	leaf *x509.Certificate
	root *x509.Certificate
	pool *x509.CertPool
	// lastReq is the SAN set of the last request, so a hub that filters some
	// of them does not make the node renew again and again.
	lastReq string
}

func newLeafStore(dir string) *leafStore { return &leafStore{dir: dir} }

func (s *leafStore) path(name string) string { return filepath.Join(s.dir, name) }

// key loads the leaf key or creates it (ECDSA P-256, mode 0600).
func (s *leafStore) key() (*ecdsa.PrivateKey, error) {
	p := s.path(LeafKeyFile)
	if b, err := os.ReadFile(p); err == nil {
		return ParseKeyPEM(b)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	b, err := KeyPEM(k)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) { // another process won the race
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		return ParseKeyPEM(b)
	}
	if err != nil {
		return nil, err
	}
	_, werr := f.Write(b)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(p)
		return nil, werr
	}
	return k, nil
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// install checks and stores a leaf and its root. The leaf must certify the
// local key and be signed by the root.
func (s *leafStore) install(certPEM, caPEM []byte, persist bool) error {
	leaf, err := ParseCertPEM(certPEM)
	if err != nil {
		return err
	}
	root, err := ParseCertPEM(caPEM)
	if err != nil {
		return err
	}
	if !root.IsCA {
		return errors.New("devicecert: the root is not a CA certificate")
	}
	if err := leaf.CheckSignatureFrom(root); err != nil {
		return fmt.Errorf("devicecert: the leaf is not signed by the root: %w", err)
	}
	k, err := s.key()
	if err != nil {
		return err
	}
	pub, _ := leaf.PublicKey.(*ecdsa.PublicKey)
	if pub == nil || !pub.Equal(&k.PublicKey) {
		return errors.New("devicecert: the leaf does not certify the local key")
	}
	if !slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageServerAuth) {
		return errors.New("devicecert: the leaf has no serverAuth usage")
	}
	if persist {
		if err := writeFileAtomic(s.path(CAFile), caPEM, 0o644); err != nil {
			return err
		}
		if err := writeFileAtomic(s.path(LeafCertFile), certPEM, 0o644); err != nil {
			return err
		}
	}
	pool := x509.NewCertPool()
	pool.AddCert(root)
	s.mu.Lock()
	s.cert = &tls.Certificate{Certificate: [][]byte{leaf.Raw}, PrivateKey: k, Leaf: leaf}
	s.leaf, s.root, s.pool = leaf, root, pool
	s.mu.Unlock()
	return nil
}

// load reads the stored leaf and root, if any.
func (s *leafStore) load() error {
	c, err := os.ReadFile(s.path(LeafCertFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	ca, err := os.ReadFile(s.path(CAFile))
	if err != nil {
		return err
	}
	return s.install(c, ca, false)
}

func (s *leafStore) current() (leaf, root *x509.Certificate, pool *x509.CertPool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.leaf, s.root, s.pool
}

func (s *leafStore) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.cert == nil {
		return nil, errors.New("devicecert: this node has no edge certificate yet")
	}
	return s.cert, nil
}

// due reports whether the leaf must be renewed at now: there is none, less than
// 1/RenewFraction of its life is left, or the wanted addresses are not in it
// (and were not asked for already).
func (s *leafStore) due(now time.Time, wanted []string) bool {
	s.mu.RLock()
	leaf, last := s.leaf, s.lastReq
	s.mu.RUnlock()
	return leafDue(leaf, now, wanted, last)
}

func leafDue(leaf *x509.Certificate, now time.Time, wanted []string, lastReq string) bool {
	if leaf == nil {
		return true
	}
	life := leaf.NotAfter.Sub(leaf.NotBefore)
	if leaf.NotAfter.Sub(now) < life/RenewFraction {
		return true
	}
	if key := sansKey(wanted); key != lastReq && !coversSANs(leaf, wanted) {
		return true
	}
	return false
}

func sansKey(sans []string) string {
	c := slices.Clone(sans)
	for i := range c {
		c[i] = strings.ToLower(strings.TrimSpace(c[i]))
	}
	sort.Strings(c)
	sum := sha256.Sum256([]byte(strings.Join(c, ",")))
	return hex.EncodeToString(sum[:8])
}

func coversSANs(leaf *x509.Certificate, wanted []string) bool {
	dns, ips := SplitSANs(wanted)
	for _, d := range dns {
		if !slices.Contains(leaf.DNSNames, d) {
			return false
		}
	}
	for _, ip := range ips {
		found := false
		for _, have := range leaf.IPAddresses {
			if have.Equal(ip) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func (s *leafStore) setLastReq(sans []string) {
	s.mu.Lock()
	s.lastReq = sansKey(sans)
	s.mu.Unlock()
}

// spkiOf returns the DER public key of the local leaf key.
func (s *leafStore) spki() ([]byte, error) {
	k, err := s.key()
	if err != nil {
		return nil, err
	}
	return x509.MarshalPKIXPublicKey(&k.PublicKey)
}
