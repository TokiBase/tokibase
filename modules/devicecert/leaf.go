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
	"encoding/pem"
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
	// roots is the bundle received with the leaf, newest first.
	roots []Root
	// lastReq is the SAN set of the last request, so a hub that filters some
	// of them does not make the node renew again and again.
	lastReq string
}

func newLeafStore(dir string) *leafStore { return &leafStore{dir: dir} }

func (s *leafStore) path(name string) string { return filepath.Join(s.dir, name) }

// key loads the leaf key or creates it (ECDSA P-256, mode 0600). The key is
// written to a temporary file and linked into place, so a reader never sees a
// partial file and a crash leaves no truncated key; a key file that cannot be
// parsed is replaced.
func (s *leafStore) key() (*ecdsa.PrivateKey, error) {
	p := s.path(LeafKeyFile)
	exists := false
	if b, err := os.ReadFile(p); err == nil {
		if k, perr := ParseKeyPEM(b); perr == nil {
			return k, nil
		}
		exists = true // corrupt: replaced below
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
	tmp, err := writeTemp(s.dir, b, 0o600)
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp)
	if exists {
		if err := os.Rename(tmp, p); err != nil {
			return nil, err
		}
		return k, nil
	}
	if err := os.Link(tmp, p); err != nil {
		if errors.Is(err, os.ErrExist) { // another process won the race
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				return nil, rerr
			}
			return ParseKeyPEM(b)
		}
		if rerr := os.Rename(tmp, p); rerr != nil { // no hard links on this file system
			return nil, rerr
		}
	}
	return k, nil
}

// writeTemp writes data to a unique temporary file in dir and returns its path.
func writeTemp(dir string, data []byte, mode os.FileMode) (string, error) {
	f, err := os.CreateTemp(dir, ".devicecert-*.tmp")
	if err != nil {
		return "", err
	}
	name := f.Name()
	_, werr := f.Write(data)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(name, mode)
	}
	if werr != nil {
		_ = os.Remove(name)
		return "", werr
	}
	return name, nil
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := writeTemp(filepath.Dir(path), data, mode)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// install checks and stores a leaf and its root bundle (newest root first).
// The leaf must certify the local key and be signed by a root of the bundle.
// With pin set (a node that got the answer from the hub) the leaf must be
// valid now and the bundle must contain a root the node already trusts
// (trust on first use: the first root received is kept; see
// docs/modules/devicecert.md for the manual reset).
func (s *leafStore) install(certPEM, bundlePEM []byte, persist, pin bool) error {
	leaf, err := ParseCertPEM(certPEM)
	if err != nil {
		return err
	}
	roots, err := ParseBundle(bundlePEM)
	if err != nil {
		return err
	}
	now := time.Now()
	var signed bool
	for _, r := range roots {
		if !r.Cert.IsCA {
			return errors.New("devicecert: the root is not a CA certificate")
		}
		if r.Active(now) && leaf.CheckSignatureFrom(r.Cert) == nil {
			signed = true
		}
	}
	if !signed {
		return errors.New("devicecert: the leaf is not signed by a root of the bundle")
	}
	if pin {
		if now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
			return fmt.Errorf("devicecert: the leaf is not valid now (%s .. %s)", leaf.NotBefore.UTC().Format(time.RFC3339), leaf.NotAfter.UTC().Format(time.RFC3339))
		}
		s.mu.RLock()
		known := s.roots
		s.mu.RUnlock()
		if err := checkPin(known, roots, leaf, now); err != nil {
			return err
		}
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
		var bundle []byte
		for _, r := range roots {
			bundle = append(bundle, EncodeRoot(r.PEM, r.RetireAt)...)
		}
		leafPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Raw})
		// one file, one rename: a crash cannot leave a leaf next to a stale root
		if err := writeFileAtomic(s.path(BundleFile), append(append([]byte{}, leafPEM...), bundle...), 0o644); err != nil {
			return err
		}
		// derived copies for `toki devicecert ca` and operators
		_ = writeFileAtomic(s.path(CAFile), bundle, 0o644)
		_ = writeFileAtomic(s.path(LeafCertFile), leafPEM, 0o644)
	}
	s.mu.Lock()
	s.cert = &tls.Certificate{Certificate: [][]byte{leaf.Raw}, PrivateKey: k, Leaf: leaf}
	s.leaf, s.roots = leaf, roots
	s.mu.Unlock()
	return nil
}

const pinHelp = "if the hub CA was replaced on purpose, set " + EnvAcceptRotation + "=on for one renewal, or delete " +
	BundleFile + ", " + CAFile + " and " + LeafCertFile + " in the data dir"

// checkPin enforces the root pin of a node that got its bundle from the hub.
// Without an explicit rotation (EnvAcceptRotation) the leaf must be signed by a
// root the node already trusts and the bundle may not add roots. With it, the
// bundle must still contain a root the node trusts and that is active.
func checkPin(known, roots []Root, leaf *x509.Certificate, now time.Time) error {
	if len(known) == 0 {
		return nil
	}
	if envOn(EnvAcceptRotation) {
		for _, k := range known {
			if !k.Active(now) {
				continue
			}
			for _, r := range roots {
				if k.Cert.Equal(r.Cert) {
					return nil
				}
			}
		}
		return errors.New("devicecert: the hub bundle shares no active root with this node (it pins the root it received first); " + pinHelp)
	}
	signed := false
	for _, k := range known {
		if k.Active(now) && leaf.CheckSignatureFrom(k.Cert) == nil {
			signed = true
		}
	}
	if !signed {
		return errors.New("devicecert: the leaf is not signed by a root this node trusts (it pins the root it received first); " + pinHelp)
	}
	for _, r := range roots {
		if !slices.ContainsFunc(known, func(k Root) bool { return k.Cert.Equal(r.Cert) }) {
			return errors.New("devicecert: the hub bundle adds a root this node does not know (it pins the root it received first); " + pinHelp)
		}
	}
	return nil
}

func sharesRoot(a, b []Root) bool {
	for _, x := range a {
		for _, y := range b {
			if x.Cert.Equal(y.Cert) {
				return true
			}
		}
	}
	return false
}

// load reads the stored leaf and roots, if any.
func (s *leafStore) load() error {
	if b, err := os.ReadFile(s.path(BundleFile)); err == nil {
		blk, rest := pem.Decode(b)
		if blk == nil || blk.Type != "CERTIFICATE" {
			return errors.New("devicecert: invalid " + BundleFile)
		}
		return s.install(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: blk.Bytes}), rest, false, false)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
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
	return s.install(c, ca, false, false)
}

// current returns the leaf, the newest root and the pool of the roots that
// are active now.
func (s *leafStore) current() (leaf, root *x509.Certificate, pool *x509.CertPool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.roots) > 0 {
		root = s.roots[0].Cert
		pool = PoolOf(s.roots, time.Now())
	}
	return s.leaf, root, pool
}

// rootCount is the number of roots this node trusts now.
func (s *leafStore) rootCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, r := range s.roots {
		if r.Active(time.Now()) {
			n++
		}
	}
	return n
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
