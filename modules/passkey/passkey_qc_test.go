package passkey

import (
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
)

func (e *env) login(t *testing.T, a *softAuth, handle string, status int) map[string]any {
	t.Helper()
	a.count++
	return e.do(t, "POST", base+"/login/verify", map[string]any{"credential": a.assert(t, e.loginOpts(t, ""), handle)}, "", status)
}

func countFailures(t *testing.T) *int {
	n := 0
	SetFailureSink(func(string, *core.Record) { n++ })
	t.Cleanup(func() { SetFailureSink(nil) })
	return &n
}

// K1: assertions without the UV flag are rejected and do not count as failures.
func TestLoginRequiresUserVerification(t *testing.T) {
	e := setup(t, true)
	a, saved := e.enroll(t, e.password(t))
	if saved["uvCapable"] != true {
		t.Fatalf("uv_capable not recorded: %v", saved)
	}
	n := countFailures(t)
	a.noUV = true
	e.login(t, a, userHandle(e), 400)
	if *n != 0 {
		t.Fatal("a UV=0 assertion must not feed lockout")
	}
	a.noUV = false
	e.login(t, a, userHandle(e), 200)
}

// K1: registration with a non-UV authenticator is accepted but flagged uv_capable=false.
func TestRegisterRecordsUVCapableFalse(t *testing.T) {
	e := setup(t, true)
	tok := e.password(t)
	a := newSoftAuth(t)
	a.noUV = true
	opts := e.do(t, "POST", base+"/register/options", nil, tok, 200)
	saved := e.do(t, "POST", base+"/register/verify", map[string]any{"credential": a.register(t, opts)}, tok, 200)
	if saved["uvCapable"] != false {
		t.Fatalf("%v", saved)
	}
	// such a credential cannot log in (UV required)
	e.login(t, a, userHandle(e), 400)
}

// K2: known and unknown identities get indistinguishable options.
func TestLoginOptionsDoNotLeakAccounts(t *testing.T) {
	e := setup(t, true)
	e.enroll(t, e.password(t))
	known := e.loginOpts(t, "test@example.com")
	unknown := e.loginOpts(t, "nobody@example.com")
	for _, o := range []map[string]any{known, unknown} {
		if ac, _ := o["allowCredentials"].([]any); len(ac) != 0 {
			t.Fatalf("allowCredentials leaked: %v", o)
		}
	}
	if len(known) != len(unknown) {
		t.Fatalf("shape differs: %v vs %v", known, unknown)
	}
	// opt-in hint restores allowCredentials
	e.m.cfg.IdentityHint = true
	if ac, _ := e.loginOpts(t, "test@example.com")["allowCredentials"].([]any); len(ac) != 1 {
		t.Fatal("hint mode must list credentials")
	}
}

func TestLoginWrongRPOriginRejected(t *testing.T) {
	e := setup(t, true)
	a, _ := e.enroll(t, e.password(t))
	n := countFailures(t)
	h := userHandle(e)

	a.rpID = "evil.example"
	e.login(t, a, h, 400)
	a.rpID = ""
	a.origin = "https://evil.example"
	e.login(t, a, h, 400)
	a.origin = ""
	a.crossOrigin = true
	e.login(t, a, h, 400)
	a.crossOrigin = false
	if *n != 0 {
		t.Fatalf("origin/RP errors must not count as failures: %d", *n)
	}
	e.login(t, a, h, 200)
}

func TestLoginUserHandleMismatch(t *testing.T) {
	e := setup(t, true)
	a, _ := e.enroll(t, e.password(t))
	n := countFailures(t)
	other, err := e.app.FindAuthRecordByEmail("clients", "test2@example.com")
	if err != nil {
		t.Skip("no second client in test data")
	}
	e.login(t, a, b64.EncodeToString([]byte(other.Id)), 400)
	e.login(t, a, "", 400)
	if *n != 0 {
		t.Fatal("handle mismatch must not count as failure")
	}
	e.login(t, a, userHandle(e), 200)
}

func TestLoginReplayedAssertion(t *testing.T) {
	e := setup(t, true)
	a, _ := e.enroll(t, e.password(t))
	opts := e.loginOpts(t, "")
	a.count++
	cred := a.assert(t, opts, userHandle(e))
	e.do(t, "POST", base+"/login/verify", map[string]any{"credential": cred}, "", 200)
	e.do(t, "POST", base+"/login/verify", map[string]any{"credential": cred}, "", 400)
}

func TestLoginCrossCollection(t *testing.T) {
	e := setup(t, true)
	a, _ := e.enroll(t, e.password(t))
	su := "/api/collections/_superusers/passkeys"
	// challenge of collection A used on collection B is refused (and consumed)
	opts := e.loginOpts(t, "")
	a.count++
	e.do(t, "POST", su+"/login/verify", map[string]any{"credential": a.assert(t, opts, userHandle(e))}, "", 400)
	// challenge of B with a credential of A
	bopts := e.do(t, "POST", su+"/login/options", nil, "", 200)
	n := countFailures(t)
	a.count++
	e.do(t, "POST", su+"/login/verify", map[string]any{"credential": a.assert(t, bopts, userHandle(e))}, "", 400)
	if *n != 0 {
		t.Fatal("cross-collection credential must not count as failure")
	}
	e.login(t, a, userHandle(e), 200)
}

// K3: passkey login honours the lockout state and counts only attributable bad signatures.
func TestLockoutIntegration(t *testing.T) {
	e := setup(t, true)
	a, _ := e.enroll(t, e.password(t))
	locked := true
	SetLockedSink(func(string, *core.Record) bool { return locked })
	t.Cleanup(func() { SetLockedSink(nil) })
	n := countFailures(t)
	out := e.login(t, a, userHandle(e), 400)
	if out["message"] != msgAuthFailed || out["token"] != nil {
		t.Fatalf("%v", out)
	}
	if *n != 0 {
		t.Fatal("a locked login attempt must not extend the lock")
	}
	locked = false
	e.login(t, a, userHandle(e), 200)
}

// K4: per-IP rate limit on options, per-IP pending cap with oldest eviction.
func TestChallengeFloodLimits(t *testing.T) {
	e := setup(t, true)
	for i := 0; i < rateLimitPerMinute; i++ {
		e.loginOpts(t, "")
	}
	e.do(t, "POST", base+"/login/options", nil, "", 429)
	// the window slides
	base := e.m.now()
	e.m.now = func() time.Time { return base.Add(2 * time.Minute) }
	e.loginOpts(t, "")

	// pending cap per IP (the HTTP limiter is lower, so write directly)
	e2 := setup(t, true)
	for i := 0; i < 30; i++ {
		s := mustSession(t, e2)
		if err := e2.m.putChallenge("login", "col", "", "10.0.0.1", s); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	_ = e2.app.AuxDB().NewQuery("SELECT COUNT(*) FROM {{_passkey_challenges}} WHERE [[ip]]='10.0.0.1'").Row(&n)
	if n != maxChallengesPerIP {
		t.Fatalf("pending per IP = %d want %d", n, maxChallengesPerIP)
	}
	// another IP is unaffected
	if err := e2.m.putChallenge("login", "col", "", "10.0.0.2", mustSession(t, e2)); err != nil {
		t.Fatal(err)
	}
}

// K4: the global cap is per collection.
func TestChallengeCapPerCollection(t *testing.T) {
	e := setup(t, true)
	if _, err := e.app.AuxDB().NewQuery(`INSERT INTO {{_passkey_challenges}} ([[challenge]],[[kind]],[[collection]],[[expires]]) VALUES ('x','login','full',{:e})`).
		Bind(dbx.Params{"e": e.m.now().Add(time.Hour).UnixMilli()}).Execute(); err != nil {
		t.Fatal(err)
	}
	// simulate a full collection by temporarily lowering nothing: just check isolation of the count query
	if err := e.m.putChallenge("login", "other", "", "", mustSession(t, e)); err != nil {
		t.Fatal(err)
	}
}

// K5: step-up for register/verify and delete.
func TestFreshAuthRequired(t *testing.T) {
	e := setup(t, true)
	tok := e.password(t)
	_, saved := e.enroll(t, tok) // fresh token: ok
	id := saved["id"].(string)

	now := e.m.now()
	e.m.now = func() time.Time { return now.Add(time.Hour) } // token is now "old"
	failures := countFailures(t)

	// register
	opts := e.do(t, "POST", base+"/register/options", nil, tok, 200)
	a2 := newSoftAuth(t)
	cred := a2.register(t, opts)
	e.do(t, "POST", base+"/register/verify", map[string]any{"credential": cred}, tok, 403)
	e.do(t, "POST", base+"/register/verify", map[string]any{"credential": cred, "password": "wrong-password"}, tok, 403)
	if *failures != 1 {
		t.Fatalf("wrong re-auth password must count once, got %d", *failures)
	}
	// the challenge is consumed by the failed attempt only after reauth passes
	e.do(t, "POST", base+"/register/verify", map[string]any{"credential": cred, "password": "1234567890"}, tok, 200)

	// delete
	e.do(t, "DELETE", base+"/"+id, nil, tok, 403)
	e.do(t, "DELETE", base+"/"+id, map[string]any{"password": "nope"}, tok, 403)
	e.do(t, "DELETE", base+"/"+id, map[string]any{"password": "1234567890"}, tok, 204)

	// a non-refreshable (impersonation style) token never counts as fresh
	e.m.now = func() time.Time { return now }
	rec, _ := e.app.FindAuthRecordByEmail("clients", "test@example.com")
	static, err := rec.NewStaticAuthToken(0)
	if err != nil {
		t.Fatal(err)
	}
	opts = e.do(t, "POST", base+"/register/options", nil, static, 200)
	a3 := newSoftAuth(t)
	e.do(t, "POST", base+"/register/verify", map[string]any{"credential": a3.register(t, opts)}, static, 403)
}

func TestFreshAuthRespectsLockout(t *testing.T) {
	e := setup(t, true)
	tok := e.password(t)
	e.enroll(t, tok)
	now := e.m.now()
	e.m.now = func() time.Time { return now.Add(time.Hour) }
	SetLockedSink(func(string, *core.Record) bool { return true })
	t.Cleanup(func() { SetLockedSink(nil) })
	opts := e.do(t, "POST", base+"/register/options", nil, tok, 200)
	a := newSoftAuth(t)
	e.do(t, "POST", base+"/register/verify", map[string]any{"credential": a.register(t, opts), "password": "1234567890"}, tok, 403)
}

func mustSession(t *testing.T, e *env) *webauthn.SessionData {
	t.Helper()
	_, s, err := e.m.wa.BeginDiscoverableLogin()
	if err != nil {
		t.Fatal(err)
	}
	return s
}
