//go:build !no_devicecert

package devicecert

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tests"
)

type fakeIdent struct {
	hub, node string
	priv      ed25519.PrivateKey
}

func (f fakeIdent) NodeID() string                  { return f.node }
func (f fakeIdent) HubID() string                   { return f.hub }
func (f fakeIdent) HubPub() ed25519.PublicKey       { return f.priv.Public().(ed25519.PublicKey) }
func (f fakeIdent) Cert() string                    { return "" }
func (f fakeIdent) Sign(msg []byte) ([]byte, error) { return ed25519.Sign(f.priv, msg), nil }

func newHubIdent(t *testing.T) fakeIdent {
	t.Helper()
	_, k, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return fakeIdent{hub: "hhub000000000001", node: "hhub000000000001", priv: k}
}

func newHubModule(t *testing.T, ident kernel.NodeIdentity) (*Module, *tests.TestApp) {
	t.Helper()
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)
	kernel.SetNodeIdentity(app, ident)
	t.Cleanup(func() { kernel.SetNodeIdentity(app, nil) })
	if err := ensureSchema(app); err != nil {
		t.Fatal(err)
	}
	m := New(app)
	m.lanIPs = func() []string { return []string{"127.0.0.1"} }
	return m, app
}

func newKey(t *testing.T) (*ecdsa.PrivateKey, []byte) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return k, der
}

func TestCAIssueAndVerify(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	ca, err := NewCA("hhub1", now)
	if err != nil {
		t.Fatal(err)
	}
	if !ca.Cert.IsCA || ca.Cert.NotAfter.Sub(ca.Cert.NotBefore) < 3650*24*time.Hour {
		t.Fatalf("root must be a ten-year CA: %v", ca.Cert.NotAfter.Sub(ca.Cert.NotBefore))
	}
	if ca.Cert.PublicKey.(*ecdsa.PublicKey).Curve != elliptic.P256() {
		t.Fatal("root must be P-256")
	}
	if !strings.Contains(ca.Fingerprint(), ":") || len(ca.Fingerprint()) != 95 {
		t.Fatalf("fingerprint %q", ca.Fingerprint())
	}
	_, spki := newKey(t)
	dns, ips := SplitSANs([]string{"n123.edge.toki.local", "192.168.1.10", "bad name", "192.168.1.10", "0.0.0.0"})
	cert, _, err := ca.Issue(now, LeafParams{Name: "n123", Kind: kernel.DeviceCertServer, SPKI: spki, DNS: dns, IPs: ips, Days: 14})
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyLeaf(ca.Cert, cert, now.Add(time.Minute), x509.ExtKeyUsageServerAuth); err != nil {
		t.Fatalf("server usage: %v", err)
	}
	if err := VerifyLeaf(ca.Cert, cert, now.Add(time.Minute), x509.ExtKeyUsageClientAuth); err != nil {
		t.Fatalf("a server leaf also has clientAuth: %v", err)
	}
	if err := cert.VerifyHostname("n123.edge.toki.local"); err != nil {
		t.Fatal(err)
	}
	if err := cert.VerifyHostname("192.168.1.10"); err != nil {
		t.Fatal(err)
	}
	if cert.VerifyHostname("other.edge.toki.local") == nil {
		t.Fatal("another node name must not match")
	}
	if len(cert.DNSNames) != 1 || len(cert.IPAddresses) != 1 {
		t.Fatalf("SANs %v %v", cert.DNSNames, cert.IPAddresses)
	}
	// backdated one hour for devices with a bad clock
	if got := now.Sub(cert.NotBefore); got != time.Hour {
		t.Fatalf("backdate %v", got)
	}
	if err := VerifyLeaf(ca.Cert, cert, now.Add(-30*time.Minute), x509.ExtKeyUsageServerAuth); err != nil {
		t.Fatalf("a clock 30 min behind must be accepted: %v", err)
	}
	// expired / not yet valid
	if err := VerifyLeaf(ca.Cert, cert, now.Add(15*24*time.Hour), x509.ExtKeyUsageServerAuth); err == nil {
		t.Fatal("an expired leaf must be rejected")
	}
	if err := VerifyLeaf(ca.Cert, cert, now.Add(-2*time.Hour), x509.ExtKeyUsageServerAuth); err == nil {
		t.Fatal("a leaf before NotBefore must be rejected")
	}
	// a client leaf has no serverAuth
	cl, _, err := ca.Issue(now, LeafParams{Name: "gate-ctrl-1", Kind: kernel.DeviceCertClient, SPKI: spki, Days: 90})
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyLeaf(ca.Cert, cl, now, x509.ExtKeyUsageServerAuth); err == nil {
		t.Fatal("a client certificate must not be accepted as a server certificate")
	}
	if err := VerifyLeaf(ca.Cert, cl, now, x509.ExtKeyUsageClientAuth); err != nil {
		t.Fatal(err)
	}
	// another CA does not verify it
	other, _ := NewCA("hother", now)
	if err := VerifyLeaf(other.Cert, cert, now, x509.ExtKeyUsageServerAuth); err == nil {
		t.Fatal("a leaf must not verify against another root")
	}
	// lifetime caps
	long, _, _ := ca.Issue(now, LeafParams{Name: "x", Kind: kernel.DeviceCertServer, SPKI: spki, Days: 5000})
	if d := long.NotAfter.Sub(now); d != time.Duration(MaxLeafDays)*24*time.Hour {
		t.Fatalf("server leaf is capped at %d days, got %v", MaxLeafDays, d)
	}
	// keys other than P-256 are refused
	if _, _, err := ca.Issue(now, LeafParams{Name: "x", SPKI: []byte("junk")}); err == nil {
		t.Fatal("junk key accepted")
	}
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	der, _ := x509.MarshalPKIXPublicKey(&p384.PublicKey)
	if _, _, err := ca.Issue(now, LeafParams{Name: "x", SPKI: der}); err != ErrBadKey {
		t.Fatalf("P-384 must be refused, got %v", err)
	}
	ed, _, _ := ed25519.GenerateKey(rand.Reader)
	der, _ = x509.MarshalPKIXPublicKey(ed)
	if _, _, err := ca.Issue(now, LeafParams{Name: "x", SPKI: der}); err != ErrBadKey {
		t.Fatalf("Ed25519 must be refused, got %v", err)
	}
}

func TestWrappedCAKeyNeedsTheHubKey(t *testing.T) {
	now := time.Now().UTC()
	ca, _ := NewCA("h1", now)
	hub := newHubIdent(t)
	secret, _ := wrapSecret(hub)
	wrapped, err := WrapCAKey(secret, ca)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(ca.Key)
	if strings.Contains(string(wrapped), string(der[len(der)-24:])) {
		t.Fatal("the key must not appear in the clear")
	}
	back, err := UnwrapCA(secret, wrapped, ca.PEM)
	if err != nil || !back.Key.Equal(ca.Key) {
		t.Fatalf("unwrap: %v", err)
	}
	// another hub key: same data.db, wrong key
	other := newHubIdent(t)
	otherSecret, _ := wrapSecret(other)
	if _, err := UnwrapCA(otherSecret, wrapped, ca.PEM); err == nil {
		t.Fatal("a copy of data.db without the hub key must not open the CA")
	}
	// the signature is deterministic (needed to unwrap after a restart)
	again, _ := wrapSecret(hub)
	if string(again) != string(secret) {
		t.Fatal("the wrap secret must be deterministic")
	}
	// the root PEM is authenticated: a swapped root does not unwrap
	other2, _ := NewCA("h2", now)
	if _, err := UnwrapCA(secret, wrapped, other2.PEM); err == nil {
		t.Fatal("a wrapped key must be bound to its root")
	}
	// truncated / flipped
	if _, err := UnwrapCA(secret, wrapped[:10], ca.PEM); err == nil {
		t.Fatal("truncated blob accepted")
	}
	flip := slices.Clone(wrapped)
	flip[len(flip)-1] ^= 1
	if _, err := UnwrapCA(secret, flip, ca.PEM); err == nil {
		t.Fatal("tampered blob accepted")
	}
}

func TestHubCAPersistsAndIssuesRecords(t *testing.T) {
	hub := newHubIdent(t)
	m, app := newHubModule(t, hub)
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }

	if ca, err := m.CA(false); err != nil || ca != nil {
		t.Fatalf("no CA yet: %v %v", ca, err)
	}
	var audited []string
	SetAuditSink(func(action, _, record string, _ map[string]any) { audited = append(audited, action+":"+record) })
	defer SetAuditSink(nil)

	c, err := m.Issue(context.Background(), kernel.DeviceCertRequest{Name: "n1", Node: "n1", Kind: kernel.DeviceCertServer, SANs: []string{"10.1.2.3"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.KeyPEM) == 0 || len(c.CAPEM) == 0 || len(c.CertPEM) == 0 {
		t.Fatal("a provider-generated key comes back with the certificate")
	}
	leaf, _ := ParseCertPEM(c.CertPEM)
	root, _ := ParseCertPEM(c.CAPEM)
	if err := VerifyLeaf(root, leaf, now, x509.ExtKeyUsageServerAuth); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(leaf.DNSNames, "n1"+NodeDNSSuffix) {
		t.Fatalf("the node name must be a SAN: %v", leaf.DNSNames)
	}
	if got := leaf.NotAfter.Sub(now); got != DefaultLeafDays*24*time.Hour {
		t.Fatalf("default lifetime %v", got)
	}
	if len(audited) != 1 || audited[0] != "devicecert.issue:"+c.Serial {
		t.Fatalf("audit %v", audited)
	}

	// the row: system collection, rules nil
	col, _ := app.FindCollectionByNameOrId(CertsCollection)
	if col == nil || !col.System || col.ListRule != nil || col.ViewRule != nil || col.CreateRule != nil || col.UpdateRule != nil || col.DeleteRule != nil {
		t.Fatalf("_device_certs must be a system collection with null rules: %+v", col)
	}
	got, err := m.Lookup(context.Background(), c.Serial)
	if err != nil || got.Name != "n1" || got.Kind != kernel.DeviceCertServer || got.Node != "n1" || !got.RevokedAt.IsZero() {
		t.Fatalf("lookup: %+v %v", got, err)
	}
	if byName, err := m.Lookup(context.Background(), "n1"); err != nil || byName.Serial != c.Serial {
		t.Fatalf("lookup by name: %+v %v", byName, err)
	}
	if _, err := m.Lookup(context.Background(), "nope"); err != kernel.ErrDeviceCertNotFound {
		t.Fatalf("unknown: %v", err)
	}

	// "restart": a new module over the same database opens the same CA
	m2 := New(app)
	ca2, err := m2.CA(false)
	if err != nil || ca2 == nil || ca2.Fingerprint() != Fingerprint(root.Raw) {
		t.Fatalf("restart: %v", err)
	}
	// a hub with another key cannot open it
	kernel.SetNodeIdentity(app, newHubIdent(t))
	if _, err := New(app).CA(false); err == nil {
		t.Fatal("a different hub key must not open the CA")
	}
	kernel.SetNodeIdentity(app, hub)

	// revoke
	if err := m.Revoke(context.Background(), "n1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.Lookup(context.Background(), c.Serial); got.RevokedAt.IsZero() {
		t.Fatal("revoked_at must be set")
	}
	if !m.deny.has(c.Serial) {
		t.Fatal("a revoked serial must be on the deny list")
	}
	if !slices.Contains(audited, "devicecert.revoke:"+c.Serial) {
		t.Fatalf("audit %v", audited)
	}
	if err := m.Revoke(context.Background(), "nope"); err != kernel.ErrDeviceCertNotFound {
		t.Fatalf("revoke unknown: %v", err)
	}
}

func TestOnlyTheHubIssues(t *testing.T) {
	// a spoke: node id differs from the hub id
	_, k, _ := ed25519.GenerateKey(rand.Reader)
	m, _ := newHubModule(t, fakeIdent{hub: "hhub000000000001", node: "nspoke0000000001", priv: k})
	if _, err := m.Issue(context.Background(), kernel.DeviceCertRequest{Name: "x"}); err != ErrNotHub {
		t.Fatalf("spoke issue: %v", err)
	}
	// no sync at all
	app, _ := tests.NewTestApp()
	defer app.Cleanup()
	_ = ensureSchema(app)
	if _, err := New(app).Issue(context.Background(), kernel.DeviceCertRequest{Name: "x"}); err == nil {
		t.Fatal("without sync there is no CA")
	}
}

func TestLeafRenewalThreshold(t *testing.T) {
	hub := newHubIdent(t)
	m, _ := newHubModule(t, hub)
	m.leaf = newLeafStore(t.TempDir())
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return base }

	// no leaf: due, and a request carries the local public key
	req, err := m.LeafRequest(base)
	if err != nil || req == nil || len(req.SPKI) == 0 {
		t.Fatalf("first request: %v %v", req, err)
	}
	if !slices.Contains(req.SANs, "127.0.0.1") {
		t.Fatalf("sans %v", req.SANs)
	}
	if _, err := ParseSPKI(req.SPKI); err != nil {
		t.Fatal(err)
	}
	// the private key stays on the node, mode 0600
	kp := filepath.Join(m.leaf.dir, LeafKeyFile)
	if st, err := os.Stat(kp); err != nil {
		t.Fatal(err)
	} else if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("leaf key mode %v", st.Mode().Perm())
	}
	// the same key on the next call
	req2, _ := m.LeafRequest(base)
	if string(req2.SPKI) != string(req.SPKI) {
		t.Fatal("the leaf key must be stable")
	}

	c, err := m.Issue(context.Background(), kernel.DeviceCertRequest{Name: "n", Node: "n", Kind: kernel.DeviceCertServer, SANs: req.SANs, SPKI: req.SPKI, Days: 14})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.InstallLeaf(c.CertPEM, c.CAPEM); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{LeafCertFile, CAFile} {
		if _, err := os.Stat(filepath.Join(m.leaf.dir, f)); err != nil {
			t.Fatal(err)
		}
	}
	// fresh: nothing to do. life is 14 d + 1 h backdate; renew in the last third
	for _, tc := range []struct {
		after time.Duration
		due   bool
	}{
		{0, false}, {5 * 24 * time.Hour, false}, {9 * 24 * time.Hour, false},
		{10 * 24 * time.Hour, true}, {13 * 24 * time.Hour, true}, {20 * 24 * time.Hour, true},
	} {
		got, err := m.LeafRequest(base.Add(tc.after))
		if err != nil || (got != nil) != tc.due {
			t.Fatalf("after %v: due=%v err=%v", tc.after, got != nil, err)
		}
	}
	// a new LAN address is not in the leaf: renew once, not forever
	m.lanIPs = func() []string { return []string{"127.0.0.1", "192.168.50.2"} }
	if got, _ := m.LeafRequest(base); got == nil {
		t.Fatal("a new address must trigger a renewal")
	}
	if err := m.InstallLeaf(c.CertPEM, c.CAPEM); err != nil { // the hub filtered the new address
		t.Fatal(err)
	}
	if got, _ := m.LeafRequest(base); got != nil {
		t.Fatal("the same set of addresses must not be asked for again")
	}
	// a leaf for another key is refused
	m2 := New(m.app)
	m2.leaf = newLeafStore(t.TempDir())
	if err := m2.InstallLeaf(c.CertPEM, c.CAPEM); err == nil {
		t.Fatal("a leaf that does not certify the local key must be refused")
	}
	// a leaf with the wrong root is refused
	otherCA, _ := NewCA("hx", base)
	if err := m.InstallLeaf(c.CertPEM, otherCA.PEM); err == nil {
		t.Fatal("a leaf not signed by the given root must be refused")
	}
	// stored files reload
	m3 := New(m.app)
	m3.leaf = newLeafStore(m.leaf.dir)
	if err := m3.leaf.load(); err != nil {
		t.Fatal(err)
	}
	if leaf, _, _ := m3.leaf.current(); leaf == nil || SerialHex(leaf.SerialNumber) != c.Serial {
		t.Fatal("the stored leaf must reload")
	}
}

func TestLeafDueFunction(t *testing.T) {
	now := time.Now()
	if !leafDue(nil, now, nil, "") {
		t.Fatal("no leaf is due")
	}
}

// startTLS serves handler with the module's TLS config on a loopback port.
func startTLS(t *testing.T, m *Module, h http.Handler) string {
	t.Helper()
	// not StartTLS: it adds a certificate of its own, which Go prefers to GetCertificate when the client sends no SNI
	srv := httptest.NewUnstartedServer(h)
	srv.Listener = tls.NewListener(srv.Listener, m.tlsConfig())
	srv.Start()
	t.Cleanup(srv.Close)
	return "https://" + srv.Listener.Addr().String()
}

func clientFor(t *testing.T, root *x509.Certificate, cert *tls.Certificate) *http.Client {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(root)
	cfg := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	if cert != nil {
		// always offer it: Go would otherwise hold it back when the issuer is not in the server's CA list
		cfg.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return cert, nil }
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}, Timeout: 5 * time.Second}
}

func TestTLSServerWithOptionalClientCert(t *testing.T) {
	t.Setenv("TOKI_DEVICECERT_MTLS", "optional")
	hub := newHubIdent(t)
	m, _ := newHubModule(t, hub)
	m.leaf = newLeafStore(t.TempDir())
	if err := m.selfIssue(context.Background()); err != nil {
		t.Fatal(err)
	}
	leaf, root, _ := m.leaf.current()
	if leaf == nil {
		t.Fatal("hub must hold its own leaf")
	}
	if !slices.Contains(leaf.DNSNames, hub.hub+NodeDNSSuffix) {
		t.Fatalf("hub name missing: %v", leaf.DNSNames)
	}

	url := startTLS(t, m, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		who := "anonymous"
		if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
			who = r.TLS.PeerCertificates[0].Subject.CommonName
		}
		io.WriteString(w, who)
	}))
	get := func(c *http.Client) (string, error) {
		res, err := c.Get(url)
		if err != nil {
			return "", err
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return string(b), nil
	}

	// the server certificate verifies against the root (loopback SAN) and a client without a cert is let in
	if who, err := get(clientFor(t, root, nil)); err != nil || who != "anonymous" {
		t.Fatalf("no client cert: %q %v", who, err)
	}
	// an unrelated root is refused by the client
	otherCA, _ := NewCA("hx", time.Now())
	if _, err := get(clientFor(t, otherCA.Cert, nil)); err == nil {
		t.Fatal("a client that does not trust the root must fail")
	}
	// a client cert from the CA is seen
	issue := func(ca *CA, name string) *tls.Certificate {
		k, spki := newKey(t)
		_, pemb, err := ca.Issue(time.Now(), LeafParams{Name: name, Kind: kernel.DeviceCertClient, SPKI: spki, Days: 30})
		if err != nil {
			t.Fatal(err)
		}
		b, _ := pem.Decode(pemb)
		return &tls.Certificate{Certificate: [][]byte{b.Bytes}, PrivateKey: k}
	}
	ca, _ := m.CA(false)
	good := issue(ca, "gate-ctrl-1")
	if who, err := get(clientFor(t, root, good)); err != nil || who != "gate-ctrl-1" {
		t.Fatalf("client cert: %q %v", who, err)
	}
	// a cert from another CA is refused during the handshake
	if _, err := get(clientFor(t, root, issue(otherCA, "evil"))); err == nil {
		t.Fatal("a client cert from another CA must be refused")
	}
	// a revoked serial is refused
	pc, _ := x509.ParseCertificate(good.Certificate[0])
	m.deny.mu.Lock()
	m.deny.set, m.deny.at = map[string]bool{SerialHex(pc.SerialNumber): true}, time.Now()
	m.deny.mu.Unlock()
	if _, err := get(clientFor(t, root, good)); err == nil {
		t.Fatal("a revoked client cert must be refused")
	}
}

func TestRequireClientCert(t *testing.T) {
	t.Setenv("TOKI_DEVICECERT_MTLS", "require")
	m, _ := newHubModule(t, newHubIdent(t))
	m.leaf = newLeafStore(t.TempDir())
	if err := m.selfIssue(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, root, _ := m.leaf.current()
	url := startTLS(t, m, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	if _, err := clientFor(t, root, nil).Get(url); err == nil {
		t.Fatal("require: a client without a cert must be refused")
	}
}

func TestLeafHotReload(t *testing.T) {
	m, _ := newHubModule(t, newHubIdent(t))
	m.leaf = newLeafStore(t.TempDir())
	if _, err := m.leaf.getCertificate(nil); err == nil {
		t.Fatal("no certificate yet must be an error, not a panic")
	}
	if err := m.selfIssue(context.Background()); err != nil {
		t.Fatal(err)
	}
	url := startTLS(t, m, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	leaf1, root, _ := m.leaf.current()
	c := clientFor(t, root, nil)
	serialOf := func() string {
		res, err := c.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		return SerialHex(res.TLS.PeerCertificates[0].SerialNumber)
	}
	if serialOf() != SerialHex(leaf1.SerialNumber) {
		t.Fatal("first leaf not served")
	}
	// a new SAN forces a renewal; the next handshake serves the new leaf with no restart
	m.lanIPs = func() []string { return []string{"127.0.0.1", "192.168.9.9"} }
	if err := m.selfIssue(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.CloseIdleConnections()
	leaf2, _, _ := m.leaf.current()
	if SerialHex(leaf2.SerialNumber) == SerialHex(leaf1.SerialNumber) {
		t.Fatal("expected a new leaf")
	}
	if serialOf() != SerialHex(leaf2.SerialNumber) {
		t.Fatal("the renewed leaf must be served without a restart")
	}
}

func TestListenerStartsAndStops(t *testing.T) {
	m, _ := newHubModule(t, newHubIdent(t))
	m.leaf = newLeafStore(t.TempDir())
	if err := m.selfIssue(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, root, _ := m.leaf.current()
	main := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "pong") })}
	if err := m.startListener("127.0.0.1:0", main); err != nil {
		t.Fatal(err)
	}
	on, addr := m.listenState()
	if !on || addr == "" {
		t.Fatal("listener must be on")
	}
	if h := m.Health(); !h.Listening || h.Addr != addr || h.Leaf == nil || h.Role != "hub" || h.CAFingerpr == "" {
		t.Fatalf("health %+v", h)
	}
	res, err := clientFor(t, root, nil).Get("https://" + addr)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if string(b) != "pong" {
		t.Fatalf("body %q", b)
	}
	m.Stop()
	if on, _ := m.listenState(); on {
		t.Fatal("listener must be off after Stop")
	}
	if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		c.Close()
		t.Fatal("the port must be closed after Stop")
	}
	// a busy port is an error, not a panic
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	if err := m.startListener(ln.Addr().String(), main); err == nil {
		t.Fatal("binding a busy port must fail")
	}
}

func TestEnvConfig(t *testing.T) {
	t.Setenv("TOKI_DEVICECERT", "")
	if Enabled() || ListenEnabled() {
		t.Fatal("off by default")
	}
	t.Setenv("TOKI_DEVICECERT", "on")
	if !Enabled() || ListenEnabled() {
		t.Fatal("on without a listen address")
	}
	t.Setenv("TOKI_DEVICECERT_LISTEN", ":8443")
	if !ListenEnabled() {
		t.Fatal("listen enabled")
	}
	for in, want := range map[string]int{"": 14, "30": 30, "90": 90, "500": 90, "-3": 14, "x": 14} {
		t.Setenv("TOKI_DEVICECERT_LEAF_DAYS", in)
		if got := LeafDays(); got != want {
			t.Fatalf("LEAF_DAYS=%q: %d want %d", in, got, want)
		}
	}
	for in, want := range map[string]MTLS{"": MTLSOff, "off": MTLSOff, "OPTIONAL": MTLSOptional, "require": MTLSRequire, "junk": MTLSOff} {
		t.Setenv("TOKI_DEVICECERT_MTLS", in)
		if MTLSMode() != want {
			t.Fatalf("MTLS=%q", in)
		}
	}
	t.Setenv("TOKI_DEVICECERT_SANS", " a.local, 10.0.0.2 ,,")
	if got := ExtraSANs(); !slices.Equal(got, []string{"a.local", "10.0.0.2"}) {
		t.Fatalf("sans %v", got)
	}
}

func TestMarkerLists(t *testing.T) {
	var found *kernel.ModuleMarker
	for _, mk := range kernel.ModuleMarkers() {
		if mk.Name == "devicecert" {
			mk := mk
			found = &mk
		}
	}
	if found == nil || found.Stubbed {
		t.Fatalf("marker: %+v", found)
	}
	if !slices.Contains(found.Envs, "TOKI_DEVICECERT") || !slices.Contains(found.Collections, "_device_certs") {
		t.Fatalf("marker lists: %+v", found)
	}
}
