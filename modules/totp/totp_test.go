//go:build !no_totp

package totp

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tests"
)

const base = "/api/collections/clients"

type env struct {
	app   *tests.TestApp
	m     *Module
	mux   http.Handler
	clock time.Time
	fails int
	audit []string
}

func setup(t *testing.T) *env {
	t.Helper()
	t.Setenv(EnvKey, base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	t.Setenv(EnvRequiredRoles, "")
	t.Setenv(EnvRequireSuper, "")
	t.Setenv(EnvGraceDays, "")
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	e := &env{app: app, clock: time.Now().Truncate(30 * time.Second)}
	t.Cleanup(func() { SetFailureSink(nil); SetLockedSink(nil); SetAuditSink(nil); app.Cleanup() })
	SetFailureSink(func(string, *core.Record) { e.fails++ })
	SetAuditSink(func(a, _, _ string, _ map[string]any) { e.audit = append(e.audit, a) })
	e.m = Register(app)
	e.m.now = func() time.Time { return e.clock }

	col, _ := app.FindCollectionByNameOrId("clients")
	col.OTP.Enabled = true // upstream needs two methods for MFA
	col.MFA.Enabled = true
	col.MFA.Rule = ""
	col.Fields.Add(&kernel.TextField{Name: "role"})
	if err := app.Save(col); err != nil {
		t.Fatal(err)
	}
	router, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.OnServe().Trigger(&core.ServeEvent{App: app, Router: router}, func(se *core.ServeEvent) error {
		mux, err := se.Router.BuildMux()
		e.mux = mux
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) do(t *testing.T, method, url string, body any, token string, status int) map[string]any {
	t.Helper()
	b, _ := json.Marshal(body)
	if body == nil {
		b = nil
	}
	req := httptest.NewRequest(method, url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	if rec.Code != status {
		t.Fatalf("%s %s: status %d want %d: %s", method, url, rec.Code, status, rec.Body.String())
	}
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out
}

// pwToken returns a fresh session token (as a completed login would give).
func (e *env) pwToken(t *testing.T, email string) string {
	tok, err := mustRec(t, e, email).NewAuthToken()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func (e *env) code(secret string) string {
	s, _ := b32.DecodeString(secret)
	return Code(s, e.clock.Unix())
}

// enrol sets TOTP up for email and returns the secret and recovery codes.
func (e *env) enrol(t *testing.T, email string) (string, []any) {
	tok := e.pwToken(t, email)
	out := e.do(t, "POST", base+"/totp/setup", nil, tok, 200)
	secret := out["secret"].(string)
	if !strings.HasPrefix(out["otpauth_url"].(string), "otpauth://totp/") || !strings.Contains(out["qr_svg"].(string), "<svg") {
		t.Fatalf("setup: %v", out)
	}
	codes := out["recovery_codes"].([]any)
	if len(codes) != 10 || len(codes[0].(string)) != 10 {
		t.Fatalf("recovery codes: %v", codes)
	}
	e.do(t, "POST", base+"/totp/confirm", map[string]any{"code": "000000"}, tok, 400)
	e.do(t, "POST", base+"/totp/confirm", map[string]any{"code": e.code(secret)}, tok, 200)
	return secret, codes
}

func (e *env) mfaStart(t *testing.T, email string) string {
	out := e.do(t, "POST", base+"/auth-with-password", map[string]any{"identity": email, "password": "1234567890"}, "", 401)
	id, _ := out["mfaId"].(string)
	if id == "" {
		t.Fatalf("no mfaId: %v", out)
	}
	return id
}

func TestRFC6238Vectors(t *testing.T) {
	secret := []byte("12345678901234567890")
	// RFC 6238 appendix B (SHA-1), last 6 of the 8-digit codes
	for ts, want := range map[int64]string{59: "287082", 1111111109: "081804", 1234567890: "005924", 2000000000: "279037"} {
		if got := Code(secret, ts); got != want {
			t.Fatalf("t=%d got %s want %s", ts, got, want)
		}
	}
	if _, ok := match(secret, "287082", 59+30); !ok {
		t.Fatal("window +-1 must accept")
	}
	if _, ok := match(secret, "287082", 59+90); ok {
		t.Fatal("outside window must reject")
	}
}

func TestFullFlow(t *testing.T) {
	e := setup(t)
	secret, _ := e.enrol(t, "test@example.com")
	e.clock = e.clock.Add(30 * time.Second) // confirm consumed the previous step
	id := e.mfaStart(t, "test@example.com")
	out := e.do(t, "POST", base+"/auth-with-totp", map[string]any{"mfaId": id, "code": e.code(secret)}, "", 200)
	if out["token"] == "" || out["record"] == nil {
		t.Fatalf("%v", out)
	}
	// mfa record is consumed
	e.do(t, "POST", base+"/auth-with-totp", map[string]any{"mfaId": id, "code": e.code(secret)}, "", 400)
	if !contains(e.audit, ActionEnabled) {
		t.Fatalf("audit %v", e.audit)
	}
	// secret is stored encrypted
	row := findRow(e.app, mustRec(t, e, "test@example.com"))
	if strings.Contains(row.GetString("secret"), secret) || !strings.HasPrefix(row.GetString("secret"), "v1:") {
		t.Fatal("secret not encrypted")
	}
}

func TestWrongCodeAndRateLimit(t *testing.T) {
	e := setup(t)
	secret, _ := e.enrol(t, "test@example.com")
	e.clock = e.clock.Add(30 * time.Second)
	id := e.mfaStart(t, "test@example.com")
	for i := 0; i < 5; i++ {
		e.do(t, "POST", base+"/auth-with-totp", map[string]any{"mfaId": id, "code": "123456"}, "", 400)
	}
	if e.fails != 5 {
		t.Fatalf("lockout failures %d", e.fails)
	}
	// 6th attempt (even valid) is rate limited
	e.do(t, "POST", base+"/auth-with-totp", map[string]any{"mfaId": id, "code": e.code(secret)}, "", 429)
}

func TestReuseRejected(t *testing.T) {
	e := setup(t)
	secret, _ := e.enrol(t, "test@example.com")
	// same step as the confirm code
	id := e.mfaStart(t, "test@example.com")
	e.do(t, "POST", base+"/auth-with-totp", map[string]any{"mfaId": id, "code": e.code(secret)}, "", 400)
	e.clock = e.clock.Add(30 * time.Second)
	e.do(t, "POST", base+"/auth-with-totp", map[string]any{"mfaId": id, "code": e.code(secret)}, "", 200)
	// the code just used cannot start another login either
	id2 := e.mfaStart(t, "test@example.com")
	e.do(t, "POST", base+"/auth-with-totp", map[string]any{"mfaId": id2, "code": e.code(secret)}, "", 400)
}

func TestRecoveryCodeSingleUse(t *testing.T) {
	e := setup(t)
	_, codes := e.enrol(t, "test@example.com")
	rc := codes[0].(string)
	id := e.mfaStart(t, "test@example.com")
	e.do(t, "POST", base+"/auth-with-totp", map[string]any{"mfaId": id, "code": rc}, "", 200)
	id2 := e.mfaStart(t, "test@example.com")
	e.do(t, "POST", base+"/auth-with-totp", map[string]any{"mfaId": id2, "code": rc}, "", 400)
	e.do(t, "POST", base+"/auth-with-totp", map[string]any{"mfaId": id2, "code": strings.ToUpper(codes[1].(string))}, "", 200)
	if !contains(e.audit, ActionRecoveryUse) {
		t.Fatalf("audit %v", e.audit)
	}
	row := findRow(e.app, mustRec(t, e, "test@example.com"))
	if n := len(recoveryHashes(row)); n != 8 {
		t.Fatalf("hashes left %d", n)
	}
	if strings.Contains(row.GetString("recovery_codes"), rc) {
		t.Fatal("plain recovery code stored")
	}
	// regenerate replaces them
	tok := e.pwToken(t, "test@example.com")
	out := e.do(t, "POST", base+"/totp/recovery/regenerate", nil, tok, 200)
	if len(out["recovery_codes"].([]any)) != 10 {
		t.Fatal(out)
	}
	id3 := e.mfaStart(t, "test@example.com")
	e.do(t, "POST", base+"/auth-with-totp", map[string]any{"mfaId": id3, "code": codes[2].(string)}, "", 400)
}

func TestDisableNeedsFreshAuth(t *testing.T) {
	e := setup(t)
	e.enrol(t, "test@example.com")
	tok := e.pwToken(t, "test@example.com")
	e.clock = e.clock.Add(11 * time.Minute) // token is now stale
	e.do(t, "DELETE", base+"/totp", nil, tok, 403)
	e.do(t, "DELETE", base+"/totp", map[string]any{"password": "wrong-password"}, tok, 403)
	e.do(t, "POST", base+"/totp/setup", nil, tok, 403)
	e.do(t, "DELETE", base+"/totp", nil, "", 401)
	e.do(t, "DELETE", base+"/totp", map[string]any{"password": "1234567890"}, tok, 204)
	if findRow(e.app, mustRec(t, e, "test@example.com")) != nil {
		t.Fatal("row still present")
	}
	if !contains(e.audit, ActionDisabled) {
		t.Fatalf("audit %v", e.audit)
	}
}

func TestEnforcementBlocksRole(t *testing.T) {
	e := setup(t)
	t.Setenv(EnvRequiredRoles, "admin,staff")
	rec := mustRec(t, e, "test@example.com")
	rec.Set("role", "admin")
	if err := e.app.Save(rec); err != nil {
		t.Fatal(err)
	}
	out := e.do(t, "POST", base+"/auth-with-password", map[string]any{"identity": "test@example.com", "password": "1234567890"}, "", 403)
	d, _ := out["data"].(map[string]any)
	if x, _ := d["totp"].(map[string]any); x["code"] != CodeRequired {
		t.Fatalf("%v", out)
	}
	// grace window lets the user in to enrol
	t.Setenv(EnvGraceDays, "7")
	setParam(e.app, ParamEnforcedAt, `"`+e.clock.Format(time.RFC3339)+`"`)
	tok := e.pwToken(t, "test@example.com")
	e.do(t, "POST", base+"/totp/setup", nil, tok, 200)
	// grace over
	e.clock = e.clock.AddDate(0, 0, 8)
	e.do(t, "POST", base+"/auth-with-password", map[string]any{"identity": "test@example.com", "password": "1234567890"}, "", 403)
	// other roles are not affected
	rec.Set("role", "member")
	_ = e.app.Save(rec)
	e.mfaStart(t, "test@example.com") // normal MFA step, not 403
}

func TestEnforcementPassesWithTotp(t *testing.T) {
	e := setup(t)
	secret, _ := e.enrol(t, "test@example.com")
	t.Setenv(EnvRequiredRoles, "admin")
	rec := mustRec(t, e, "test@example.com")
	rec.Set("role", "admin")
	_ = e.app.Save(rec)
	e.clock = e.clock.Add(30 * time.Second)
	id := e.mfaStart(t, "test@example.com")
	e.do(t, "POST", base+"/auth-with-totp", map[string]any{"mfaId": id, "code": e.code(secret)}, "", 200)
}

func TestEnforcementParamsFromCLI(t *testing.T) {
	e := setup(t)
	SetEnforcement(e.app, Enforcement{Roles: []string{"admin"}, Superusers: true}, false)
	cfg := loadEnforcement(e.app)
	if !cfg.Superusers || len(cfg.Roles) != 1 {
		t.Fatalf("%+v", cfg)
	}
	if _, ok := EnforcedAt(e.app); !ok {
		t.Fatal("enforced_at not set")
	}
	if !contains(e.audit, ActionEnforce) {
		t.Fatalf("audit %v", e.audit)
	}
	su, _ := e.app.FindAuthRecordByEmail(core.CollectionNameSuperusers, "test@example.com")
	if su != nil && !cfg.matches(su) {
		t.Fatal("superuser must match")
	}
}

func TestMfaIdOfOtherUserRejected(t *testing.T) {
	e := setup(t)
	secretA, _ := e.enrol(t, "test@example.com")
	// a second user with their own TOTP
	col, _ := e.app.FindCollectionByNameOrId("clients")
	b := core.NewRecord(col)
	b.SetEmail("other@example.com")
	b.SetPassword("1234567890")
	b.SetVerified(true)
	if err := e.app.Save(b); err != nil {
		t.Fatal(err)
	}
	secretB, _ := e.enrol(t, "other@example.com")
	e.clock = e.clock.Add(30 * time.Second)
	idA := e.mfaStart(t, "test@example.com")
	// B's valid code cannot complete A's challenge
	e.do(t, "POST", base+"/auth-with-totp", map[string]any{"mfaId": idA, "code": e.code(secretB)}, "", 400)
	// the challenge of a different collection is rejected
	other, _ := e.app.FindCollectionByNameOrId(core.CollectionNameSuperusers)
	mfa := core.NewMFA(e.app)
	mfa.SetCollectionRef(other.Id)
	sus, _ := e.app.FindAllRecords(core.CollectionNameSuperusers)
	if len(sus) == 0 {
		t.Skip("no superuser in test data")
	}
	mfa.SetRecordRef(sus[0].Id)
	mfa.SetMethod("password")
	if err := e.app.Save(mfa); err != nil {
		t.Fatal(err)
	}
	e.do(t, "POST", base+"/auth-with-totp", map[string]any{"mfaId": mfa.Id, "code": e.code(secretA)}, "", 400)
	e.do(t, "POST", base+"/auth-with-totp", map[string]any{"mfaId": "nope", "code": e.code(secretA)}, "", 400)
	e.do(t, "POST", base+"/auth-with-totp", map[string]any{"mfaId": idA, "code": e.code(secretA)}, "", 200)
}

func TestExpiredMfaIdRejected(t *testing.T) {
	e := setup(t)
	secret, _ := e.enrol(t, "test@example.com")
	e.clock = e.clock.Add(30 * time.Second)
	id := e.mfaStart(t, "test@example.com")
	past := time.Now().Add(-time.Hour).UTC().Format("2006-01-02 15:04:05.000Z")
	if _, err := e.app.DB().NewQuery("UPDATE {{_mfas}} SET [[created]]={:t} WHERE [[id]]={:id}").Bind(dbx.Params{"t": past, "id": id}).Execute(); err != nil {
		t.Fatal(err)
	}
	e.do(t, "POST", base+"/auth-with-totp", map[string]any{"mfaId": id, "code": e.code(secret)}, "", 400)
}

func TestNoKeyRefusesSetup(t *testing.T) {
	e := setup(t)
	t.Setenv(EnvKey, "")
	tok := e.pwToken(t, "test@example.com")
	e.do(t, "POST", base+"/totp/setup", nil, tok, 503)
}

func TestCannotStartWithoutAuthOrOnOtherCollection(t *testing.T) {
	e := setup(t)
	e.do(t, "POST", base+"/totp/setup", nil, "", 401)
	e.do(t, "POST", "/api/collections/demo1/totp/setup", nil, "", 404)
}

func mustRec(t *testing.T, e *env, email string) *core.Record {
	r, err := e.app.FindAuthRecordByEmail("clients", email)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
