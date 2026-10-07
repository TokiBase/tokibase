package audit_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/audit"
	"github.com/tokibase/tokibase/tests"
)

func newApp(t *testing.T) *tests.TestApp {
	t.Helper()
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)
	audit.Register(app)
	return app
}

var muxes sync.Map // *tests.TestApp -> http.Handler

func do(t *testing.T, app *tests.TestApp, method, url, body, token string) *http.Response {
	t.Helper()
	h, ok := muxes.Load(app)
	if !ok {
		router, err := apis.NewRouter(app)
		if err != nil {
			t.Fatal(err)
		}
		se := &core.ServeEvent{App: app, Router: router}
		err = app.OnServe().Trigger(se, func(e *core.ServeEvent) error {
			mux, err := e.Router.BuildMux()
			if err != nil {
				return err
			}
			h = http.Handler(mux)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		muxes.Store(app, h)
	}
	req := httptest.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("content-type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	rec := httptest.NewRecorder()
	h.(http.Handler).ServeHTTP(rec, req)
	return rec.Result()
}

func tokenFor(t *testing.T, app *tests.TestApp, col, email string) (string, *core.Record) {
	t.Helper()
	r, err := app.FindAuthRecordByEmail(col, email)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := r.NewAuthToken()
	if err != nil {
		t.Fatal(err)
	}
	return tok, r
}

func rows(t *testing.T, app *tests.TestApp) []audit.Entry {
	t.Helper()
	r, err := audit.Query(app, time.Time{}, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestSuperuserRecordUpdateLogged(t *testing.T) {
	app := newApp(t)
	tok, _ := tokenFor(t, app, core.CollectionNameSuperusers, "test@example.com")
	user, err := app.FindAuthRecordByEmail("users", "test@example.com")
	if err != nil {
		t.Fatal(err)
	}

	res := do(t, app, "PATCH", "/api/collections/users/records/"+user.Id, `{"name":"audited-name","password":"newpass12345","passwordConfirm":"newpass12345"}`, tok)
	if res.StatusCode != 200 {
		t.Fatalf("status %d", res.StatusCode)
	}

	got := rows(t, app)
	if len(got) != 1 {
		t.Fatalf("expected 1 row, got %d", len(got))
	}
	e := got[0]
	if e.Action != audit.ActionRecordUpdate || e.ActorKind != audit.ActorSuperuser || e.Collection != "users" || e.Record != user.Id || e.Seq != 1 || e.PrevHash != "" {
		t.Fatalf("unexpected row: %+v", e)
	}
	var diff map[string]map[string]any
	if err := json.Unmarshal([]byte(*e.Diff), &diff); err != nil {
		t.Fatal(err)
	}
	if diff["name"]["new"] != "audited-name" {
		t.Fatalf("bad diff: %v", diff)
	}
	if _, ok := diff["password"]; !ok {
		t.Fatalf("password change must appear (redacted) in diff: %v", diff)
	}

	// secrets stripped everywhere
	all := *e.Before + *e.After + *e.Diff
	for _, bad := range []string{"newpass12345", user.GetString("password"), user.TokenKey()} {
		if bad != "" && strings.Contains(all, bad) {
			t.Fatalf("secret leaked: %q", bad)
		}
	}
	for _, key := range []string{`"password"`, `"tokenKey"`} {
		if strings.Contains(*e.Before, key) || strings.Contains(*e.After, key) {
			t.Fatalf("%s must be stripped from before/after", key)
		}
	}
	if res, _ := audit.Verify(app); !res.OK {
		t.Fatalf("verify failed: %+v", res)
	}
}

func TestUserRecordUpdateNotLogged(t *testing.T) {
	app := newApp(t)
	tok, user := tokenFor(t, app, "users", "test@example.com")
	res := do(t, app, "PATCH", "/api/collections/users/records/"+user.Id, `{"name":"mine"}`, tok)
	if res.StatusCode != 200 {
		t.Fatalf("status %d", res.StatusCode)
	}
	if n := len(rows(t, app)); n != 0 {
		t.Fatalf("expected 0 rows, got %d", n)
	}
}

func TestImpersonatedWriteAndImpersonationLogged(t *testing.T) {
	app := newApp(t)
	su, _ := tokenFor(t, app, core.CollectionNameSuperusers, "test@example.com")
	user, _ := app.FindAuthRecordByEmail("users", "test@example.com")

	res := do(t, app, "POST", "/api/collections/users/impersonate/"+user.Id, `{"duration":300}`, su)
	if res.StatusCode != 200 {
		t.Fatalf("impersonate status %d", res.StatusCode)
	}
	var body struct{ Token string }
	_ = json.NewDecoder(res.Body).Decode(&body)

	res = do(t, app, "PATCH", "/api/collections/users/records/"+user.Id, `{"name":"imp"}`, body.Token)
	if res.StatusCode != 200 {
		t.Fatalf("status %d", res.StatusCode)
	}

	got := rows(t, app)
	if len(got) != 2 || got[0].Action != audit.ActionAuthImpersonate || got[1].Action != audit.ActionRecordUpdate {
		t.Fatalf("unexpected rows: %+v", got)
	}
	if got[1].ActorKind != audit.ActorUser || got[1].ImpersonatedBy == nil {
		t.Fatalf("expected impersonated user actor: %+v", got[1])
	}
	if strings.Contains(*got[0].Request, body.Token) {
		t.Fatal("token leaked")
	}
}

func TestCollectionUpdateLogged(t *testing.T) {
	app := newApp(t)
	tok, _ := tokenFor(t, app, core.CollectionNameSuperusers, "test@example.com")
	res := do(t, app, "PATCH", "/api/collections/demo1", `{"listRule":"id != ''"}`, tok)
	if res.StatusCode != 200 {
		t.Fatalf("status %d", res.StatusCode)
	}
	got := rows(t, app)
	if len(got) != 1 || got[0].Action != audit.ActionCollectionUpdate || got[0].Collection != "demo1" {
		t.Fatalf("unexpected rows: %+v", got)
	}
	if !strings.Contains(*got[0].Diff, "listRule") {
		t.Fatalf("diff should mention listRule: %s", *got[0].Diff)
	}
}

func TestVerifyDetectsTampering(t *testing.T) {
	app := newApp(t)
	l := audit.New(app)
	for i := 0; i < 3; i++ {
		after := `{"n":` + string(rune('1'+i)) + `}`
		if err := l.Append(&audit.Entry{ActorKind: audit.ActorSystem, Action: audit.ActionBackupCreate, After: &after}); err != nil {
			t.Fatal(err)
		}
	}
	res, err := audit.Verify(app)
	if err != nil || !res.OK || res.Rows != 3 {
		t.Fatalf("expected valid chain of 3: %+v %v", res, err)
	}

	if _, err := app.AuxDB().NewQuery(`UPDATE _audit SET after='{"n":99}' WHERE seq=2`).Execute(); err != nil {
		t.Fatal(err)
	}
	res, _ = audit.Verify(app)
	if res.OK || res.BrokenSeq != 2 {
		t.Fatalf("expected break at seq 2: %+v", res)
	}

	// deleting a middle row is detected as a seq gap
	app.AuxDB().NewQuery(`UPDATE _audit SET after='{"n":2}' WHERE seq=2`).Execute()
	app.AuxDB().NewQuery(`DELETE FROM _audit WHERE seq=2`).Execute()
	res, _ = audit.Verify(app)
	if res.OK || res.BrokenSeq != 3 {
		t.Fatalf("expected gap detected at seq 3: %+v", res)
	}
}

func TestOversizedJSONTruncated(t *testing.T) {
	app := newApp(t)
	tok, _ := tokenFor(t, app, core.CollectionNameSuperusers, "test@example.com")
	big := strings.Repeat("x", 100<<10)
	res := do(t, app, "PATCH", "/api/collections/demo1", `{"listRule":"id != ''","viewRule":"id != '`+big+`'"}`, tok)
	_ = res
	for _, e := range rows(t, app) {
		for _, s := range []*string{e.Before, e.After, e.Diff} {
			if s != nil && len(*s) > 64<<10 {
				t.Fatalf("json column not capped: %d", len(*s))
			}
		}
	}
}

func TestSensitiveFieldsNeverStoredInAudit(t *testing.T) {
	app := newApp(t)
	col := core.NewBaseCollection("vault_items")
	col.Fields.Add(&core.TextField{Name: "label"}, &core.TextField{Name: "diagnote"})
	open := ""
	col.ListRule, col.ViewRule, col.CreateRule, col.UpdateRule, col.DeleteRule = &open, &open, &open, &open, &open
	if err := app.Save(col); err != nil {
		t.Fatal(err)
	}
	kernel.RegisterSensitiveField(col.Id, "diagnote")
	t.Cleanup(func() { kernel.UnregisterSensitiveField(col.Id, "diagnote") })
	tok, _ := tokenFor(t, app, core.CollectionNameSuperusers, "test@example.com")

	res := do(t, app, "POST", "/api/collections/vault_items/records", `{"label":"a","diagnote":"PLAINTEXT-ONE"}`, tok)
	if res.StatusCode != 200 {
		t.Fatalf("create %d", res.StatusCode)
	}
	var created map[string]any
	_ = json.NewDecoder(res.Body).Decode(&created)
	id := created["id"].(string)
	if res := do(t, app, "PATCH", "/api/collections/vault_items/records/"+id, `{"diagnote":"PLAINTEXT-TWO"}`, tok); res.StatusCode != 200 {
		t.Fatalf("update %d", res.StatusCode)
	}
	if res := do(t, app, "DELETE", "/api/collections/vault_items/records/"+id, ``, tok); res.StatusCode != 204 {
		t.Fatalf("delete %d", res.StatusCode)
	}
	n := 0
	for _, e := range rows(t, app) {
		if e.Collection != "vault_items" {
			continue
		}
		n++
		all := ""
		for _, p := range []*string{e.Before, e.After, e.Diff} {
			if p != nil {
				all += *p
			}
		}
		if strings.Contains(all, "PLAINTEXT") {
			t.Fatalf("%s leaked plaintext: %s", e.Action, all)
		}
		if e.Action != audit.ActionRecordDelete && !strings.Contains(all, "[encrypted]") && e.Action == audit.ActionRecordCreate {
			t.Fatalf("expected the marker in %s: %s", e.Action, all)
		}
	}
	if n != 3 {
		t.Fatalf("expected 3 audit rows, got %d", n)
	}
}
