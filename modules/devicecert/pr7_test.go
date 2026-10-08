//go:build !no_devicecert

package devicecert

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/internal/edgeguard"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tests"
)

func TestNormalizeScope(t *testing.T) {
	ok := map[string]string{
		"":                              "",
		"/api/scan, /api/print/":        "/api/print,/api/scan",
		"/api/scan,/api/scan":           "/api/scan",
		"/api/kiosk/status,/api/scan/x": "/api/kiosk/status,/api/scan/x",
	}
	for in, want := range ok {
		got, err := NormalizeScope(in)
		if err != nil || got != want {
			t.Errorf("%q: %q %v, want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"/", "/api", "api/scan", "/api/collections", "/api/collections/x/records", "/api/sync/push", "/api/../x/y",
		"/api/*", "/_/", "/api/scan x", "/api/health", "/api/device/attest", "/api/realtime"} {
		if got, err := NormalizeScope(bad); err == nil {
			t.Errorf("%q must be refused, got %q", bad, got)
		}
	}
	if !MatchScope("/api/scan,/api/print", "/api/scan/scanners") || !MatchScope("/api/scan", "/api/scan") {
		t.Fatal("prefix match")
	}
	for _, p := range []string{"/api/scanner", "/api/scan/../collections/x", "/api//scan", "/api/collections/x", ""} {
		if MatchScope("/api/scan", p) {
			t.Errorf("%q must not match", p)
		}
	}
}

// scopeEnv is a full router with the device middleware and two test routes.
type scopeEnv struct {
	m   *Module
	mux http.Handler
}

func newScopeEnv(t *testing.T) *scopeEnv {
	t.Helper()
	m, app := newHubModule(t, newHubIdent(t))
	m.leaf = newLeafStore(t.TempDir())
	if err := m.selfIssue(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.bindHTTP()
	app.OnServe().BindFunc(func(se *core.ServeEvent) error {
		h := func(e *core.RequestEvent) error {
			return e.JSON(200, map[string]any{"device": edgeguard.Device(e), "hdr": e.Request.Header.Get(HeaderDevice)})
		}
		se.Router.GET("/api/scan/scanners", h).Bind(edgeguard.RequireAuthOrDevice())
		se.Router.GET("/api/other/thing", h).Bind(edgeguard.RequireAuthOrDevice())
		se.Router.GET("/api/open/echo", h)
		return se.Next()
	})
	router, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	var mux http.Handler
	if err := app.OnServe().Trigger(&core.ServeEvent{App: app, Router: router}, func(se *core.ServeEvent) error {
		var err error
		mux, err = se.Router.BuildMux()
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return &scopeEnv{m: m, mux: mux}
}

// get calls path as if cert had been verified by the TLS layer.
func (e *scopeEnv) get(t *testing.T, path string, cert *x509.Certificate, hdr string) (int, string) {
	t.Helper()
	r := httptest.NewRequest("GET", path, nil)
	r.RemoteAddr = "127.0.0.1:5000"
	if hdr != "" {
		r.Header.Set(HeaderDevice, hdr)
	}
	if cert != nil {
		r.TLS = &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{cert}}}
	}
	w := httptest.NewRecorder()
	e.mux.ServeHTTP(w, r)
	return w.Code, w.Body.String()
}

func (e *scopeEnv) issue(t *testing.T, name, scope string, kind kernel.DeviceCertKind) (*kernel.DeviceCert, *x509.Certificate) {
	t.Helper()
	c, err := e.m.Issue(context.Background(), kernel.DeviceCertRequest{Name: name, Kind: kind, RouteScope: scope, Days: 30})
	if err != nil {
		t.Fatal(err)
	}
	cert, err := ParseCertPEM(c.CertPEM)
	if err != nil {
		t.Fatal(err)
	}
	return c, cert
}

func TestRouteScopeEnforced(t *testing.T) {
	e := newScopeEnv(t)
	c, cert := e.issue(t, "gate-ctrl-1", "/api/scan", kernel.DeviceCertClient)

	if st, body := e.get(t, "/api/scan/scanners", cert, ""); st != 200 || !strings.Contains(body, `"device":"gate-ctrl-1"`) || !strings.Contains(body, `"hdr":"gate-ctrl-1"`) {
		t.Fatalf("inside the scope, no token: %d %s", st, body)
	}
	if st, _ := e.get(t, "/api/other/thing", cert, ""); st != 401 {
		t.Fatalf("outside the scope: %d", st)
	}
	if st, _ := e.get(t, "/api/scan/scanners", nil, ""); st != 401 {
		t.Fatalf("no cert: %d", st)
	}
	// an inbound header is never trusted, with or without a certificate
	if st, body := e.get(t, "/api/open/echo", nil, "gate-ctrl-1"); st != 200 || strings.Contains(body, "gate-ctrl-1") {
		t.Fatalf("a forged X-Toki-Device must be stripped: %d %s", st, body)
	}
	if st, _ := e.get(t, "/api/scan/scanners", nil, "gate-ctrl-1"); st != 401 {
		t.Fatalf("a forged header must not authenticate: %d", st)
	}
	// the header is set only inside the scope
	if _, body := e.get(t, "/api/open/echo", cert, ""); strings.Contains(body, "gate-ctrl-1") {
		t.Fatalf("outside the scope the header must be absent: %s", body)
	}

	// no scope: the certificate proves identity and grants nothing
	_, noScope := e.issue(t, "gate-ctrl-2", "", kernel.DeviceCertClient)
	if st, _ := e.get(t, "/api/scan/scanners", noScope, ""); st != 401 {
		t.Fatalf("a certificate without scope: %d", st)
	}
	// a server leaf (even presented as verified) never acts as a client
	_, srv := e.issue(t, "n123", "", kernel.DeviceCertServer)
	if st, _ := e.get(t, "/api/scan/scanners", srv, ""); st != 401 {
		t.Fatalf("server leaf: %d", st)
	}
	// a row that says kind=server with a scope never grants either
	rec, _ := e.m.find(c.Serial)
	rec.Set("kind", "server")
	if err := e.m.app.Save(rec); err != nil {
		t.Fatal(err)
	}
	e.m.resetInfos()
	if st, _ := e.get(t, "/api/scan/scanners", cert, ""); st != 401 {
		t.Fatalf("kind server row: %d", st)
	}
	rec.Set("kind", "client")
	_ = e.m.app.Save(rec)
	e.m.resetInfos()
	if st, _ := e.get(t, "/api/scan/scanners", cert, ""); st != 200 {
		t.Fatalf("restored: %d", st)
	}
	// revoked: nothing, even if the TLS layer let it through
	if err := e.m.Revoke(context.Background(), "gate-ctrl-1"); err != nil {
		t.Fatal(err)
	}
	if st, _ := e.get(t, "/api/scan/scanners", cert, ""); st != 401 {
		t.Fatalf("revoked: %d", st)
	}
}

func TestIssueClientCertValidation(t *testing.T) {
	m, _ := newHubModule(t, newHubIdent(t))
	if _, err := m.Issue(context.Background(), kernel.DeviceCertRequest{Name: "x", Kind: kernel.DeviceCertClient, RouteScope: "/api/collections"}); err == nil {
		t.Fatal("a scope on /api/collections must be refused")
	}
	c, err := m.Issue(context.Background(), kernel.DeviceCertRequest{Name: "x", Kind: kernel.DeviceCertClient, RouteScope: "/api/print,/api/scan", Days: 500})
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := ParseCertPEM(c.CertPEM)
	if !slices.Equal(cert.ExtKeyUsage, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}) || len(cert.DNSNames) != 0 {
		t.Fatalf("client cert usage %v", cert.ExtKeyUsage)
	}
	if d := cert.NotAfter.Sub(cert.NotBefore); d > 366*24*time.Hour+time.Hour {
		t.Fatalf("client cert lifetime %v", d)
	}
	if c.RouteScope != "/api/print,/api/scan" || len(c.KeyPEM) == 0 {
		t.Fatalf("%+v", c)
	}
	if ci := m.info(c.Serial); !ci.found || ci.kind != kernel.DeviceCertClient || ci.scope != "/api/print,/api/scan" {
		t.Fatalf("row %+v", ci)
	}
}

func tlsClientCert(t *testing.T, c *kernel.DeviceCert) *tls.Certificate {
	t.Helper()
	cert, err := tls.X509KeyPair(c.CertPEM, c.KeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return &cert
}

// A revoked client certificate is refused at the TLS layer, also when the
// client resumes an earlier session (VerifyPeerCertificate would be skipped).
func TestRevokedRefusedAtTLSAndOnResume(t *testing.T) {
	t.Setenv("TOKI_DEVICECERT_MTLS", "optional")
	m, _ := newHubModule(t, newHubIdent(t))
	m.leaf = newLeafStore(t.TempDir())
	if err := m.selfIssue(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, root, _ := m.leaf.current()
	url := startTLS(t, m, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	c, err := m.Issue(context.Background(), kernel.DeviceCertRequest{Name: "gate-ctrl-9", Kind: kernel.DeviceCertClient, RouteScope: "/api/scan", Days: 30})
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(root)
	cc := tlsClientCert(t, c)
	cfg := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12, ClientSessionCache: tls.NewLRUClientSessionCache(8),
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return cc, nil }}
	cl := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg, DisableKeepAlives: true}, Timeout: 5 * time.Second}
	do := func() (bool, error) {
		res, err := cl.Get(url)
		if err != nil {
			return false, err
		}
		defer res.Body.Close()
		_, _ = io.ReadAll(res.Body)
		return res.TLS.DidResume, nil
	}
	if _, err := do(); err != nil {
		t.Fatal(err)
	}
	resumed, err := do()
	if err != nil || !resumed {
		t.Fatalf("the second connection must resume (it makes this test meaningful): resumed=%v err=%v", resumed, err)
	}
	if err := m.Revoke(context.Background(), c.Serial); err != nil {
		t.Fatal(err)
	}
	if _, err := do(); err == nil {
		t.Fatal("a revoked certificate must be refused on a resumed session")
	}
	// fresh session cache: refused too
	cfg.ClientSessionCache = tls.NewLRUClientSessionCache(8)
	if _, err := do(); err == nil {
		t.Fatal("a revoked certificate must be refused on a full handshake")
	}
}

// The edge server leaf is not a client certificate.
func TestServerLeafAsClientRefused(t *testing.T) {
	t.Setenv("TOKI_DEVICECERT_MTLS", "optional")
	m, _ := newHubModule(t, newHubIdent(t))
	m.leaf = newLeafStore(t.TempDir())
	if err := m.selfIssue(context.Background()); err != nil {
		t.Fatal(err)
	}
	leafCert, root, _ := m.leaf.current()
	url := startTLS(t, m, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	m.leaf.mu.RLock()
	as := m.leaf.cert
	m.leaf.mu.RUnlock()
	if leafCert == nil || as == nil {
		t.Fatal("no leaf")
	}
	if _, err := clientFor(t, root, as).Get(url); err == nil {
		t.Fatal("a server leaf presented as a client certificate must be refused")
	}
	// the verifyConn guard on its own (a chain that the pool accepted)
	err := m.verifyConn(tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{leafCert}}})
	if err != ErrNotClientCert {
		t.Fatalf("verifyConn: %v", err)
	}
}

func TestRotateCAOverlap(t *testing.T) {
	m, _ := newHubModule(t, newHubIdent(t))
	ctx := context.Background()
	oldC, err := m.Issue(ctx, kernel.DeviceCertRequest{Name: "old-peer", Kind: kernel.DeviceCertClient, Days: 30})
	if err != nil {
		t.Fatal(err)
	}
	oldCA, _ := m.CA(false)
	newCA, retire, err := m.RotateCA(30)
	if err != nil {
		t.Fatal(err)
	}
	if newCA.Fingerprint() == oldCA.Fingerprint() {
		t.Fatal("a new root is expected")
	}
	if d := retire.Sub(m.now()); d < 29*24*time.Hour || d > 31*24*time.Hour {
		t.Fatalf("overlap %v", d)
	}
	cur, _ := m.CA(false)
	if cur.Fingerprint() != newCA.Fingerprint() {
		t.Fatal("the newest CA must sign")
	}
	// the new leaf is signed by the new root; the bundle carries both, old one with a retire time
	nc, err := m.Issue(ctx, kernel.DeviceCertRequest{Name: "new-peer", Kind: kernel.DeviceCertClient, Days: 30})
	if err != nil {
		t.Fatal(err)
	}
	nl, _ := ParseCertPEM(nc.CertPEM)
	if nl.CheckSignatureFrom(newCA.Cert) != nil {
		t.Fatal("signed by the new CA")
	}
	roots, err := ParseBundle(nc.CAPEM)
	if err != nil || len(roots) != 2 || !roots[0].RetireAt.IsZero() || roots[1].RetireAt.IsZero() {
		t.Fatalf("bundle %v %v", roots, err)
	}
	// both are in the pool during the overlap: the old-signed leaf verifies
	ol, _ := ParseCertPEM(oldC.CertPEM)
	pool := m.clientPool()
	opts := func(at time.Time) x509.VerifyOptions {
		return x509.VerifyOptions{Roots: pool, CurrentTime: at, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	}
	if _, err := ol.Verify(opts(m.now())); err != nil {
		t.Fatalf("old-signed leaf during the overlap: %v", err)
	}
	if _, err := nl.Verify(opts(m.now())); err != nil {
		t.Fatalf("new-signed leaf: %v", err)
	}
	// after the overlap the old root leaves the pool
	later := retire.Add(time.Hour)
	m.now = func() time.Time { return later }
	m.caMu.Lock()
	m.cas = nil
	m.caMu.Unlock()
	pool = m.clientPool()
	if _, err := ol.Verify(opts(later)); err == nil {
		t.Fatal("old-signed leaf after the overlap must be refused")
	}
	if cs, _ := m.loadCAs(false); len(cs.roots(later)) != 1 {
		t.Fatal("one root after the overlap")
	}
	// the hub leaf is re-signed by the new CA
	m.leaf = newLeafStore(t.TempDir())
	if err := m.selfIssue(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestRotateCAReissuesHubLeafAndNodePinning(t *testing.T) {
	m, _ := newHubModule(t, newHubIdent(t))
	m.leaf = newLeafStore(t.TempDir())
	ctx := context.Background()
	if err := m.selfIssue(ctx); err != nil {
		t.Fatal(err)
	}
	first, _, _ := m.leaf.current()
	if _, _, err := m.RotateCA(30); err != nil {
		t.Fatal(err)
	}
	m.caMu.Lock()
	m.cas = nil
	m.caMu.Unlock()
	if err := m.selfIssue(ctx); err != nil {
		t.Fatal(err)
	}
	second, root, _ := m.leaf.current()
	cs, _ := m.loadCAs(false)
	if second.Equal(first) || second.CheckSignatureFrom(cs.cur.Cert) != nil || !root.Equal(cs.cur.Cert) {
		t.Fatal("the hub leaf must be re-signed by the rotated CA")
	}
	if m.leaf.rootCount() != 2 {
		t.Fatalf("roots %d", m.leaf.rootCount())
	}

	// a node pins the first root it received: a bundle of another CA is refused
	nodeStore := newLeafStore(t.TempDir())
	spki, _ := nodeStore.spki()
	c1, err := m.Issue(ctx, kernel.DeviceCertRequest{Name: "n1", Kind: kernel.DeviceCertServer, Node: "n1", SPKI: spki, SANs: []string{"127.0.0.1"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := nodeStore.install(c1.CertPEM, c1.CAPEM, true, true); err != nil {
		t.Fatal(err)
	}
	evil, _ := NewCA("evil", time.Now())
	_, evilLeaf, err := evil.Issue(time.Now(), LeafParams{Name: "n1", Kind: kernel.DeviceCertServer, SPKI: spki, DNS: []string{"n1.edge.toki.local"}, Days: 14})
	if err != nil {
		t.Fatal(err)
	}
	if err := nodeStore.install(evilLeaf, evil.PEM, true, true); err == nil || !strings.Contains(err.Error(), "pins") {
		t.Fatalf("a foreign root must be refused: %v", err)
	}
	// an expired leaf is refused as well
	past := time.Now().Add(-40 * 24 * time.Hour)
	oldCA, _ := m.CA(false)
	_, expired, _ := oldCA.Issue(past, LeafParams{Name: "n1", Kind: kernel.DeviceCertServer, SPKI: spki, DNS: []string{"n1.edge.toki.local"}, Days: 14})
	if err := nodeStore.install(expired, c1.CAPEM, true, true); err == nil || !strings.Contains(err.Error(), "not valid now") {
		t.Fatalf("an expired leaf must be refused: %v", err)
	}
	// the bundle file reloads atomically as one unit
	reload := newLeafStore(nodeStore.dir)
	if err := reload.load(); err != nil {
		t.Fatal(err)
	}
	if l, _, _ := reload.current(); l == nil || reload.rootCount() != 2 {
		t.Fatal("reload from the bundle file")
	}
}

func TestDenyListRefreshAndFailClosed(t *testing.T) {
	m, _ := newHubModule(t, newHubIdent(t))
	ctx := context.Background()
	c, err := m.Issue(ctx, kernel.DeviceCertRequest{Name: "p1", Kind: kernel.DeviceCertClient, Days: 30})
	if err != nil {
		t.Fatal(err)
	}
	if m.deny.size() != 0 || m.deny.has(c.Serial) {
		t.Fatal("empty deny list")
	}
	// another process revokes (row written without going through this module)
	rec, _ := m.find(c.Serial)
	rec.Set("revoked_at", "2026-10-01 00:00:00.000Z")
	if err := m.app.Save(rec); err != nil {
		t.Fatal(err)
	}
	if m.deny.has(c.Serial) {
		t.Fatal("the cache is still fresh")
	}
	if n := m.deny.refresh(); n != 1 || !m.deny.has(c.Serial) {
		t.Fatalf("refresh: %d", n)
	}
	if h := m.Health(); h.CAs != 1 {
		t.Fatalf("health cas %d", h.CAs)
	}
	// a failing reload keeps the last set during the grace period, then fails closed
	fail := func() (map[string]bool, error) { return nil, os.ErrClosed }
	m.deny.load = fail
	m.deny.at = m.now().Add(-time.Minute)
	if m.deny.has("somebody-else") {
		t.Fatal("inside the grace period the last good set is used")
	}
	m.deny.at = m.now().Add(-time.Hour)
	if !m.deny.has("somebody-else") {
		t.Fatal("past the grace period every certificate is refused")
	}
}

func TestRevokeByNameRevokesAllAndPrune(t *testing.T) {
	m, _ := newHubModule(t, newHubIdent(t))
	ctx := context.Background()
	a, _ := m.Issue(ctx, kernel.DeviceCertRequest{Name: "nodeX", Kind: kernel.DeviceCertServer, Node: "nodex", SANs: []string{"127.0.0.1"}})
	b, _ := m.Issue(ctx, kernel.DeviceCertRequest{Name: "nodeX", Kind: kernel.DeviceCertServer, Node: "nodex", SANs: []string{"127.0.0.2"}})
	if err := m.Revoke(ctx, "nodeX"); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{a.Serial, b.Serial} {
		if got, _ := m.Lookup(ctx, s); got.RevokedAt.IsZero() {
			t.Fatalf("%s must be revoked", s)
		}
	}
	// prune drops rows that expired long ago
	rec, _ := m.find(a.Serial)
	rec.Set("not_after", m.now().Add(-60*24*time.Hour).UTC().Format("2006-01-02 15:04:05.000Z"))
	if err := m.app.Save(rec); err != nil {
		t.Fatal(err)
	}
	if n := m.prune(); n != 1 {
		t.Fatalf("pruned %d", n)
	}
	if _, err := m.Lookup(ctx, a.Serial); err != kernel.ErrDeviceCertNotFound {
		t.Fatalf("pruned row: %v", err)
	}
	if err := m.Revoke(ctx, "never-issued"); err != kernel.ErrDeviceCertNotFound {
		t.Fatalf("unknown: %v", err)
	}
}

func TestDeviceIdentityAndAttest(t *testing.T) {
	e := newScopeEnv(t)
	post := func(body string) (int, string) {
		r := httptest.NewRequest("POST", "/api/device/attest", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.RemoteAddr = "10.0.0.5:1"
		w := httptest.NewRecorder()
		e.mux.ServeHTTP(w, r)
		return w.Code, w.Body.String()
	}
	r := httptest.NewRequest("GET", "/api/device/identity", nil)
	r.RemoteAddr = "10.0.0.5:1"
	w := httptest.NewRecorder()
	e.mux.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"node_id":"hhub000000000001"`) || !strings.Contains(w.Body.String(), `"leaf_fp":"`) {
		t.Fatalf("identity %d %s", w.Code, w.Body.String())
	}
	if st, _ := post(`{"nonce":"short"}`); st != 400 {
		t.Fatalf("short nonce: %d", st)
	}
	now := e.m.now().Unix()
	if st, _ := post(`{"nonce":"0123456789abcdef","ts":` + itoa(now-3600) + `}`); st != 400 {
		t.Fatalf("stale ts: %d", st)
	}
	if st, _ := post(`{"nonce":"0123456789abcdef","ts":` + itoa(now+3600) + `}`); st != 400 {
		t.Fatalf("future ts: %d", st)
	}
	st, body := post(`{"nonce":"0123456789abcdef","ts":` + itoa(now) + `}`)
	if st != 200 || !strings.Contains(body, `"alg":"Ed25519"`) {
		t.Fatalf("attest %d %s", st, body)
	}
	// the signature verifies with the node key over the documented message
	ni := kernel.NodeIdentityOf(e.m.app)
	msg := AttestMessage(ni.NodeID(), "0123456789abcdef", now)
	sig, _ := ni.Sign(msg)
	if !ed25519.Verify(ni.HubPub(), msg, sig) {
		t.Fatal("sanity")
	}
	if !strings.Contains(body, base64Std(sig)) {
		t.Fatalf("response signature differs: %s", body)
	}
	// rate limit
	limited := false
	for i := 0; i < 40; i++ {
		if st, _ := post(`{"nonce":"0123456789abcdef"}`); st == 429 {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("attest must be rate limited")
	}
}

func TestCLIDoesNotCreateTablesWhenOff(t *testing.T) {
	t.Setenv("TOKI_DEVICECERT", "")
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)
	if _, err := cliModule(app); err == nil {
		t.Fatal("cliModule must refuse while the module is off")
	}
	if app.HasTable(StateTable) {
		t.Fatal("no table while off")
	}
}

func TestIssueCLIWritesFiles(t *testing.T) {
	t.Setenv("TOKI_DEVICECERT", "on")
	m, app := newHubModule(t, newHubIdent(t))
	_ = m
	dir := t.TempDir()
	cmd := NewCommand(app)
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"issue", "--name", "gate-ctrl-1", "--days", "90", "--scope", "/api/scan,/api/print", "--out", dir})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	kb, err := os.ReadFile(filepath.Join(dir, "gate-ctrl-1.key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(filepath.Join(dir, "gate-ctrl-1.key.pem")); st.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %v", st.Mode())
	}
	cb, _ := os.ReadFile(filepath.Join(dir, "gate-ctrl-1.crt.pem"))
	if _, err := tls.X509KeyPair(cb, kb); err != nil {
		t.Fatal(err)
	}
	if b, _ := pem.Decode(kb); b == nil {
		t.Fatal("key pem")
	}
	rows, _ := app.FindAllRecords(CertsCollection)
	if len(rows) != 1 || rows[0].GetString("route_scope") != "/api/print,/api/scan" || rows[0].GetString("kind") != "client" {
		t.Fatalf("rows %v", rows)
	}
	cmd2 := NewCommand(app)
	cmd2.SetOut(&out)
	cmd2.SetArgs([]string{"revoke", "gate-ctrl-1"})
	if err := cmd2.Execute(); err != nil {
		t.Fatal(err)
	}
	if rows, _ := app.FindAllRecords(CertsCollection); rows[0].GetDateTime("revoked_at").IsZero() {
		t.Fatal("revoked")
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func base64Std(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
