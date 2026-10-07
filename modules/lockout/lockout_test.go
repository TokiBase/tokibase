package lockout

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/tests"
)

type env struct {
	app *tests.TestApp
	m   *Module
	now time.Time
	mux http.Handler
}

func setup(t *testing.T, p Policy) *env {
	t.Helper()
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)
	t.Setenv("TOKI_LOCKOUT_THRESHOLD", strconv.Itoa(p.Threshold))
	m := Register(app)
	e := &env{app: app, m: m, now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	m.now = func() time.Time { return e.now }
	e.mux = buildMux(t, app)
	return e
}

// buildMux builds the router once: ApiScenario rebuilds it per scenario and
// cannot reuse one app across several requests (stateful lockout needs that).
func buildMux(t *testing.T, app *tests.TestApp) http.Handler {
	router, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	var h http.Handler
	err = app.OnServe().Trigger(&core.ServeEvent{App: app, Router: router}, func(se *core.ServeEvent) error {
		mux, err := se.Router.BuildMux()
		h = mux
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func post(t *testing.T, h http.Handler, url, body, ip string, status int, content ...string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", ip)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != status {
		t.Fatalf("%s: status %d want %d: %s", body, rec.Code, status, rec.Body.String())
	}
	for _, c := range content {
		if !strings.Contains(rec.Body.String(), c) {
			t.Fatalf("body %s missing %q", rec.Body.String(), c)
		}
	}
	return rec.Result()
}

func (e *env) pw(t *testing.T, identity, password, ip string, status int, content ...string) *http.Response {
	t.Helper()
	return post(t, e.mux, "/api/collections/users/auth-with-password",
		`{"identity":"`+identity+`","password":"`+password+`"}`, ip, status, content...)
}

func TestPasswordLockProgressive(t *testing.T) {
	e := setup(t, Policy{Threshold: 5})
	if e.m.policy.Threshold != 5 {
		t.Fatalf("policy %+v", e.m.policy)
	}
	ok := `"mfaId":` // the test users collection requires MFA: 401 + mfaId = valid password
	bad := `"message":"Failed to authenticate."`

	// failures from different IPs (and different letter case) accumulate
	for i := 0; i < 5; i++ {
		id := "test@example.com"
		if i%2 == 1 {
			id = "TEST@example.com"
		}
		res := e.pw(t, id, "wrongpass", "10.0.0."+strconv.Itoa(i+1), 400, bad)
		if res.Header.Get("Retry-After") != "" {
			t.Fatal("unexpected Retry-After before lock")
		}
	}

	// 6th attempt with the CORRECT password still fails and carries Retry-After
	res := e.pw(t, "test@example.com", "1234567890", "10.1.1.1", 400, bad)
	if res.Header.Get("Retry-After") != "60" {
		t.Fatalf("Retry-After = %q", res.Header.Get("Retry-After"))
	}

	rows, err := List(e.app)
	if err != nil || len(rows) != 1 || rows[0].Key != "users:test@example.com" || rows[0].LockedUntil == "" {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}

	// still locked 59s later (Retry-After shrinks); another identity is not affected
	e.now = e.now.Add(59 * time.Second)
	res = e.pw(t, "test@example.com", "1234567890", "10.1.1.2", 400, bad)
	if res.Header.Get("Retry-After") != "1" {
		t.Fatalf("Retry-After = %q", res.Header.Get("Retry-After"))
	}
	res = e.pw(t, "nobody@example.com", "x", "10.1.1.3", 400, bad)
	if res.Header.Get("Retry-After") != "" {
		t.Fatal("unexpected Retry-After for unrelated identity")
	}

	// after the lock the correct password succeeds and resets the record
	e.now = e.now.Add(2 * time.Second)
	e.pw(t, "test@example.com", "1234567890", "10.1.1.4", 401, ok)
	if rows, _ := List(e.app); len(rows) != 0 {
		t.Fatalf("expected no persisted rows (nobody has 1 cached failure), got %+v", rows)
	}

	// unknown identities lock too (no enumeration): 4 more failures reach 5
	for i := 0; i < 4; i++ {
		e.pw(t, "nobody@example.com", "x", "10.2.0.1", 400, bad)
	}
	res = e.pw(t, "nobody@example.com", "x", "10.2.0.2", 400, bad)
	if res.Header.Get("Retry-After") != "60" {
		t.Fatalf("Retry-After = %q", res.Header.Get("Retry-After"))
	}
}

func TestEscalationAndWindow(t *testing.T) {
	p := Policy{Threshold: 2, Window: 15 * time.Minute, Base: time.Minute, Max: 3 * time.Minute}
	cases := map[int]time.Duration{1: time.Minute, 2: 2 * time.Minute, 3: 3 * time.Minute, 9: 3 * time.Minute}
	for n, want := range cases {
		if got := p.LockDuration(n); got != want {
			t.Errorf("LockDuration(%d)=%v want %v", n, got, want)
		}
	}

	e := setup(t, Policy{Threshold: 3})
	e.m.policy = p
	k := Key("users", "a@b.c")
	fail := func() { e.m.failure("users", "a@b.c", k) }
	fail()
	fail() // lock #1 (1m)
	if w := e.m.lockedFor(k); w != time.Minute {
		t.Fatalf("lock1 %v", w)
	}
	e.now = e.now.Add(time.Minute + time.Second)
	fail()
	fail() // lock #2 (2m)
	if w := e.m.lockedFor(k); w != 2*time.Minute {
		t.Fatalf("lock2 %v", w)
	}
	// quiet for longer than the window after the lock ends -> forgotten
	e.now = e.now.Add(2*time.Minute + 16*time.Minute)
	fail()
	fail()
	if w := e.m.lockedFor(k); w != time.Minute {
		t.Fatalf("after window expected lock #1 again, got %v", w)
	}
}

func TestUnlockAndClear(t *testing.T) {
	e := setup(t, Policy{Threshold: 5})
	for i := 0; i < 5; i++ {
		e.pw(t, "test@example.com", "bad", "1.1.1.1", 400)
	}
	e.pw(t, "test@example.com", "1234567890", "1.1.1.1", 400)

	ok, err := Unlock(e.app, "users", "Test@Example.com")
	if err != nil || !ok {
		t.Fatalf("unlock ok=%v err=%v", ok, err)
	}
	// the running process sees a CLI unlock at once
	e.pw(t, "test@example.com", "1234567890", "1.1.1.1", 401, `"mfaId":`)

	k := Key("users", "x@example.com")
	for i := 0; i < 5; i++ {
		e.m.failure("users", "x@example.com", k)
	}
	n, err := Clear(e.app)
	if err != nil || n != 1 {
		t.Fatalf("clear n=%d err=%v", n, err)
	}
	if w := e.m.lockedFor(k); w != 0 {
		t.Fatalf("still locked after clear: %v", w)
	}
}

func TestOffDoesNotLock(t *testing.T) {
	t.Setenv("TOKI_LOCKOUT", "off")
	if Enabled() {
		t.Fatal("expected disabled")
	}
	// tokibase.go does not Register when disabled
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)
	h := buildMux(t, app)
	url := "/api/collections/users/auth-with-password"
	for i := 0; i < 8; i++ {
		post(t, h, url, `{"identity":"test@example.com","password":"bad"}`, "1.1.1.1", 400)
	}
	post(t, h, url, `{"identity":"test@example.com","password":"1234567890"}`, "1.1.1.1", 401, `"mfaId":`)
}

func TestOTPLock(t *testing.T) {
	e := setup(t, Policy{Threshold: 3})
	id := strings.Repeat("a", 15)
	col, err := e.app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatal(err)
	}
	col.MFA.Enabled = false // a valid OTP then completes the login (200)
	if err := e.app.Save(col); err != nil {
		t.Fatal(err)
	}
	user, err := e.app.FindAuthRecordByEmail("users", "test@example.com")
	if err != nil {
		t.Fatal(err)
	}
	otp := core.NewOTP(e.app)
	otp.Id = id
	otp.SetCollectionRef(user.Collection().Id)
	otp.SetRecordRef(user.Id)
	otp.SetPassword("1234567890")
	if err := e.app.Save(otp); err != nil {
		t.Fatal(err)
	}
	do := func(pw string, status int, content ...string) *http.Response {
		return post(t, e.mux, "/api/collections/users/auth-with-otp",
			`{"otpId":"`+id+`","password":"`+pw+`"}`, "2.2.2.2", status, content...)
	}
	for i := 0; i < 3; i++ {
		do("000000", 400, "Invalid or expired OTP")
	}
	res := do("1234567890", 400, "Invalid or expired OTP")
	if res.Header.Get("Retry-After") != "60" {
		t.Fatalf("Retry-After=%q", res.Header.Get("Retry-After"))
	}
	e.now = e.now.Add(61 * time.Second)
	do("1234567890", 200, `"token":`)
}

func TestRecordFailureExported(t *testing.T) {
	e := setup(t, Policy{Threshold: 3})
	for i := 0; i < 3; i++ {
		RecordFailure("clients", "RecordID1")
	}
	if wait := e.m.lockedFor(Key("clients", "recordid1")); wait <= 0 {
		t.Fatal("exported RecordFailure must count towards the lock")
	}
}

// Passkey integration: IdentityKey/IsLocked/RecordFailureFor share the key of
// the password flow, so passkey failures and password failures add up.
func TestIdentityKeySharedWithPasswordFlow(t *testing.T) {
	e := setup(t, Policy{Threshold: 3})
	rec, err := e.app.FindAuthRecordByEmail("users", "test@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if IdentityKey("users", rec) != Key("users", "TEST@example.com ") {
		t.Fatalf("key %q", IdentityKey("users", rec))
	}
	if IsLocked("users", rec) {
		t.Fatal("not locked yet")
	}
	e.pw(t, "test@example.com", "wrong", "1.1.1.1", 400)
	RecordFailureFor("users", rec)
	RecordFailureFor("users", rec) // 3rd failure overall
	if !IsLocked("users", rec) {
		t.Fatal("mixed failures must lock")
	}
	e.pw(t, "test@example.com", "1234567890", "1.1.1.1", 400) // locked: even the right password
}
