//go:build !no_sync

package sync

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	stdsync "sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/modules/sync/client"
	"github.com/tokibase/tokibase/modules/sync/hlc"
	"github.com/tokibase/tokibase/modules/sync/proto"
)

func nodeStatus(t *testing.T, h *hubEnv, ref string) (status string, revokedAt bool) {
	t.Helper()
	r, err := FindNode(h.app, ref)
	if err != nil {
		t.Fatalf("node %s: %v", ref, err)
	}
	return r.GetString("status"), !r.GetDateTime("revoked_at").IsZero()
}

// I1: a handshake never writes status back; a revoke that wins stays.
func TestRevokeNotUndoneByHandshake(t *testing.T) {
	h := newHub(t)
	s := newSpoke(t)
	h.join(t, s, h.enroll(t, "gate-1", nil))
	id := s.m.NodeID()

	// deterministic: the node was loaded as active, then revoked, then the handshake writes
	if _, err := RevokeNode(h.app, "gate-1", false); err != nil {
		t.Fatal(err)
	}
	touched, err := h.m.touchNode(id, time.Now(), 5, 1, "9.9", time.Now().UnixMilli(), time.Time{})
	if err != nil || touched {
		t.Fatalf("a revoked node must not be touched: %v %v", touched, err)
	}
	if st, ra := nodeStatus(t, h, id); st != NodeRevoked || !ra {
		t.Fatalf("revoked state lost: %s %v", st, ra)
	}

	// concurrent: handshakes hammer the hub while the operator revokes
	h.m.guards.handshakePerMin = 1 << 20
	s2 := newSpoke(t)
	h.join(t, s2, h.enroll(t, "gate-2", nil))
	c := s2.client(t, h)
	var stop atomic.Bool
	var wg stdsync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			_, _ = c.Handshake(context.Background())
		}
	}()
	time.Sleep(30 * time.Millisecond)
	if _, err := RevokeNode(h.app, "gate-2", false); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	stop.Store(true)
	wg.Wait()
	if st, ra := nodeStatus(t, h, s2.m.NodeID()); st != NodeRevoked || !ra {
		t.Fatalf("revoked state lost after racing handshakes: %s %v", st, ra)
	}
	if _, err := c.Handshake(context.Background()); !client.IsCode(err, proto.CodeNodeRevoked) {
		t.Fatalf("handshake after revoke: %v", err)
	}
}

// I1: enrollment renames the row id; a revoke still hits the right row.
func TestRevokeDuringEnrollRename(t *testing.T) {
	h := newHub(t)
	var audits []string
	SetAuditSink(func(action, _, _ string, _ map[string]any) { audits = append(audits, action) })
	t.Cleanup(func() { SetAuditSink(nil) })

	s := newSpoke(t)
	code := h.enroll(t, "gate-1", nil)
	pend, _ := FindNode(h.app, "gate-1")
	h.join(t, s, code) // the row id changes from pend.Id to the node id
	if pend.Id == s.m.NodeID() {
		t.Fatal("test needs a renamed row")
	}
	// revoke by the stale pending id: no row -> an error, no audit entry
	before := len(audits)
	if _, err := RevokeNode(h.app, pend.Id, false); err == nil {
		t.Fatal("revoking a stale id must fail, not claim success")
	}
	if len(audits) != before {
		t.Fatalf("no audit entry for a revoke that did nothing: %v", audits)
	}
	if st, _ := nodeStatus(t, h, s.m.NodeID()); st != NodeActive {
		t.Fatalf("status %s", st)
	}
	// by name the revoke finds the renamed row
	rec, err := RevokeNode(h.app, "gate-1", false)
	if err != nil || rec.Id != s.m.NodeID() {
		t.Fatalf("revoke by name: %v %v", rec, err)
	}
	if st, ra := nodeStatus(t, h, s.m.NodeID()); st != NodeRevoked || !ra {
		t.Fatalf("status %s", st)
	}
	// revoking twice is a no-op without a second audit entry
	n := len(audits)
	if _, err := RevokeNode(h.app, "gate-1", false); err != nil || len(audits) != n {
		t.Fatalf("second revoke: %v %v", err, audits)
	}

	// a pending node revoked before it enrolls cannot enroll afterwards
	code2 := h.enroll(t, "gate-2", nil)
	if _, err := RevokeNode(h.app, "gate-2", false); err != nil {
		t.Fatal(err)
	}
	s2 := newSpoke(t)
	if _, err := client.Join(context.Background(), s2.app, client.EnrollParams{HubURL: h.srv.URL, Code: code2, Identity: s2.m.Identity(), Insecure: true}); !client.IsCode(err, proto.CodeEnrollInvalid) {
		t.Fatalf("enroll of a revoked pending node: %v", err)
	}
}

func postEnroll(t *testing.T, h *hubEnv, code string, id *proto.Identity) (int, proto.ErrorBody) {
	t.Helper()
	body, _ := json.Marshal(proto.EnrollRequest{Code: code, Ed25519Pub: b64(id.Pub()), X25519Pub: b64(id.KX())})
	res, err := http.Post(h.srv.URL+proto.PathEnroll, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var eb proto.ErrorBody
	_ = json.NewDecoder(res.Body).Decode(&eb)
	return res.StatusCode, eb
}

// The docs say a code works once "even under concurrency".
func TestConcurrentEnrollSameCode(t *testing.T) {
	h := newHub(t)
	code := h.enroll(t, "gate-1", nil)
	const n = 8
	ids := make([]*proto.Identity, n)
	for i := range ids {
		ids[i], _ = proto.GenerateIdentity()
	}
	var ok, invalid atomic.Int32
	var wg stdsync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			switch st, eb := postEnroll(t, h, code, ids[i]); {
			case st == 200:
				ok.Add(1)
			case st == 400 && eb.Data["code"] == proto.CodeEnrollInvalid:
				invalid.Add(1)
			default:
				t.Errorf("unexpected answer %d %v", st, eb)
			}
		}(i)
	}
	wg.Wait()
	if ok.Load() != 1 || invalid.Load() != n-1 {
		t.Fatalf("ok=%d invalid=%d, want 1/%d", ok.Load(), invalid.Load(), n-1)
	}
}

// A key whose node id exists already cannot take a second pending row.
func TestEnrollKeyAlreadyEnrolled(t *testing.T) {
	h := newHub(t)
	s := newSpoke(t)
	h.join(t, s, h.enroll(t, "gate-1", nil))
	if _, err := s.client(t, h).Handshake(context.Background()); err != nil { // consumes the retry window
		t.Fatal(err)
	}
	code2 := h.enroll(t, "gate-2", nil)
	if st, eb := postEnroll(t, h, code2, s.m.Identity()); st != 400 || eb.Data["code"] != proto.CodeEnrollInvalid {
		t.Fatalf("same key, second pending row: %d %v", st, eb)
	}
	r, _ := FindNode(h.app, "gate-2")
	if r.GetString("status") != NodePending || r.GetString("enroll_hash") == "" {
		t.Fatal("the failed claim must roll back: the row stays pending with its code")
	}
	// and the code still works for a different key
	other := newSpoke(t)
	h.join(t, other, code2)
}

// I9: the node can not choose its profile.
func TestProfileNotOverwrittenByNode(t *testing.T) {
	h := newHub(t)
	s := newSpoke(t)
	h.join(t, s, h.enroll(t, "gate-1", nil)) // admin chose edge
	c := s.client(t, h, func(o *client.Options) { o.Profile = "cluster" })
	if _, err := c.Handshake(context.Background()); err != nil {
		t.Fatal(err)
	}
	node, _ := h.app.FindRecordById(NodesCollection, s.m.NodeID())
	if node.GetString("profile") != "edge" {
		t.Fatalf("profile %q", node.GetString("profile"))
	}
	// the enroll body can not choose it either
	s2 := newSpoke(t)
	code := h.enroll(t, "gate-2", nil)
	if _, err := client.Join(context.Background(), s2.app, client.EnrollParams{HubURL: h.srv.URL, Code: code, Identity: s2.m.Identity(), Profile: "cluster", Insecure: true}); err != nil {
		t.Fatal(err)
	}
	if r, _ := FindNode(h.app, "gate-2"); r.GetString("profile") != "edge" {
		t.Fatalf("profile %q", r.GetString("profile"))
	}
}

// ---- I2/I3: hub key file and derived session secret ----

func TestHubKeyFileMissingRefusesNewIdentity(t *testing.T) {
	app := newApp(t)
	e := setupWith(t, app, RoleHub) // identity in the database
	id := e.m.HubID()
	st := dbState{db: app.NonconcurrentDB()}

	// the session secret is never stored and is derived from the hub key
	if _, ok, _ := st.Get(keySessionSecret); ok {
		t.Fatal("session_secret must not be stored")
	}
	want, _ := deriveSessionSecret(e.m.hub.priv)
	if !bytes.Equal(e.m.hub.secret, want) {
		t.Fatal("session secret must be derived from the hub key")
	}

	// migration database -> file keeps the identity and removes the copy in data.db
	p := filepath.Join(t.TempDir(), "keys", "hub.key")
	t.Setenv(EnvHubKeyFile, p)
	if err := e.m.Init(); err != nil {
		t.Fatalf("migration to a key file: %v", err)
	}
	if e.m.HubID() != id {
		t.Fatal("the hub id must not change")
	}
	if _, ok, _ := st.Get(keyHubKey); ok {
		t.Fatal("hub_key must leave data.db when the key lives in a file")
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatal(err)
	}

	// the file disappears (volume not mounted): refuse, do not mint a new identity
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	err := e.m.Init()
	if err == nil || !strings.Contains(err.Error(), "refusing to create a new one") {
		t.Fatalf("missing key file with an existing identity: %v", err)
	}
	if _, serr := os.Stat(p); serr == nil {
		t.Fatal("no key file may be created")
	}
	if e.m.HubID() != id {
		t.Fatal("the identity in memory must not change")
	}
	if v, _, _ := st.Get(keyHubID); v != id {
		t.Fatalf("hub_id in state changed: %s", v)
	}

	// file -> database switch without the key in the database: refuse
	t.Setenv(EnvHubKeyFile, "")
	err = e.m.Init()
	if err == nil || !strings.Contains(err.Error(), "no hub key in _sync_state") {
		t.Fatalf("switch to the database without a key: %v", err)
	}
	if _, ok, _ := st.Get(keyHubKey); ok {
		t.Fatal("no key may be created")
	}

	// a different key in the file: refuse
	other := filepath.Join(t.TempDir(), "other.key")
	_, k, _ := ed25519.GenerateKey(nil)
	if err := os.WriteFile(other, []byte(base64.StdEncoding.EncodeToString(k)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvHubKeyFile, other)
	err = e.m.Init()
	if err == nil || !strings.Contains(err.Error(), "does not match the stored hub_id") {
		t.Fatalf("foreign key: %v", err)
	}
}

func TestHubKeyFileFreshCreatedAndModeWarning(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hub.key")
	t.Setenv(EnvHubKeyFile, p)
	e := setupWith(t, newApp(t), RoleHub)
	st := dbState{db: e.app.NonconcurrentDB()}
	for _, k := range []string{keyHubKey, keySessionSecret} {
		if _, ok, _ := st.Get(k); ok {
			t.Fatalf("%s must not be in data.db with a key file", k)
		}
	}
	if runtime.GOOS == "windows" {
		return
	}
	var warned []string
	warn := func(msg string, args ...any) { warned = append(warned, msg) }
	if _, err := loadHubIdentity(st, warn); err != nil || len(warned) != 0 {
		t.Fatalf("0600 file: %v %v", err, warned)
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadHubIdentity(st, warn); err != nil || len(warned) != 1 {
		t.Fatalf("a group/other readable key file must warn: %v %v", err, warned)
	}
}

// ---- I5: hub and host binding ----

func TestHandshakeBoundToHubAndHost(t *testing.T) {
	h := newHub(t)
	s := newSpoke(t)
	res := h.join(t, s, h.enroll(t, "gate-1", nil))
	r := rawHS{h: h, s: s, cert: res.Cert}
	now := time.Now()
	u, _ := url.Parse(h.srv.URL)
	mk := func(host, hub string) func(http.Header, *[]byte) {
		return func(hd http.Header, b *[]byte) {
			hd.Set(proto.HeaderSig, proto.SignRequest(s.m.Identity().Ed, "POST", proto.PathHandshake, host, hub, hd.Get(proto.HeaderSigTs), hd.Get(proto.HeaderNonce), *b))
		}
	}
	// signed for another hub (cross-hub replay): refused
	if st, _ := r.send(t, mk(u.Host, "hzzzzzzzzzzzzzz"), now, "nonce-hub-1"); st != 401 {
		t.Fatalf("other hub id: %d", st)
	}
	// signed for another host: refused
	if st, _ := r.send(t, mk("evil.example.com", h.m.HubID()), now, "nonce-host-1"); st != 401 {
		t.Fatalf("other host: %d", st)
	}
	// the legacy form without hub and host: refused
	if st, _ := r.send(t, mk("", ""), now, "nonce-leg-1"); st != 401 {
		t.Fatalf("unbound signature: %d", st)
	}
	// the right audience passes
	if st, eb := r.send(t, mk(u.Host, h.m.HubID()), now, "nonce-ok-12"); st != 200 {
		t.Fatalf("valid: %d %v", st, eb)
	}
}

func TestHandshakeAcceptsConfiguredAppURLHost(t *testing.T) {
	h := newHub(t)
	s := newSpoke(t)
	res := h.join(t, s, h.enroll(t, "gate-1", nil))
	h.app.Settings().Meta.AppURL = "https://sync.example.com"
	r := rawHS{h: h, s: s, cert: res.Cert}
	now := time.Now()
	sign := func(hd http.Header, b *[]byte) {
		hd.Set(proto.HeaderSig, proto.SignRequest(s.m.Identity().Ed, "POST", proto.PathHandshake, "SYNC.example.com:443", h.m.HubID(), hd.Get(proto.HeaderSigTs), hd.Get(proto.HeaderNonce), *b))
	}
	if st, eb := r.send(t, sign, now, "nonce-app-url"); st != 200 {
		t.Fatalf("a proxy that rewrites Host: %d %v", st, eb)
	}
}

// redirectTarget counts requests that reach it.
func redirectTarget(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { n.Add(1); w.WriteHeader(200) }))
	t.Cleanup(srv.Close)
	return srv, &n
}

func TestClientRefusesRedirects(t *testing.T) {
	target, hits := redirectTarget(t)
	for _, code := range []int{301, 302, 307, 308} {
		src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+r.URL.Path, code)
		}))
		id, _ := proto.GenerateIdentity()
		_, err := client.Enroll(context.Background(), client.EnrollParams{HubURL: src.URL, Code: "AAAA-BBBB", Identity: id, Insecure: true})
		if err == nil || !strings.Contains(err.Error(), "redirect") {
			t.Fatalf("%d enroll: %v", code, err)
		}
		hubPub, _, _ := ed25519.GenerateKey(nil)
		c, cerr := client.New(client.Options{Identity: id, HubURL: src.URL, Cert: "x", HubID: proto.HubID(hubPub), HubPub: b64(hubPub), Insecure: true})
		if cerr != nil {
			t.Fatal(cerr)
		}
		if _, err := c.Handshake(context.Background()); err == nil || !strings.Contains(err.Error(), "redirect") {
			t.Fatalf("%d handshake: %v", code, err)
		}
		// a caller supplied http.Client is not allowed to follow redirects either
		c2, _ := client.New(client.Options{Identity: id, HubURL: src.URL, Cert: "x", HubID: proto.HubID(hubPub), HubPub: b64(hubPub), Insecure: true, HTTP: &http.Client{}})
		if _, err := c2.Handshake(context.Background()); err == nil || !strings.Contains(err.Error(), "redirect") {
			t.Fatalf("%d custom client: %v", code, err)
		}
		src.Close()
	}
	if hits.Load() != 0 {
		t.Fatalf("the redirect target received %d requests (code or signed request leaked)", hits.Load())
	}
}

// ---- I4: signed clock correction ----

// fakeHub answers handshakes like a hub (or an attacker) and records them.
type fakeHub struct {
	srv  *httptest.Server
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
	n    atomic.Int32
}

// newFakeHub: handler(n, w, r, signFn) answers request number n (1 based).
func newFakeHub(t *testing.T, handler func(n int, w http.ResponseWriter, r *http.Request, sign func(st string))) *fakeHub {
	t.Helper()
	f := &fakeHub{}
	f.pub, f.priv, _ = ed25519.GenerateKey(nil)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(f.n.Add(1))
		sign := func(st string) {
			w.Header().Set(proto.HeaderServerTime, st)
			w.Header().Set(proto.HeaderServerSig, proto.SignServerTime(f.priv, r.Header.Get(proto.HeaderNode), r.Header.Get(proto.HeaderSigTs), r.Header.Get(proto.HeaderNonce), st))
		}
		handler(n, w, r, sign)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeHub) client(t *testing.T, id *proto.Identity, o func(*client.Options)) *client.Client {
	t.Helper()
	opt := client.Options{Identity: id, HubURL: f.srv.URL, Cert: "cert", HubID: proto.HubID(f.pub), HubPub: b64(f.pub), Insecure: true}
	if o != nil {
		o(&opt)
	}
	c, err := client.New(opt)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func answer401(w http.ResponseWriter, st string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(401)
	_ = json.NewEncoder(w).Encode(proto.ErrorBody{Status: 401, Message: "no", Data: map[string]any{"code": proto.CodeUnauthorized, "server_time": st}})
}

func TestClockCorrectionNeedsSignedHubTime(t *testing.T) {
	id, _ := proto.GenerateIdentity()
	far := time.Now().Add(3 * 365 * 24 * time.Hour).UTC().Format(proto.TimeLayout)
	soon := time.Now().Add(2 * time.Hour).UTC().Format(proto.TimeLayout)
	huge := time.Now().Add(30 * 24 * time.Hour).UTC().Format(proto.TimeLayout)
	other, otherPriv, _ := ed25519.GenerateKey(nil)
	_ = other

	cases := map[string]func(n int, w http.ResponseWriter, r *http.Request, sign func(string)){
		"forged 401 without signature": func(n int, w http.ResponseWriter, r *http.Request, sign func(string)) { answer401(w, far) },
		"401 signed by another key": func(n int, w http.ResponseWriter, r *http.Request, sign func(string)) {
			w.Header().Set(proto.HeaderServerTime, far)
			w.Header().Set(proto.HeaderServerSig, proto.SignServerTime(otherPriv, r.Header.Get(proto.HeaderNode), r.Header.Get(proto.HeaderSigTs), r.Header.Get(proto.HeaderNonce), far))
			answer401(w, far)
		},
		"signature for another request": func(n int, w http.ResponseWriter, r *http.Request, sign func(string)) {
			w.Header().Set(proto.HeaderServerTime, far)
			w.Header().Set(proto.HeaderServerSig, proto.SignServerTime(otherPriv, "nx", "1", "2", far))
			answer401(w, far)
		},
		"signed but absurd (30 days)": func(n int, w http.ResponseWriter, r *http.Request, sign func(string)) { sign(huge); answer401(w, huge) },
		"signed header, different body time": func(n int, w http.ResponseWriter, r *http.Request, sign func(string)) {
			sign(soon)
			answer401(w, far)
		},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			var clk = newTestClock()
			f := newFakeHub(t, h)
			c := f.client(t, id, func(o *client.Options) { o.Clock = clk })
			if _, err := c.Handshake(context.Background()); err == nil {
				t.Fatal("must fail")
			}
			if c.Offset() != 0 || clk.Offset() != 0 {
				t.Fatalf("offset applied from an unauthenticated answer: client %v clock %v", c.Offset(), clk.Offset())
			}
			if f.n.Load() != 1 {
				t.Fatalf("no retry without an authenticated correction, got %d requests", f.n.Load())
			}
		})
	}

	t.Run("signed correction, retry fails: offset reverted", func(t *testing.T) {
		clk := newTestClock()
		f := newFakeHub(t, func(n int, w http.ResponseWriter, r *http.Request, sign func(string)) {
			sign(soon)
			answer401(w, soon)
		})
		c := f.client(t, id, func(o *client.Options) { o.Clock = clk })
		if _, err := c.Handshake(context.Background()); err == nil {
			t.Fatal("must fail")
		}
		if f.n.Load() != 2 {
			t.Fatalf("one retry expected, got %d", f.n.Load())
		}
		if c.Offset() != 0 || clk.Offset() != 0 {
			t.Fatalf("the offset of a failed correction must be reverted: client %v clock %v", c.Offset(), clk.Offset())
		}
	})

	t.Run("200 that is not signed by the hub", func(t *testing.T) {
		f := newFakeHub(t, func(n int, w http.ResponseWriter, r *http.Request, sign func(string)) {
			_ = json.NewEncoder(w).Encode(proto.HandshakeResponse{SessionToken: "t", HubID: proto.HubID(otherPub(t)), ServerTime: far, Expires: far})
		})
		c := f.client(t, id, nil)
		if _, err := c.Handshake(context.Background()); err == nil || c.Offset() != 0 || c.Token() != "" {
			t.Fatalf("an unsigned answer must be refused: %v %v %q", err, c.Offset(), c.Token())
		}
	})

	t.Run("200 from another hub id", func(t *testing.T) {
		f := newFakeHub(t, func(n int, w http.ResponseWriter, r *http.Request, sign func(string)) {
			st := time.Now().UTC().Format(proto.TimeLayout)
			sign(st)
			_ = json.NewEncoder(w).Encode(proto.HandshakeResponse{SessionToken: "t", HubID: "hzzzzzzzzzzzzzz", ServerTime: st, Expires: st})
		})
		c := f.client(t, id, nil)
		if _, err := c.Handshake(context.Background()); err == nil || c.Token() != "" {
			t.Fatalf("hub id mismatch must be refused: %v", err)
		}
	})
}

func otherPub(t *testing.T) ed25519.PublicKey {
	p, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// ---- I6: throttle, header caps, audit only for known nodes ----

func TestHandshakeThrottleAndEnrollThrottle(t *testing.T) {
	h := newHub(t)
	h.m.guards.handshakePerMin = 5
	h.m.guards.enrollPerMin = 3
	s := newSpoke(t)
	res := h.join(t, s, h.enroll(t, "gate-1", nil)) // 1 enroll request
	r := rawHS{h: h, s: s, cert: res.Cert}
	got429 := false
	for i := 0; i < 10; i++ {
		st, eb := r.send(t, func(hd http.Header, _ *[]byte) { hd.Del(proto.HeaderSig) }, time.Now(), "nonce-thr-"+strconv.Itoa(i))
		if st == 429 {
			if eb.Data["code"] != proto.CodeRateLimited {
				t.Fatalf("429 body: %v", eb)
			}
			got429 = true
			if i < 5 {
				t.Fatalf("throttled too early at %d", i)
			}
		}
	}
	if !got429 {
		t.Fatal("handshakes must be throttled per IP without PocketBase rate limits")
	}
	other, _ := proto.GenerateIdentity()
	var last int
	for i := 0; i < 6; i++ {
		last, _ = postEnroll(t, h, "AAAA-BBBB-CCCC-DDDD", other)
	}
	if last != 429 {
		t.Fatalf("enroll must be throttled, last status %d", last)
	}
}

func TestHandshakeHeaderCapsAndAuditOnlyKnownNodes(t *testing.T) {
	h := newHub(t)
	var audits []map[string]any
	SetAuditSink(func(action, _, _ string, d map[string]any) {
		if action == AuditHandshakeFailed {
			audits = append(audits, d)
		}
	})
	t.Cleanup(func() { SetAuditSink(nil) })
	s := newSpoke(t)
	res := h.join(t, s, h.enroll(t, "gate-1", nil))
	r := rawHS{h: h, s: s, cert: res.Cert}
	now := time.Now()

	// over-long, malformed node header and nonce: 400, nothing audited
	for name, mut := range map[string]func(http.Header, *[]byte){
		"long node":     func(hd http.Header, _ *[]byte) { hd.Set(proto.HeaderNode, strings.Repeat("n", 65)) },
		"huge node":     func(hd http.Header, _ *[]byte) { hd.Set(proto.HeaderNode, strings.Repeat("n", 4000)) },
		"bad charset":   func(hd http.Header, _ *[]byte) { hd.Set(proto.HeaderNode, "nABCDEFGHIJKLMN") },
		"pipe in nonce": func(hd http.Header, _ *[]byte) { hd.Set(proto.HeaderNonce, "abc|def|ghijk") },
		"short nonce":   func(hd http.Header, _ *[]byte) { hd.Set(proto.HeaderNonce, "abc") },
		"long nonce":    func(hd http.Header, _ *[]byte) { hd.Set(proto.HeaderNonce, strings.Repeat("a", 65)) },
	} {
		if st, eb := r.send(t, mut, now, "nonce-cap-12"); st != 400 || eb.Data["code"] != proto.CodeBadRequest {
			t.Fatalf("%s: %d %v", name, st, eb)
		}
	}
	if len(audits) != 0 {
		t.Fatalf("malformed requests must not be audited: %v", audits)
	}

	// well formed but unknown node: 401, counted, not audited
	unknown := proto.NodeID(otherPub(t))
	before := h.m.UnknownHandshakes()
	for i := 0; i < 5; i++ {
		if st, _ := r.send(t, func(hd http.Header, b *[]byte) {
			hd.Set(proto.HeaderNode, unknown)
			*b = bytes.Replace(*b, []byte(s.m.NodeID()), []byte(unknown), 1)
		}, now, "nonce-unk-"+strconv.Itoa(i)+"-xx"); st != 401 {
			t.Fatalf("unknown node: %d", st)
		}
	}
	if len(audits) != 0 {
		t.Fatalf("unknown node ids must not be audited: %v", audits)
	}
	if got := h.m.UnknownHandshakes() - before; got != 5 {
		t.Fatalf("counter %d", got)
	}

	// a KNOWN node with a bad signature is audited
	if st, _ := r.send(t, func(hd http.Header, _ *[]byte) { hd.Set(proto.HeaderSig, "AAAA") }, now, "nonce-bad-sig1"); st != 401 {
		t.Fatalf("bad signature: %d", st)
	}
	if len(audits) != 1 || audits[0]["node"] != s.m.NodeID() || audits[0]["reason"] != "bad_signature" {
		t.Fatalf("audit: %v", audits)
	}
}

// ---- I7: replay after a restart ----

func TestReplayRefusedAfterRestartByPersistedFloor(t *testing.T) {
	h := newHub(t)
	s := newSpoke(t)
	res := h.join(t, s, h.enroll(t, "gate-1", nil))
	r := rawHS{h: h, s: s, cert: res.Cert}
	now := time.Now()
	if st, eb := r.send(t, nil, now, "nonce-first-1"); st != 200 {
		t.Fatalf("first: %d %v", st, eb)
	}
	// restart: the in-memory nonce cache is empty
	h.m.nonces.mu.Lock()
	h.m.nonces.nodes = nil
	h.m.nonces.mu.Unlock()
	if st, _ := r.send(t, nil, now, "nonce-first-1"); st != 401 {
		t.Fatalf("a captured request must not replay after a restart: %d", st)
	}
	// an older request (never seen) is refused too: ts must be above the floor
	if st, _ := r.send(t, nil, now.Add(-time.Second), "nonce-older-12"); st != 401 {
		t.Fatalf("older ts: %d", st)
	}
	// a newer one passes
	if st, _ := r.send(t, nil, now.Add(time.Second), "nonce-newer-12"); st != 200 {
		t.Fatalf("newer ts: %d", st)
	}
}

// ---- I14: certificate renewal ----

func TestCertRenewedWhenNearExpiry(t *testing.T) {
	h := newHub(t)
	s := newSpoke(t)
	res := h.join(t, s, h.enroll(t, "gate-1", map[string]string{"branch": "B12"}))
	node, _ := h.app.FindRecordById(NodesCollection, s.m.NodeID())
	exp := node.GetDateTime("cert_expires").Time()

	// plenty of validity left: no renewal
	r := rawHS{h: h, s: s, cert: res.Cert}
	_, _, raw, _ := r.sendFull(t, nil, time.Now(), "nonce-norenew-1")
	var hs proto.HandshakeResponse
	if err := json.Unmarshal(raw, &hs); err != nil || hs.Cert != "" {
		t.Fatalf("no renewal expected: %v %q", err, hs.Cert)
	}

	// 10 days before expiry the client gets a fresh certificate and stores it
	at := exp.Add(-10 * 24 * time.Hour)
	h.m.now = func() time.Time { return at }
	c := s.client(t, h, func(o *client.Options) { o.Now = func() time.Time { return at } })
	if _, err := c.Handshake(context.Background()); err != nil {
		t.Fatal(err)
	}
	cur, _ := client.LoadCursor(s.app)
	if cur.Cert == res.Cert {
		t.Fatal("the certificate must be renewed")
	}
	cl, err := proto.VerifyCert(h.m.HubPub(), cur.Cert, at)
	old, _ := proto.VerifyCert(h.m.HubPub(), res.Cert, at)
	if err != nil || cl.Subject != s.m.NodeID() || cl.Ser != old.Ser || cl.Params["branch"] != "B12" || !cl.ExpiresAt.Time.After(old.ExpiresAt.Time) {
		t.Fatalf("renewed cert: %v %+v", err, cl)
	}
	node, _ = h.app.FindRecordById(NodesCollection, s.m.NodeID())
	if !node.GetDateTime("cert_expires").Time().After(exp) {
		t.Fatal("cert_expires must move")
	}
	// the new certificate is used for the next handshake
	h.m.now = func() time.Time { return at.Add(time.Hour) }
	c2 := s.client(t, h, func(o *client.Options) { o.Now = func() time.Time { return at.Add(time.Hour) } })
	if _, err := c2.Handshake(context.Background()); err != nil {
		t.Fatalf("handshake with the renewed certificate: %v", err)
	}
}

// ---- tokens do not cross ----

func TestSessionTokenAndPocketBaseTokensDoNotCross(t *testing.T) {
	h := newHub(t)
	s := newSpoke(t)
	h.join(t, s, h.enroll(t, "gate-1", nil))
	c := s.client(t, h)
	if _, err := c.Handshake(context.Background()); err != nil {
		t.Fatal(err)
	}
	do := func(path, tok string) int {
		req, _ := http.NewRequest("GET", h.srv.URL+path, nil)
		req.Header.Set("Authorization", tok)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	// PocketBase tokens (superuser, record) are refused by the node middleware
	suTok, _ := h.su.NewAuthToken()
	usrTok, _ := h.usr.NewAuthToken()
	for name, tok := range map[string]string{"superuser": suTok, "record": usrTok} {
		if st := do(proto.PathPing, "Bearer "+tok); st != 401 {
			t.Fatalf("%s token on a sync route: %d", name, st)
		}
	}
	// the sync session token is a guest elsewhere: a superusers-only route stays closed
	if st := do("/api/collections/_sync_nodes/records", "Bearer "+c.Token()); st != 403 && st != 401 {
		t.Fatalf("sync token on a superuser route: %d", st)
	}
	if st := do("/api/collections/_sync_nodes/records", suTok); st != 200 {
		t.Fatalf("superuser sanity check: %d", st)
	}
}

// ---- join code input ----

func TestJoinCodeSources(t *testing.T) {
	mk := func(in string) *cobra.Command {
		c := &cobra.Command{}
		c.SetIn(strings.NewReader(in))
		return c
	}
	if got, err := joinCode(mk(""), []string{"https://h", " AAAA-BBBB "}); err != nil || got != "AAAA-BBBB" {
		t.Fatalf("argument: %q %v", got, err)
	}
	if got, err := joinCode(mk("CCCC-DDDD\nrest\n"), []string{"https://h", "-"}); err != nil || got != "CCCC-DDDD" {
		t.Fatalf("stdin: %q %v", got, err)
	}
	t.Setenv(EnvEnrollCode, "EEEE-FFFF")
	if got, err := joinCode(mk(""), []string{"https://h"}); err != nil || got != "EEEE-FFFF" {
		t.Fatalf("env: %q %v", got, err)
	}
	t.Setenv(EnvEnrollCode, "")
	if _, err := joinCode(mk(""), []string{"https://h"}); err == nil {
		t.Fatal("missing code must fail")
	}
	if _, err := joinCode(mk(""), []string{"https://h", "-"}); err == nil {
		t.Fatal("empty stdin must fail")
	}
}

func newTestClock() *hlc.Clock { return hlc.NewClock(time.Now, 0) }
