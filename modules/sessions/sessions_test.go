package sessions

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tests"
	"github.com/tokibase/tokibase/tools/security"
)

type env struct {
	app *tests.TestApp
	m   *Module
	mux http.Handler
}

func setup(t *testing.T) *env {
	t.Helper()
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { kernel.OnAuthTokenIssue = nil; app.Cleanup() })
	m := Register(app)
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
	return &env{app: app, m: m, mux: h}
}

func (e *env) do(t *testing.T, method, url, body, token, device string, status int) map[string]any {
	t.Helper()
	req := httptest.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	if device != "" {
		req.Header.Set(DeviceHeader, device)
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

func (e *env) login(t *testing.T, device string) string {
	out := e.do(t, "POST", "/api/collections/clients/auth-with-password", `{"identity":"test@example.com","password":"1234567890"}`, "", device, 200)
	return out["token"].(string)
}

func sidOf(t *testing.T, token string) string {
	claims, err := security.ParseUnverifiedJWT(token)
	if err != nil {
		t.Fatal(err)
	}
	sid, _ := claims[ClaimSID].(string)
	if len(sid) != 16 {
		t.Fatalf("sid claim %q", sid)
	}
	return sid
}

func (e *env) session(t *testing.T, sid string) *Session {
	s, ok := e.m.byToken(sid)
	if !ok {
		t.Fatalf("no session row for %s", sid)
	}
	return s
}

const refresh = "/api/collections/clients/auth-refresh"

func TestLoginCreatesSession(t *testing.T) {
	e := setup(t)
	tok := e.login(t, "phone-1")
	s := e.session(t, sidOf(t, tok))
	if s.Kind != KindAuth || s.Device != "phone-1" || s.Revoked != "" || s.Collection == "" || s.Record == "" || s.Expires == "" {
		t.Fatalf("%+v", s)
	}
	e.do(t, "POST", refresh, "", tok, "", 200)
}

func TestRevokedSessionIs401(t *testing.T) {
	e := setup(t)
	tok := e.login(t, "")
	e.do(t, "POST", refresh, "", tok, "", 200)
	ok, err := Revoke(e.app, e.session(t, sidOf(t, tok)).Id, "test")
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	out := e.do(t, "POST", refresh, "", tok, "", 401)
	if out["message"] != "The request requires valid record authorization token." {
		t.Fatalf("%v", out)
	}
}

func TestPasswordChangeRevokesAll(t *testing.T) {
	e := setup(t)
	a, b := e.login(t, "a"), e.login(t, "b")
	rec, err := e.app.FindAuthRecordByEmail("clients", "test@example.com")
	if err != nil {
		t.Fatal(err)
	}
	rec.SetPassword("newpassword123")
	if err := e.app.Save(rec); err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{a, b} {
		if s := e.session(t, sidOf(t, tok)); s.Revoked == "" || s.RevokedReason != "password_changed" {
			t.Fatalf("%+v", s)
		}
	}
}

func TestEmailChangeRevokesAll(t *testing.T) {
	e := setup(t)
	a := e.login(t, "")
	rec, _ := e.app.FindAuthRecordByEmail("clients", "test@example.com")
	rec.SetEmail("changed@example.com")
	if err := e.app.Save(rec); err != nil {
		t.Fatal(err)
	}
	if s := e.session(t, sidOf(t, a)); s.RevokedReason != "email_changed" {
		t.Fatalf("%+v", s)
	}
}

func TestRotation(t *testing.T) {
	t.Setenv("TOKI_SESSIONS_ROTATE", "on")
	e := setup(t)
	var events []string
	SetAuditSink(func(action, c, r string, d map[string]any) { events = append(events, action) })
	t.Cleanup(func() { SetAuditSink(nil) })

	tok1 := e.login(t, "dev")
	out := e.do(t, "POST", refresh, "", tok1, "", 200)
	tok2 := out["token"].(string)
	sid1, sid2 := sidOf(t, tok1), sidOf(t, tok2)
	if sid1 == sid2 {
		t.Fatal("rotation must issue a new sid")
	}
	if s := e.session(t, sid1); s.RevokedReason != ReasonRotated {
		t.Fatalf("%+v", s)
	}
	if s := e.session(t, sid2); s.Kind != KindRefresh || s.Device != "dev" || s.Revoked != "" {
		t.Fatalf("%+v", s)
	}
	e.do(t, "POST", refresh, "", tok2, "", 200) // new token still works

	// reuse of the first token: 401 and the whole family is revoked
	e.do(t, "POST", refresh, "", tok1, "", 401)
	if s := e.session(t, sid2); s.Revoked == "" {
		t.Fatalf("family not revoked: %+v", s)
	}
	e.do(t, "POST", refresh, "", tok2, "", 401)
	if len(events) != 1 || events[0] != ActionReuse {
		t.Fatalf("events %v", events)
	}
}

func TestNoRotationByDefault(t *testing.T) {
	e := setup(t)
	tok1 := e.login(t, "")
	tok2 := e.do(t, "POST", refresh, "", tok1, "", 200)["token"].(string)
	if s := e.session(t, sidOf(t, tok1)); s.Revoked != "" {
		t.Fatalf("%+v", s)
	}
	e.do(t, "POST", refresh, "", tok1, "", 200)
	e.do(t, "POST", refresh, "", tok2, "", 200)
}

func TestRevokeAllAndPurge(t *testing.T) {
	e := setup(t)
	a := e.login(t, "")
	e.login(t, "")
	rec, _ := e.app.FindAuthRecordByEmail("clients", "test@example.com")
	n, err := RevokeUser(e.app, "clients", rec.Id, "cli")
	if err != nil || n != 2 {
		t.Fatal(n, err)
	}
	e.do(t, "POST", refresh, "", a, "", 401)
	rows, err := List(e.app, "clients", rec.Id)
	if err != nil || len(rows) != 2 {
		t.Fatal(rows, err)
	}
	if _, err := e.app.DB().NewQuery("UPDATE {{_sessions}} SET expires='2000-01-01 00:00:00.000Z'").Execute(); err != nil {
		t.Fatal(err)
	}
	if n, err := PurgeExpired(e.app); err != nil || n != 2 {
		t.Fatal(n, err)
	}
}

func TestOffByEnv(t *testing.T) {
	t.Setenv("TOKI_SESSIONS", "off")
	if Enabled() {
		t.Fatal("expected disabled")
	}
	// without Register no seam is installed: tokens carry no sid
	kernel.OnAuthTokenIssue = nil
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()
	rec, _ := app.FindAuthRecordByEmail("clients", "test@example.com")
	tok, _ := rec.NewAuthToken()
	if claims, _ := security.ParseUnverifiedJWT(tok); claims[ClaimSID] != nil {
		t.Fatal("sid claim must be absent")
	}
}

func TestTokenWithoutSidStillValid(t *testing.T) {
	e := setup(t)
	kernel.OnAuthTokenIssue = nil
	rec, _ := e.app.FindAuthRecordByEmail("clients", "test@example.com")
	tok, _ := rec.NewAuthToken()
	e.do(t, "POST", refresh, "", tok, "", 200)
}
