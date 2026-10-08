//go:build !no_devicecert

package devicecert

import (
	"context"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
)

// Every spelling that snake-cases to x_toki_device must be stripped: rules
// read @request.headers.x_toki_device.
func TestForgedDeviceHeaderSpellingsAreStripped(t *testing.T) {
	e := newScopeEnv(t)
	for _, name := range []string{"X-Toki-Device", "X_Toki_Device", "x_toki_device", "x.toki.device", "X~Toki~Device", "X-TOKI-DEVICE"} {
		r := httptest.NewRequest("GET", "/api/open/echo", nil)
		r.RemoteAddr = "127.0.0.1:5000"
		r.Header[name] = []string{"gate-ctrl-1"}
		w := httptest.NewRecorder()
		e.mux.ServeHTTP(w, r)
		if w.Code != 200 || strings.Contains(w.Body.String(), "gate-ctrl-1") {
			t.Fatalf("%q reached the handler or the rules: %d %s", name, w.Code, w.Body.String())
		}
		r = httptest.NewRequest("GET", "/api/scan/scanners", nil)
		r.RemoteAddr = "127.0.0.1:5000"
		r.Header[name] = []string{"gate-ctrl-1"}
		w = httptest.NewRecorder()
		e.mux.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatalf("%q must not authenticate: %d", name, w.Code)
		}
	}
}

func TestIdentityHidesCertFromAnonymous(t *testing.T) {
	e := newScopeEnv(t)
	get := func(token string) string {
		r := httptest.NewRequest("GET", "/api/device/identity", nil)
		r.RemoteAddr = "10.0.0.9:1"
		if token != "" {
			r.Header.Set("Authorization", token)
		}
		w := httptest.NewRecorder()
		e.mux.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("identity %d %s", w.Code, w.Body.String())
		}
		return w.Body.String()
	}
	if body := get(""); strings.Contains(body, `"cert"`) || !strings.Contains(body, `"node_id"`) {
		t.Fatalf("anonymous identity must not carry the cert: %s", body)
	}
	su, err := e.m.app.FindAuthRecordByEmail(core.CollectionNameSuperusers, "test@example.com")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := su.NewAuthToken()
	if err != nil {
		t.Fatal(err)
	}
	if body := get(tok); !strings.Contains(body, `"cert"`) {
		t.Fatalf("an authenticated caller gets the cert: %s", body)
	}
}

func TestDenyRefreshFailureKeepsGrace(t *testing.T) {
	m, _ := newHubModule(t, newHubIdent(t))
	if m.deny.size() != 0 {
		t.Fatal("empty")
	}
	m.deny.load = func() (map[string]bool, error) { return map[string]bool{"aa": true}, nil }
	if n := m.deny.refresh(); n != 1 {
		t.Fatalf("refresh %d", n)
	}
	m.deny.load = func() (map[string]bool, error) { return nil, os.ErrClosed }
	m.deny.refresh() // a failing background reload
	if m.deny.has("other") || !m.deny.has("aa") {
		t.Fatal("a failed refresh inside the grace must keep the last good set")
	}
	m.deny.at = m.now().Add(-time.Hour)
	m.deny.refresh()
	if !m.deny.has("other") {
		t.Fatal("past the grace the list fails closed")
	}
}

func TestPinRejectsAttackerRootAppended(t *testing.T) {
	m, _ := newHubModule(t, newHubIdent(t))
	ctx := context.Background()
	node := newLeafStore(t.TempDir())
	spki, _ := node.spki()
	c1, err := m.Issue(ctx, kernel.DeviceCertRequest{Name: "n1", Kind: kernel.DeviceCertServer, Node: "n1", SPKI: spki, SANs: []string{"127.0.0.1"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := node.install(c1.CertPEM, c1.CAPEM, true, true); err != nil {
		t.Fatal(err)
	}
	evil, _ := NewCA("evil", time.Now())
	_, evilLeaf, err := evil.Issue(time.Now(), LeafParams{Name: "n1", Kind: kernel.DeviceCertServer, SPKI: spki, DNS: []string{"n1.edge.toki.local"}, Days: 14})
	if err != nil {
		t.Fatal(err)
	}
	bundle := append(append([]byte{}, evil.PEM...), c1.CAPEM...) // [attacker, known]
	if err := node.install(evilLeaf, bundle, true, true); err == nil || !strings.Contains(err.Error(), "pins") {
		t.Fatalf("an attacker root next to the known root must be refused: %v", err)
	}
	// the known roots are all that is trusted
	if node.rootCount() != 1 {
		t.Fatalf("roots %d", node.rootCount())
	}
	// a genuine leaf with an extra root is refused as well ...
	if err := node.install(c1.CertPEM, bundle, true, true); err == nil {
		t.Fatal("a bundle adding a root needs an explicit rotation")
	}
	// ... unless the operator accepts the rotation; then the leaf may chain to the added root
	t.Setenv(EnvAcceptRotation, "on")
	if err := node.install(c1.CertPEM, bundle, true, true); err != nil {
		t.Fatalf("explicit rotation: %v", err)
	}
}

func TestIssueDaysAndOverlapBounds(t *testing.T) {
	m, _ := newHubModule(t, newHubIdent(t))
	if _, err := m.Issue(context.Background(), kernel.DeviceCertRequest{Name: "neg", Kind: kernel.DeviceCertClient, Days: -1}); err == nil {
		t.Fatal("negative days must be refused")
	}
	if _, _, err := m.RotateCA(MaxOverlapDays + 1); err == nil {
		t.Fatal("overlap above the bound must be refused")
	}
	if _, _, err := m.RotateCA(-1); err == nil {
		t.Fatal("negative overlap must be refused")
	}
	_, app := newHubModule(t, newHubIdent(t))
	cmd := NewCommand(app)
	cmd.SetArgs([]string{"issue", "--name", "x1", "--days", "0", "--out", t.TempDir()})
	t.Setenv("TOKI_DEVICECERT", "on")
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--days") {
		t.Fatalf("--days 0 must be an error: %v", err)
	}
}
