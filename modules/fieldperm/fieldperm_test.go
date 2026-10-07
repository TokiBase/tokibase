//go:build !no_fieldperm

package fieldperm

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/tests"
)

type env struct {
	app   *tests.TestApp
	m     *Module
	mux   http.Handler
	owner *core.Record
	other *core.Record
	su    *core.Record
}

func s(v string) *string { return &v }

func rf(m *Module, c *core.Collection) map[string]*Rule {
	r, _ := m.rulesFor(c)
	return r
}

func buildMux(t *testing.T, app *tests.TestApp) http.Handler {
	t.Helper()
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

func setup(t *testing.T) *env {
	t.Helper()
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)
	m := Register(app)

	c := core.NewBaseCollection("clans")
	c.Fields.Add(
		&core.TextField{Name: "title"},
		&core.TextField{Name: "secret"},
		&core.TextField{Name: "leader"},
	)
	open := ""
	c.ListRule, c.ViewRule, c.CreateRule, c.UpdateRule = &open, &open, &open, &open
	if err := app.Save(c); err != nil {
		t.Fatal(err)
	}

	e := &env{app: app, m: m}
	e.owner, err = app.FindAuthRecordByEmail("users", "test@example.com")
	if err != nil {
		t.Fatal(err)
	}
	e.other, err = app.FindAuthRecordByEmail("users", "test2@example.com")
	if err != nil {
		t.Fatal(err)
	}
	e.su, err = app.FindAuthRecordByEmail(core.CollectionNameSuperusers, "test@example.com")
	if err != nil {
		t.Fatal(err)
	}
	e.mux = buildMux(t, app)
	return e
}

func (e *env) do(t *testing.T, who *core.Record, method, url, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if who != nil {
		tok, err := who.NewAuthToken()
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", tok)
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func (e *env) seed(t *testing.T) *core.Record {
	t.Helper()
	col, _ := e.app.FindCollectionByNameOrId("clans")
	r := core.NewRecord(col)
	r.Set("title", "alpha")
	r.Set("secret", "s3cret")
	r.Set("leader", e.owner.Id)
	if err := e.app.Save(r); err != nil {
		t.Fatal(err)
	}
	return r
}

func (e *env) rule(t *testing.T, field string, o SetOptions) {
	t.Helper()
	if _, err := Set(e.app, "clans", field, o); err != nil {
		t.Fatal(err)
	}
}

func TestReadRuleHidesField(t *testing.T) {
	e := setup(t)
	r := e.seed(t)
	e.rule(t, "secret", SetOptions{ReadSet: true, Read: s("leader = @request.auth.id")})

	url := "/api/collections/clans/records/" + r.Id
	if _, b := e.do(t, e.owner, "GET", url, ""); b["secret"] != "s3cret" {
		t.Fatalf("owner should see secret: %v", b)
	}
	code, b := e.do(t, e.other, "GET", url, "")
	if code != 200 {
		t.Fatal(code)
	}
	if _, has := b["secret"]; has {
		t.Fatalf("secret leaked: %v", b)
	}
	if b["title"] != "alpha" {
		t.Fatalf("other fields must stay: %v", b)
	}
	// anonymous list
	_, l := e.do(t, nil, "GET", "/api/collections/clans/records", "")
	items := l["items"].([]any)
	if _, has := items[0].(map[string]any)["secret"]; has {
		t.Fatalf("list leaked: %v", l)
	}
	// superuser bypasses
	if _, b := e.do(t, e.su, "GET", url, ""); b["secret"] != "s3cret" {
		t.Fatalf("superuser should see secret: %v", b)
	}
	// ... unless enforcing
	t.Setenv(EnvSuperuser, "enforce")
	if _, b := e.do(t, e.su, "GET", url, ""); b["secret"] != nil {
		t.Fatalf("enforce: superuser should not match the rule: %v", b)
	}
}

func TestReadHiddenOnCreateResponse(t *testing.T) {
	e := setup(t)
	e.rule(t, "secret", SetOptions{ReadSet: true, Read: s("leader = @request.auth.id")})
	code, b := e.do(t, e.other, "POST", "/api/collections/clans/records", `{"title":"x","secret":"y","leader":"`+e.owner.Id+`"}`)
	if code != 200 {
		t.Fatal(code, b)
	}
	if _, has := b["secret"]; has {
		t.Fatalf("create response leaked: %v", b)
	}
}

func TestWriteLockedBlocksUserNotSuperuser(t *testing.T) {
	e := setup(t)
	r := e.seed(t)
	e.rule(t, "secret", SetOptions{WriteSet: true, Write: s("")})
	url := "/api/collections/clans/records/" + r.Id

	code, b := e.do(t, e.owner, "PATCH", url, `{"secret":"new"}`)
	if code != 400 {
		t.Fatalf("status %d: %v", code, b)
	}
	if b["message"] != "Failed to update record." {
		t.Fatalf("message: %v", b)
	}
	f := b["data"].(map[string]any)["secret"].(map[string]any)
	if f["code"] != ErrCode || f["message"] != ErrMessage {
		t.Fatalf("shape: %v", b)
	}
	// untouched fields still writable
	if code, _ := e.do(t, e.owner, "PATCH", url, `{"title":"beta"}`); code != 200 {
		t.Fatal(code)
	}
	// create with the locked field is rejected too
	if code, _ := e.do(t, e.owner, "POST", "/api/collections/clans/records", `{"title":"n","secret":"x"}`); code != 400 {
		t.Fatal(code)
	}
	// superuser passes
	if code, _ := e.do(t, e.su, "PATCH", url, `{"secret":"su"}`); code != 200 {
		t.Fatal(code)
	}
	// enforce mode blocks the superuser as well
	t.Setenv(EnvSuperuser, "enforce")
	if code, _ := e.do(t, e.su, "PATCH", url, `{"secret":"su2"}`); code != 400 {
		t.Fatal(code)
	}
}

func TestWriteRuleLeaderOnly(t *testing.T) {
	e := setup(t)
	r := e.seed(t) // leader = owner
	e.rule(t, "leader", SetOptions{WriteSet: true, Write: s("@request.auth.id = leader")})
	url := "/api/collections/clans/records/" + r.Id

	if code, b := e.do(t, e.other, "PATCH", url, `{"leader":"`+e.other.Id+`"}`); code != 400 {
		t.Fatalf("non leader must be blocked: %d %v", code, b)
	}
	// modifier syntax is covered
	// (a modifier that leaves the stored value unchanged is not a change and passes)
	code, _ := e.do(t, e.other, "PATCH", url, `{"leader+":"x"}`)
	if cur, _ := e.app.FindRecordById("clans", r.Id); code != 400 && cur.GetString("leader") != e.owner.Id {
		t.Fatalf("modifier changed the field without the rule: %d", code)
	}
	if code, b := e.do(t, e.owner, "PATCH", url, `{"leader":"`+e.other.Id+`"}`); code != 200 {
		t.Fatalf("leader must pass: %d %v", code, b)
	}
	// the original record decides: the old leader lost the right
	if code, _ := e.do(t, e.owner, "PATCH", url, `{"leader":"`+e.owner.Id+`"}`); code != 400 {
		t.Fatal("former leader must be blocked")
	}
}

func TestWriteRuleOnBodyAndCreate(t *testing.T) {
	e := setup(t)
	// on create a user may only set themselves as leader
	e.rule(t, "leader", SetOptions{WriteSet: true, Write: s("@request.body.leader = @request.auth.id")})
	if code, b := e.do(t, e.owner, "POST", "/api/collections/clans/records", `{"title":"a","leader":"`+e.other.Id+`"}`); code != 400 {
		t.Fatalf("%d %v", code, b)
	}
	if code, b := e.do(t, e.owner, "POST", "/api/collections/clans/records", `{"title":"a","leader":"`+e.owner.Id+`"}`); code != 200 {
		t.Fatalf("%d %v", code, b)
	}
	// rule on the submitted record (create)
	e.rule(t, "leader", SetOptions{WriteSet: true, Write: s("title != 'forbidden'")})
	if code, _ := e.do(t, e.owner, "POST", "/api/collections/clans/records", `{"title":"forbidden","leader":"x"}`); code != 400 {
		t.Fatal(code)
	}
	if code, b := e.do(t, e.owner, "POST", "/api/collections/clans/records", `{"title":"fine","leader":"x"}`); code != 200 {
		t.Fatalf("%d %v", code, b)
	}
}

func TestBatchGoesThroughHooks(t *testing.T) {
	e := setup(t)
	r := e.seed(t)
	e.rule(t, "secret", SetOptions{WriteSet: true, Write: s("")})
	s := e.app.Settings()
	s.Batch.Enabled = true
	if err := e.app.Save(s); err != nil {
		t.Fatal(err)
	}
	body := `{"requests":[{"method":"PATCH","url":"/api/collections/clans/records/` + r.Id + `","body":{"secret":"hack"}}]}`
	code, b := e.do(t, e.owner, "POST", "/api/batch", body)
	if code == 200 {
		t.Fatalf("batch must be rejected: %v", b)
	}
	got, _ := e.app.FindRecordById("clans", r.Id)
	if got.GetString("secret") != "s3cret" {
		t.Fatal("secret was changed through batch")
	}
}

func TestLintUnknownField(t *testing.T) {
	e := setup(t)
	e.rule(t, "secret", SetOptions{ReadSet: true, Read: s("leader = @request.auth.id")})
	fs, err := Lint(e.app)
	if err != nil || len(fs) != 0 {
		t.Fatalf("clean expected: %v %v", fs, err)
	}
	// bypass Set's field check, like a field that was renamed later
	col, _ := e.app.FindCollectionByNameOrId(CollectionName)
	rec := core.NewRecord(col)
	rec.Set("collection", "clans")
	rec.Set("field", "ghost")
	rec.Set("read_rule", encodeRule(s("nope = 1")))
	rec.Set("write_rule", encodeRule(nil))
	if err := e.app.Save(rec); err != nil {
		t.Fatal(err)
	}
	fs, _ = Lint(e.app)
	if len(fs) < 1 || fs[0].Field != "ghost" {
		t.Fatalf("expected unknown field finding: %+v", fs)
	}
	if _, err := Set(e.app, "clans", "ghost2", SetOptions{ReadSet: true}); err == nil {
		t.Fatal("Set must reject unknown fields")
	}
	// bad expression
	e.rule(t, "title", SetOptions{WriteSet: true, Write: s("unknown_col = 1")})
	fs, _ = Lint(e.app)
	found := false
	for _, f := range fs {
		if f.Field == "title" && f.Kind == "write" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected parse finding: %+v", fs)
	}
}

func TestCacheInvalidation(t *testing.T) {
	e := setup(t)
	r := e.seed(t)
	url := "/api/collections/clans/records/" + r.Id
	if _, b := e.do(t, e.other, "GET", url, ""); b["secret"] != "s3cret" {
		t.Fatal("no rule yet")
	}
	e.rule(t, "secret", SetOptions{ReadSet: true, Read: s("leader = @request.auth.id")})
	if _, b := e.do(t, e.other, "GET", url, ""); b["secret"] != nil {
		t.Fatal("rule must apply right after save")
	}
	// update through the record API of the rules collection
	rec, _ := e.app.FindFirstRecordByFilter(CollectionName, "field='secret'")
	rec.Set("read_rule", encodeRule(s("")))
	if err := e.app.Save(rec); err != nil {
		t.Fatal(err)
	}
	if _, b := e.do(t, e.other, "GET", url, ""); b["secret"] != "s3cret" {
		t.Fatal("cache must refresh after update")
	}
	if ok, err := Remove(e.app, "clans", "secret"); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if rs := rf(e.m, r.Collection()); len(rs) != 0 {
		t.Fatal("cache must refresh after delete")
	}
}

func TestCacheTTL(t *testing.T) {
	e := setup(t)
	now := time.Now()
	e.m.now = func() time.Time { return now }
	col, _ := e.app.FindCollectionByNameOrId("clans")
	rf(e.m, col)
	// write behind the module's back (another process)
	if _, err := e.app.DB().NewQuery("INSERT INTO _field_rules (id, collection, field, read_rule, write_rule, note, created, updated) VALUES ('abcdefghij12345','clans','secret','\"\"','null','','','')").Execute(); err != nil {
		t.Fatal(err)
	}
	if len(rf(e.m, col)) != 0 {
		t.Fatal("still cached")
	}
	now = now.Add(cacheTTL + time.Second)
	if len(rf(e.m, col)) != 1 {
		t.Fatal("ttl refresh expected")
	}
}

func TestRulesCollectionSuperuserOnly(t *testing.T) {
	e := setup(t)
	if code, _ := e.do(t, e.owner, "GET", "/api/collections/_field_rules/records", ""); code != 403 {
		t.Fatalf("user must be forbidden: %d", code)
	}
	if code, _ := e.do(t, nil, "GET", "/api/collections/_field_rules/records", ""); code != 403 {
		t.Fatal(code)
	}
	if code, _ := e.do(t, e.su, "GET", "/api/collections/_field_rules/records", ""); code != 200 {
		t.Fatal(code)
	}
}

func TestAuditSampling(t *testing.T) {
	e := setup(t)
	r := e.seed(t)
	e.rule(t, "secret", SetOptions{WriteSet: true, Write: s("")})
	now := time.Now()
	e.m.now = func() time.Time { return now }
	var n int
	var got map[string]any
	SetAuditSink(func(action, collection, record string, d map[string]any) {
		if action == ActionDenied {
			n++
			got = d
		}
	})
	defer SetAuditSink(nil)
	url := "/api/collections/clans/records/" + r.Id
	for i := 0; i < 3; i++ {
		e.do(t, e.owner, "PATCH", url, `{"secret":"x"}`)
	}
	if n != 1 || got["field"] != "secret" || got["user"] != e.owner.Id {
		t.Fatalf("n=%d %v", n, got)
	}
	now = now.Add(61 * time.Second)
	e.do(t, e.owner, "PATCH", url, `{"secret":"x"}`)
	if n != 2 {
		t.Fatalf("n=%d", n)
	}
	e.do(t, e.other, "PATCH", url, `{"secret":"x"}`)
	if n != 3 {
		t.Fatalf("other user is sampled separately: n=%d", n)
	}
}

func TestExpandedRelationHidesField(t *testing.T) {
	e := setup(t)
	r := e.seed(t)
	clans, _ := e.app.FindCollectionByNameOrId("clans")
	c := core.NewBaseCollection("posts")
	c.Fields.Add(&core.RelationField{Name: "clan", CollectionId: clans.Id, MaxSelect: 1})
	open := ""
	c.ListRule, c.ViewRule = &open, &open
	if err := e.app.Save(c); err != nil {
		t.Fatal(err)
	}
	p := core.NewRecord(c)
	p.Set("clan", r.Id)
	if err := e.app.Save(p); err != nil {
		t.Fatal(err)
	}
	e.rule(t, "secret", SetOptions{ReadSet: true, Read: s("leader = @request.auth.id")})
	_, b := e.do(t, e.other, "GET", "/api/collections/posts/records/"+p.Id+"?expand=clan", "")
	exp := b["expand"].(map[string]any)["clan"].(map[string]any)
	if _, has := exp["secret"]; has || exp["title"] != "alpha" {
		t.Fatalf("expanded record leaked: %v", exp)
	}
	_, b = e.do(t, e.owner, "GET", "/api/collections/posts/records/"+p.Id+"?expand=clan", "")
	if b["expand"].(map[string]any)["clan"].(map[string]any)["secret"] != "s3cret" {
		t.Fatal("owner must see expanded secret")
	}
}
