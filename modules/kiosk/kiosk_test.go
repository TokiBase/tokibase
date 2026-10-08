//go:build !no_kiosk

package kiosk

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/sessions"
	"github.com/tokibase/tokibase/tests"
	"github.com/tokibase/tokibase/tools/security"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type env struct {
	app   *tests.TestApp
	m     *Module
	clk   *clock
	mux   http.Handler
	actor *core.Record
}

// setup builds the app; withSessions registers modules/sessions (test only),
// which gives the kernel.RevokeSession seam and token revocation.
func setup(t *testing.T, withSessions bool) *env {
	t.Helper()
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		kernel.OnAuthTokenIssue, kernel.SessionActive = nil, nil
		kernel.RevokeSession, kernel.RevokeUserSessions = nil, nil
		kernel.SetSyncStatusProvider(app, nil)
		app.Cleanup()
	})
	if withSessions {
		sessions.Register(app)
	}
	m := Register(app)
	clk := &clock{t: time.Now().UTC().Truncate(time.Second)}
	m.now = clk.now
	app.OnServe().BindFunc(func(se *core.ServeEvent) error {
		se.Router.GET("/t/me", func(e *core.RequestEvent) error {
			return e.JSON(200, map[string]any{"id": e.Auth.Id, "collection": e.Auth.Collection().Name})
		}).Bind(apis.RequireAuth())
		return se.Next()
	})
	router, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	var h http.Handler
	if err := app.OnServe().Trigger(&core.ServeEvent{App: app, Router: router}, func(se *core.ServeEvent) error {
		var err error
		h, err = se.Router.BuildMux()
		return err
	}); err != nil {
		t.Fatal(err)
	}
	actor, err := app.FindAuthRecordByEmail("users", "test@example.com")
	if err != nil {
		t.Fatal(err)
	}
	return &env{app: app, m: m, clk: clk, mux: h, actor: actor}
}

// provision creates a device for the test actor and returns its pairing code.
func (e *env) provision(t *testing.T, mod func(*Device), pin string) (string, *Device) {
	t.Helper()
	d := &Device{Name: "gate-1", AuthCollection: "users", AuthRecord: e.actor.Id}
	if mod != nil {
		mod(d)
	}
	_, code, err := Provision(e.app, d, pin, e.clk.now(), 0)
	if err != nil {
		t.Fatal(err)
	}
	return code, d
}

type req struct {
	method, path, body, cookie, auth, remote string
}

func (e *env) do(t *testing.T, r req) (int, map[string]any, *httptest.ResponseRecorder) {
	t.Helper()
	hr := httptest.NewRequest(r.method, r.path, strings.NewReader(r.body))
	hr.Header.Set("Content-Type", "application/json")
	hr.RemoteAddr = "127.0.0.1:50000"
	if r.remote != "" {
		hr.RemoteAddr = r.remote
	}
	if r.cookie != "" {
		hr.AddCookie(&http.Cookie{Name: CookieName, Value: r.cookie})
	}
	if r.auth != "" {
		hr.Header.Set("Authorization", r.auth)
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, hr)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out, rec
}

func (e *env) pair(t *testing.T, code string) (string, int) {
	t.Helper()
	st, _, rec := e.do(t, req{method: "POST", path: "/api/kiosk/pair", body: `{"code":"` + code + `"}`})
	for _, c := range rec.Result().Cookies() {
		if c.Name == CookieName {
			return c.Value, st
		}
	}
	return "", st
}

func (e *env) dbRow(t *testing.T, name string) *core.Record {
	t.Helper()
	r, err := findByName(e.app, name)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestTokenHashing(t *testing.T) {
	a, b := NewSecret(), NewSecret()
	if a == b || len(a) != 43 { // 32 bytes, unpadded base64url
		t.Fatalf("secrets %q %q", a, b)
	}
	if h := HashToken("abc"); h != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("sha256: %s", h)
	}
	e := setup(t, false)
	code, _ := e.provision(t, nil, "")
	tok, st := e.pair(t, code)
	if st != 200 || tok == "" {
		t.Fatalf("pair %d", st)
	}
	r := e.dbRow(t, "gate-1")
	if r.GetString("token_hash") != HashToken(tok) || strings.Contains(r.GetString("token_hash"), tok) {
		t.Fatal("only the hash of the device token may be stored")
	}
	if r.GetString("pairing_hash") != "" {
		t.Fatal("the pairing code must be cleared once used")
	}
}

func TestPairingSingleUseAndExpiry(t *testing.T) {
	e := setup(t, false)
	code, _ := e.provision(t, nil, "")
	if _, st := e.pair(t, "nope"); st != 403 {
		t.Fatalf("wrong code: %d", st)
	}
	tok, st := e.pair(t, code)
	if st != 200 || tok == "" {
		t.Fatalf("first use: %d", st)
	}
	if _, st := e.pair(t, code); st != 403 {
		t.Fatalf("second use must fail, got %d", st)
	}
	// a cookie never works for another device and a stranger cookie is refused
	if st, _, _ := e.do(t, req{method: "POST", path: "/api/kiosk/session", cookie: "bogus"}); st != 401 {
		t.Fatalf("bogus cookie: %d", st)
	}
	// expiry on the injected clock
	code2, _ := e.provision(t, func(d *Device) { d.Name = "gate-2" }, "")
	e.clk.add(pairingTTL + time.Second)
	if _, st := e.pair(t, code2); st != 403 {
		t.Fatalf("expired code: %d", st)
	}
	// rotate gives a new code and drops the cookie
	r := e.dbRow(t, "gate-1")
	c3 := newPairing(r, e.clk.now(), 0)
	if err := e.app.Save(r); err != nil {
		t.Fatal(err)
	}
	if st, _, _ := e.do(t, req{method: "POST", path: "/api/kiosk/session", cookie: tok}); st != 401 {
		t.Fatalf("old cookie after rotate: %d", st)
	}
	if _, st := e.pair(t, c3); st != 200 {
		t.Fatalf("rotated code: %d", st)
	}
}

func TestPairRemoteOnlyWithFlag(t *testing.T) {
	e := setup(t, false)
	code, _ := e.provision(t, nil, "")
	remote := func() int {
		st, _, _ := e.do(t, req{method: "POST", path: "/api/kiosk/pair", body: `{"code":"` + code + `"}`, remote: "10.1.2.3:4000"})
		return st
	}
	if st := remote(); st != 403 {
		t.Fatalf("remote pairing: %d", st)
	}
	// a loopback proxy hop does not make a remote client local
	hr := httptest.NewRequest("POST", "/api/kiosk/pair", strings.NewReader(`{"code":"`+code+`"}`))
	hr.RemoteAddr = "127.0.0.1:1"
	hr.Header.Set("X-Forwarded-For", "10.1.2.3")
	if isLoopback(hr) {
		t.Fatal("a proxied request is not loopback")
	}
	t.Setenv("TOKI_KIOSK_ALLOW_REMOTE", "1")
	if st := remote(); st != 200 {
		t.Fatalf("remote pairing allowed by env: %d", st)
	}
}

func TestBindIP(t *testing.T) {
	d := &Device{BindIP: "10.0.0.9"}
	if !d.bindOK("10.0.0.9") || d.bindOK("10.0.0.10") || d.bindOK("junk") {
		t.Fatal("exact bind_ip")
	}
	d = &Device{BindIP: "192.168.1.0/24"}
	if !d.bindOK("192.168.1.77") || d.bindOK("192.168.2.1") {
		t.Fatal("cidr bind_ip")
	}
	if !(&Device{}).bindOK("1.2.3.4") {
		t.Fatal("no bind_ip means any")
	}
	e := setup(t, false)
	code, _ := e.provision(t, func(d *Device) { d.BindIP = "10.9.9.9" }, "")
	if _, st := e.pair(t, code); st != 403 { // pair comes from 127.0.0.1
		t.Fatalf("pair from another ip: %d", st)
	}
	code2, _ := e.provision(t, func(d *Device) { d.Name = "gate-2"; d.BindIP = "127.0.0.1" }, "")
	tok, st := e.pair(t, code2)
	if st != 200 {
		t.Fatalf("pair on the bound ip: %d", st)
	}
	if st, _, _ := e.do(t, req{method: "POST", path: "/api/kiosk/session", cookie: tok}); st != 200 {
		t.Fatalf("session on the bound ip: %d", st)
	}
	if st, _, _ := e.do(t, req{method: "POST", path: "/api/kiosk/session", cookie: tok, remote: "10.0.0.5:1"}); st != 403 {
		t.Fatalf("session from another ip: %d", st)
	}
	if st, _, _ := e.do(t, req{method: "GET", path: "/api/kiosk/status", cookie: tok, remote: "10.0.0.5:1"}); st != 403 {
		t.Fatalf("status from another ip: %d", st)
	}
}

func TestPinBackoffInjectedClock(t *testing.T) {
	e := setup(t, false)
	e.provision(t, nil, "4321")
	id := e.dbRow(t, "gate-1").Id
	m := e.m
	for i := 1; i < pinFailures; i++ {
		if ok, _ := m.pinAllowed(id); !ok {
			t.Fatalf("locked after %d failures", i-1)
		}
		if n, lock := m.pinFailed(id); n != i || lock != 0 {
			t.Fatalf("failure %d: n=%d lock=%v", i, n, lock)
		}
	}
	if _, lock := m.pinFailed(id); lock != 60*time.Second {
		t.Fatalf("5th failure locks 60s, got %v", lock)
	}
	if ok, wait := m.pinAllowed(id); ok || wait != 60*time.Second {
		t.Fatalf("allowed=%v wait=%v", ok, wait)
	}
	// persisted: a fresh Module (a restart) still has the lockout
	if ok, _ := newModule(e.app).pinAllowed(id); ok {
		t.Fatal("the brake must survive a restart")
	}
	e.clk.add(59 * time.Second)
	if ok, _ := m.pinAllowed(id); ok {
		t.Fatal("still locked at 59s")
	}
	e.clk.add(2 * time.Second)
	if ok, _ := m.pinAllowed(id); !ok {
		t.Fatal("free after 61s")
	}
	// the next lockout doubles: 5 more failures give 120 s, then 240 s
	for i := 0; i < pinFailures-1; i++ {
		m.pinFailed(id)
	}
	if _, lock := m.pinFailed(id); lock != 120*time.Second {
		t.Fatalf("second lockout: %v", lock)
	}
	e.clk.add(121 * time.Second)
	for i := 0; i < pinFailures-1; i++ {
		m.pinFailed(id)
	}
	if _, lock := m.pinFailed(id); lock != 240*time.Second {
		t.Fatalf("third lockout: %v", lock)
	}
	m.pinOK(id)
	if ok, _ := m.pinAllowed(id); !ok {
		t.Fatal("a good PIN clears the brake")
	}
	for i := 0; i < pinFailures; i++ {
		_, lock := m.pinFailed(id)
		if i == pinFailures-1 && lock != 60*time.Second {
			t.Fatalf("after a reset the first lockout is 60 s again, got %v", lock)
		}
	}
}

// Parallel wrong PINs: the brake is applied atomically, so exactly pinFailures
// of them are evaluated and every other one is refused with 429.
func TestUnlockBurstIsBraked(t *testing.T) {
	e := setup(t, false)
	code, _ := e.provision(t, nil, "4321")
	tok, _ := e.pair(t, code)
	var wg sync.WaitGroup
	var mu sync.Mutex
	got := map[int]int{}
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// distinct client IPs: only the per-device brake may stop them
			st, _, _ := e.do(t, req{method: "POST", path: "/api/kiosk/unlock", cookie: tok, body: `{"pin":"0000"}`, remote: "10.0.1." + strconv.Itoa(i+1) + ":1"})
			mu.Lock()
			got[st]++
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	if got[401] != pinFailures || got[429] != 40-pinFailures {
		t.Fatalf("burst result %v", got)
	}
	if st, _, _ := e.do(t, req{method: "POST", path: "/api/kiosk/unlock", cookie: tok, body: `{"pin":"4321"}`}); st != 429 {
		t.Fatalf("right PIN during the lockout: %d", st)
	}
}

func TestPerIPThrottle(t *testing.T) {
	e := setup(t, false)
	for i := 0; i < pairPerWindow; i++ {
		if !e.m.allow("pair", "1.2.3.4", pairPerWindow) {
			t.Fatalf("attempt %d refused", i)
		}
	}
	if e.m.allow("pair", "1.2.3.4", pairPerWindow) {
		t.Fatal("over the limit")
	}
	if !e.m.allow("pair", "1.2.3.5", pairPerWindow) || !e.m.allow("unlock", "1.2.3.4", unlockPerWin) {
		t.Fatal("limits are per tag and IP")
	}
	e.clk.add(throttleWindow + time.Second)
	if !e.m.allow("pair", "1.2.3.4", pairPerWindow) {
		t.Fatal("the window must reset")
	}
	// over HTTP, without any PocketBase rate-limit rule
	var last int
	var body map[string]any
	for i := 0; i <= pairPerWindow+1; i++ {
		last, body, _ = e.do(t, req{method: "POST", path: "/api/kiosk/pair", body: `{"code":"nope"}`, remote: "127.0.0.1:" + strconv.Itoa(1000+i)})
	}
	if last != 429 || body["data"].(map[string]any)["code"] != "rate_limited" {
		t.Fatalf("pair flood: %d %v", last, body)
	}
}

func TestPairingConcurrentSingleWinner(t *testing.T) {
	e := setup(t, false)
	code, _ := e.provision(t, nil, "")
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, st := e.pair(t, code); st == 200 {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("%d concurrent pairings succeeded", wins)
	}
}

func TestRevokedActor(t *testing.T) {
	e := setup(t, false)
	code, _ := e.provision(t, nil, "")
	tok, _ := e.pair(t, code)
	if st, _, _ := e.do(t, req{method: "POST", path: "/api/kiosk/session", cookie: tok}); st != 200 {
		t.Fatalf("session: %d", st)
	}
	if err := e.app.Delete(e.actor); err != nil {
		t.Fatal(err)
	}
	st, body, _ := e.do(t, req{method: "POST", path: "/api/kiosk/session", cookie: tok})
	if st != 403 || body["data"].(map[string]any)["code"] != "actor_gone" {
		t.Fatalf("deleted actor: %d %v", st, body)
	}
}

func TestProvisionRefusesSuperuserAndMissingActor(t *testing.T) {
	e := setup(t, false)
	if _, _, err := Provision(e.app, &Device{Name: "a", AuthCollection: "_superusers", AuthRecord: "x"}, "", e.clk.now(), 0); err == nil {
		t.Fatal("a superuser must not be an actor")
	}
	if _, _, err := Provision(e.app, &Device{Name: "a", AuthCollection: "users", AuthRecord: "missing0000000x"}, "", e.clk.now(), 0); err == nil {
		t.Fatal("a missing actor must be refused")
	}
	if _, _, err := Provision(e.app, &Device{Name: "bad name", AuthCollection: "users", AuthRecord: e.actor.Id}, "", e.clk.now(), 0); err == nil {
		t.Fatal("a bad name must be refused")
	}
	if _, _, err := Provision(e.app, &Device{Name: "a", AuthCollection: "users", AuthRecord: e.actor.Id}, "12", e.clk.now(), 0); err == nil {
		t.Fatal("a short PIN must be refused")
	}
}

func decodeTTL(t *testing.T, body map[string]any) float64 {
	t.Helper()
	v, ok := body["ttl_s"].(float64)
	if !ok {
		t.Fatalf("no ttl_s in %v", body)
	}
	return v
}

// pair → session → SDK-style call → lock → 401 → unlock → 200.
func TestFlowPairSessionLockUnlock(t *testing.T) {
	e := setup(t, true)
	code, _ := e.provision(t, func(d *Device) { d.LockAfterS = 30 }, "4321")
	tok, st := e.pair(t, code)
	if st != 200 {
		t.Fatalf("pair %d", st)
	}
	// no cookie, no session
	if st, _, _ := e.do(t, req{method: "POST", path: "/api/kiosk/session"}); st != 401 {
		t.Fatalf("session without cookie: %d", st)
	}
	st, s1, _ := e.do(t, req{method: "POST", path: "/api/kiosk/session", cookie: tok})
	if st != 200 {
		t.Fatalf("session %d %v", st, s1)
	}
	if decodeTTL(t, s1) != 12*3600 || s1["pin_required"] != true || s1["lock_after_s"].(float64) != 30 {
		t.Fatalf("session body %v", s1)
	}
	auth := s1["token"].(string)
	if st, me, _ := e.do(t, req{method: "GET", path: "/t/me", auth: auth}); st != 200 || me["id"] != e.actor.Id {
		t.Fatalf("SDK style call: %d %v", st, me)
	}
	if st, _, _ := e.do(t, req{method: "GET", path: "/t/me"}); st != 401 {
		t.Fatalf("guest: %d", st)
	}

	// lock: the token dies, the session route answers 423
	st, lk, _ := e.do(t, req{method: "POST", path: "/api/kiosk/lock", cookie: tok, auth: auth})
	if st != 200 || lk["locked"] != true || lk["revoked"].(float64) < 1 {
		t.Fatalf("lock %d %v", st, lk)
	}
	if st, _, _ := e.do(t, req{method: "GET", path: "/t/me", auth: auth}); st != 401 {
		t.Fatalf("a locked device's token must be refused, got %d", st)
	}
	if st, _, _ := e.do(t, req{method: "POST", path: "/api/kiosk/session", cookie: tok}); st != 423 {
		t.Fatalf("session while locked: %d", st)
	}

	// wrong then right PIN
	if st, _, _ := e.do(t, req{method: "POST", path: "/api/kiosk/unlock", cookie: tok, body: `{"pin":"0000"}`}); st != 401 {
		t.Fatalf("wrong pin: %d", st)
	}
	st, s2, _ := e.do(t, req{method: "POST", path: "/api/kiosk/unlock", cookie: tok, body: `{"pin":"4321"}`})
	if st != 200 {
		t.Fatalf("unlock %d %v", st, s2)
	}
	if st, _, _ := e.do(t, req{method: "GET", path: "/t/me", auth: s2["token"].(string)}); st != 200 {
		t.Fatalf("call after unlock: %d", st)
	}
	if e.dbRow(t, "gate-1").GetBool("locked") {
		t.Fatal("unlock must clear the flag")
	}
	// the old token stays dead
	if st, _, _ := e.do(t, req{method: "GET", path: "/t/me", auth: auth}); st != 401 {
		t.Fatalf("old token after unlock: %d", st)
	}
}

func TestLockNeedsPinAndBruteForceIsBraked(t *testing.T) {
	e := setup(t, true)
	code, _ := e.provision(t, func(d *Device) { d.Name = "nopin" }, "")
	tok, _ := e.pair(t, code)
	if st, _, _ := e.do(t, req{method: "POST", path: "/api/kiosk/lock", cookie: tok}); st != 409 {
		t.Fatalf("lock without a PIN: %d", st)
	}
	code, _ = e.provision(t, func(d *Device) { d.Name = "pinned" }, "9876")
	tok, _ = e.pair(t, code)
	for i := 0; i < pinFailures; i++ {
		if st, _, _ := e.do(t, req{method: "POST", path: "/api/kiosk/unlock", cookie: tok, body: `{"pin":"1111"}`}); st != 401 {
			t.Fatalf("failure %d: %d", i, st)
		}
	}
	// the right PIN is refused during the lockout
	st, _, rec := e.do(t, req{method: "POST", path: "/api/kiosk/unlock", cookie: tok, body: `{"pin":"9876"}`})
	if st != 429 || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("lockout: %d", st)
	}
	e.clk.add(61 * time.Second)
	if st, _, _ := e.do(t, req{method: "POST", path: "/api/kiosk/unlock", cookie: tok, body: `{"pin":"9876"}`}); st != 200 {
		t.Fatalf("after the lockout: %d", st)
	}
}

type fakeAudit struct {
	mu  sync.Mutex
	got []string
}

func (f *fakeAudit) sink(action, collection, record string, details map[string]any) {
	f.mu.Lock()
	f.got = append(f.got, action+"@"+collection)
	f.mu.Unlock()
}

func TestAudit(t *testing.T) {
	f := &fakeAudit{}
	SetAuditSink(f.sink)
	t.Cleanup(func() { SetAuditSink(nil) })
	e := setup(t, false)
	code, _ := e.provision(t, nil, "5555")
	tok, _ := e.pair(t, code)
	e.do(t, req{method: "POST", path: "/api/kiosk/unlock", cookie: tok, body: `{"pin":"1"}`})
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.Join(f.got, ",") != "kiosk.pair@_kiosk_devices,kiosk.unlock_fail@_kiosk_devices" {
		t.Fatalf("audit: %v", f.got)
	}
}

// Without modules/sessions the kiosk_gen claim still kills the tokens of a
// locked device within one request, and the TTL is not capped.
func TestNoSessionsLockStillRevokes(t *testing.T) {
	e := setup(t, false)
	code, _ := e.provision(t, nil, "4321")
	tok, _ := e.pair(t, code)
	_, s, _ := e.do(t, req{method: "POST", path: "/api/kiosk/session", cookie: tok})
	if got := decodeTTL(t, s); got != 12*3600 {
		t.Fatalf("ttl %v", got)
	}
	old := s["token"].(string)
	if st, _, _ := e.do(t, req{method: "GET", path: "/t/me", auth: old}); st != 200 {
		t.Fatalf("token before lock: %d", st)
	}
	st, lk, _ := e.do(t, req{method: "POST", path: "/api/kiosk/lock", cookie: tok})
	if st != 200 || lk["revocable"] != true {
		t.Fatalf("lock %d %v", st, lk)
	}
	if st, _, _ := e.do(t, req{method: "GET", path: "/t/me", auth: old}); st != 401 {
		t.Fatalf("a locked device's token must die at once: %d", st)
	}
	if st, _, _ := e.do(t, req{method: "POST", path: "/api/kiosk/session", cookie: tok}); st != 423 {
		t.Fatalf("no new token while locked: %d", st)
	}
	st, u, _ := e.do(t, req{method: "POST", path: "/api/kiosk/unlock", cookie: tok, body: `{"pin":"4321"}`})
	if st != 200 {
		t.Fatalf("unlock %d", st)
	}
	if st, _, _ := e.do(t, req{method: "GET", path: "/t/me", auth: u["token"].(string)}); st != 200 {
		t.Fatalf("new token: %d", st)
	}
	if st, _, _ := e.do(t, req{method: "GET", path: "/t/me", auth: old}); st != 401 {
		t.Fatalf("old token after unlock: %d", st)
	}
}

// Every token ever issued to the device dies on lock (far more than the old 64
// in-memory sids), with and without modules/sessions, and the record is in the DB.
func TestLockRevokesAllIssuedTokens(t *testing.T) {
	for _, withSessions := range []bool{true, false} {
		withSessions := withSessions
		t.Run("sessions="+strconv.FormatBool(withSessions), func(t *testing.T) {
			e := setup(t, withSessions)
			code, _ := e.provision(t, nil, "4321")
			tok, _ := e.pair(t, code)
			var toks []string
			for i := 0; i < 70; i++ {
				_, s, _ := e.do(t, req{method: "POST", path: "/api/kiosk/session", cookie: tok})
				toks = append(toks, s["token"].(string))
			}
			if withSessions {
				var n int
				if err := e.app.DB().NewQuery("SELECT COUNT(*) FROM {{" + SessionsTable + "}}").Row(&n); err != nil || n != 70 {
					t.Fatalf("recorded sessions: %d %v", n, err)
				}
			}
			// a "restart": nothing is kept in memory, the module is rebuilt
			e.m.mu.Lock()
			e.m.touch = map[string]time.Time{}
			e.m.mu.Unlock()
			if st, _, _ := e.do(t, req{method: "POST", path: "/api/kiosk/lock", cookie: tok}); st != 200 {
				t.Fatalf("lock %d", st)
			}
			for i, tk := range toks {
				if st, _, _ := e.do(t, req{method: "GET", path: "/t/me", auth: tk}); st != 401 {
					t.Fatalf("sessions=%v: token %d still works after lock: %d", withSessions, i, st)
				}
			}
		})
	}
}

func runCLI(t *testing.T, e *env, args ...string) (string, error) {
	t.Helper()
	cmd := NewCommand(e.app)
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestRevokeAndRotateKillIssuedTokens(t *testing.T) {
	for _, withSessions := range []bool{true, false} {
		for _, verb := range []string{"revoke", "rotate"} {
			withSessions, verb := withSessions, verb
			t.Run(verb+strconv.FormatBool(withSessions), func(t *testing.T) {
				e := setup(t, withSessions)
				code, _ := e.provision(t, nil, "4321")
				tok, _ := e.pair(t, code)
				_, s, _ := e.do(t, req{method: "POST", path: "/api/kiosk/session", cookie: tok})
				old := s["token"].(string)
				if st, _, _ := e.do(t, req{method: "GET", path: "/t/me", auth: old}); st != 200 {
					t.Fatalf("token before %s: %d", verb, st)
				}
				if out, err := runCLI(t, e, verb, "gate-1"); err != nil {
					t.Fatalf("%s: %v %s", verb, err, out)
				}
				if st, _, _ := e.do(t, req{method: "GET", path: "/t/me", auth: old}); st != 401 {
					t.Fatalf("sessions=%v: token after %s: %d", withSessions, verb, st)
				}
			})
		}
	}
}

// off-boarding: the actor is deleted first, then the operator unpairs the device.
func TestRevokeRotateSetPinWithDeletedActor(t *testing.T) {
	e := setup(t, true)
	code, _ := e.provision(t, nil, "4321")
	e.pair(t, code)
	if err := e.app.Delete(e.actor); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"set-pin", "gate-1", "--pin", "9999"}, {"revoke", "gate-1"}, {"rotate", "gate-1"}} {
		if out, err := runCLI(t, e, args...); err != nil {
			t.Fatalf("%v with a deleted actor: %v %s", args, err, out)
		}
	}
	if e.dbRow(t, "gate-1").GetString("token_hash") != "" {
		t.Fatal("the device must be unpaired")
	}
}

func TestAgentsAndSystemActorsRefused(t *testing.T) {
	e := setup(t, false)
	for _, col := range []string{kernel.CollectionNameAgents, core.CollectionNameSuperusers, "_authOrigins"} {
		if _, _, err := Provision(e.app, &Device{Name: "a", AuthCollection: col, AuthRecord: "x"}, "", e.clk.now(), 0); err == nil {
			t.Fatalf("%s must not be an actor", col)
		}
	}
	// also when set directly on a saved device
	e.provision(t, nil, "")
	r := e.dbRow(t, "gate-1")
	r.Set("auth_collection", kernel.CollectionNameAgents)
	if err := e.app.Save(r); err == nil {
		t.Fatal("switching a device to an _agents actor must fail")
	}
	d := &Device{AuthCollection: kernel.CollectionNameAgents, AuthRecord: "x"}
	if _, err := actorOf(e.app, d); err == nil {
		t.Fatal("actorOf must refuse _agents")
	}
}

// lock must not trust the claims of an unverified Authorization token.
func TestLockIgnoresForgedAuthorization(t *testing.T) {
	e := setup(t, true)
	code, _ := e.provision(t, nil, "4321")
	tok, _ := e.pair(t, code)
	victim, err := e.actor.NewAuthToken() // a normal login of the same actor, not issued by the kiosk
	if err != nil {
		t.Fatal(err)
	}
	vc, _ := security.ParseUnverifiedJWT(victim)
	forged, err := security.NewJWT(jwt.MapClaims{"id": e.actor.Id, "type": "auth", "collectionId": e.actor.Collection().Id, "sid": vc["sid"]}, "not-the-key", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if st, _, _ := e.do(t, req{method: "GET", path: "/t/me", auth: victim}); st != 200 {
		t.Fatalf("victim token: %d", st)
	}
	if st, _, _ := e.do(t, req{method: "POST", path: "/api/kiosk/lock", cookie: tok, auth: forged}); st != 200 {
		t.Fatalf("lock: %d", st)
	}
	if st, _, _ := e.do(t, req{method: "GET", path: "/t/me", auth: victim}); st != 200 {
		t.Fatalf("a forged Authorization revoked another session: %d", st)
	}
}

func TestStaticTokenCannotBeRefreshed(t *testing.T) {
	e := setup(t, false)
	code, _ := e.provision(t, nil, "")
	tok, _ := e.pair(t, code)
	_, s, _ := e.do(t, req{method: "POST", path: "/api/kiosk/session", cookie: tok})
	auth := s["token"].(string)
	st, body, _ := e.do(t, req{method: "POST", path: "/api/collections/users/auth-refresh", auth: auth})
	if st != 200 || body["token"] != auth {
		t.Fatalf("auth-refresh must hand back the same token: %d", st)
	}
}

func TestIsLoopback(t *testing.T) {
	for _, c := range []struct {
		remote string
		hdr    map[string]string
		want   bool
	}{
		{"127.0.0.1:1", nil, true},
		{"[::1]:1", nil, true},
		{"[::ffff:127.0.0.1]:1", nil, true},
		{"10.0.0.5:1", nil, false},
		{"127.0.0.1:1", map[string]string{"X-Forwarded-For": "8.8.8.8"}, false},
		{"127.0.0.1:1", map[string]string{"X-Real-IP": "8.8.8.8"}, false},
		{"127.0.0.1:1", map[string]string{"Forwarded": "for=8.8.8.8"}, false},
		{"garbage", nil, false},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.remote
		for k, v := range c.hdr {
			r.Header.Set(k, v)
		}
		if got := isLoopback(r); got != c.want {
			t.Errorf("%s %v: got %v", c.remote, c.hdr, got)
		}
	}
}

func TestUnlockEdgeCases(t *testing.T) {
	e := setup(t, false)
	code, _ := e.provision(t, nil, "4321")
	tok, _ := e.pair(t, code)
	// unlocking a device that is not locked is harmless
	if st, _, _ := e.do(t, req{method: "POST", path: "/api/kiosk/unlock", cookie: tok, body: `{"pin":"4321"}`}); st != 200 {
		t.Fatalf("unlock of an unlocked device: %d", st)
	}
	// when the session cannot be issued the device stays locked
	e.do(t, req{method: "POST", path: "/api/kiosk/lock", cookie: tok})
	if err := e.app.Delete(e.actor); err != nil {
		t.Fatal(err)
	}
	if st, _, _ := e.do(t, req{method: "POST", path: "/api/kiosk/unlock", cookie: tok, body: `{"pin":"4321"}`}); st != 403 {
		t.Fatalf("unlock with a deleted actor: %d", st)
	}
	if !e.dbRow(t, "gate-1").GetBool("locked") {
		t.Fatal("the device must stay locked when no session could be issued")
	}
}

func TestStatusWithAndWithoutSync(t *testing.T) {
	e := setup(t, false)
	code, _ := e.provision(t, nil, "")
	tok, _ := e.pair(t, code)
	if st, _, _ := e.do(t, req{method: "GET", path: "/api/kiosk/status"}); st != 401 {
		t.Fatalf("status without a device or auth: %d", st)
	}
	st, b, _ := e.do(t, req{method: "GET", path: "/api/kiosk/status", cookie: tok})
	if st != 200 || b["edge"] != true || b["time"] == nil {
		t.Fatalf("status %d %v", st, b)
	}
	for _, k := range []string{"hub", "pending_changes", "last_sync"} {
		if _, ok := b[k]; ok {
			t.Fatalf("%s must be omitted without sync: %v", k, b)
		}
	}
	if ps, ok := b["printers"].([]any); !ok || len(ps) != 0 {
		t.Fatalf("printers %v", b["printers"])
	}

	last := e.clk.now().Add(-time.Minute)
	kernel.SetSyncStatusProvider(e.app, func() kernel.SyncStatus {
		return kernel.SyncStatus{State: kernel.SyncStateOffline, HubReachable: false, Pending: 7, LastSync: last}
	})
	apis.SetHealthExtra(e.app, "printer", func(core.App) any {
		return map[string]any{"printers": []map[string]any{
			{"name": "counter", "enabled": true, "status": map[string]any{"state": "paper"}},
			{"name": "off", "enabled": false, "status": map[string]any{"state": "ok"}},
		}}
	})
	_, b, _ = e.do(t, req{method: "GET", path: "/api/kiosk/status", cookie: tok})
	if b["hub"] != false || b["pending_changes"].(float64) != 7 || b["last_sync"] != last.Format(time.RFC3339) {
		t.Fatalf("with sync: %v", b)
	}
	ps := b["printers"].([]any)
	if len(ps) != 1 || ps[0].(map[string]any)["name"] != "counter" || ps[0].(map[string]any)["state"] != "paper" {
		t.Fatalf("printers %v", ps)
	}
	kernel.SetSyncStatusProvider(e.app, func() kernel.SyncStatus { return kernel.SyncStatus{State: kernel.SyncStateOnline, HubReachable: true} })
	_, b, _ = e.do(t, req{method: "GET", path: "/api/kiosk/status", cookie: tok})
	if b["hub"] != true || b["pending_changes"].(float64) != 0 || b["last_sync"] != nil {
		t.Fatalf("online: %v", b)
	}
}

func TestServedAssets(t *testing.T) {
	e := setup(t, false)
	st, _, rec := e.do(t, req{method: "GET", path: "/kiosk/kiosk.js"})
	b := rec.Body.String()
	if st != 200 || !strings.Contains(rec.Header().Get("Content-Type"), "javascript") || !strings.Contains(b, "pocketbase_auth") || len(b) > 12<<10 {
		t.Fatalf("kiosk.js: %d %s %d bytes", st, rec.Header().Get("Content-Type"), len(b))
	}
	st, _, rec = e.do(t, req{method: "GET", path: "/kiosk/pair"})
	if st != 200 || !strings.Contains(rec.Body.String(), "/kiosk/kiosk.js") {
		t.Fatalf("pair page: %d", st)
	}
}

func TestCookieAttributes(t *testing.T) {
	e := setup(t, false)
	code, _ := e.provision(t, nil, "")
	_, _, rec := e.do(t, req{method: "POST", path: "/api/kiosk/pair", body: `{"code":"` + code + `"}`})
	c := rec.Result().Cookies()
	if len(c) != 1 || !c[0].HttpOnly || c[0].SameSite != http.SameSiteStrictMode || c[0].Path != "/api/kiosk" || c[0].Secure {
		t.Fatalf("cookie %+v", c)
	}
	code, _ = e.provision(t, func(d *Device) { d.Name = "tls" }, "")
	hr := httptest.NewRequest("POST", "/api/kiosk/pair", strings.NewReader(`{"code":"`+code+`"}`))
	hr.RemoteAddr = "127.0.0.1:1"
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("X-Forwarded-Proto", "https")
	// a proxy header makes the request non-local; allow it for this check
	t.Setenv("TOKI_KIOSK_ALLOW_REMOTE", "1")
	r2 := httptest.NewRecorder()
	e.mux.ServeHTTP(r2, hr)
	c = r2.Result().Cookies()
	if len(c) != 1 || !c[0].Secure {
		t.Fatalf("https cookie %+v", c)
	}
}

func TestNodeBinding(t *testing.T) {
	e := setup(t, false)
	code, _ := e.provision(t, func(d *Device) { d.NodeID = "node-A" }, "")
	tok, _ := e.pair(t, code)
	kernel.SetNodeIdentity(e.app, fakeNode("node-B"))
	t.Cleanup(func() { kernel.SetNodeIdentity(e.app, nil) })
	if st, _, _ := e.do(t, req{method: "POST", path: "/api/kiosk/session", cookie: tok}); st != 403 {
		t.Fatalf("another node: %d", st)
	}
	kernel.SetNodeIdentity(e.app, fakeNode("node-A"))
	if st, _, _ := e.do(t, req{method: "POST", path: "/api/kiosk/session", cookie: tok}); st != 200 {
		t.Fatalf("same node: %d", st)
	}
}
