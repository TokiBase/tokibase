//go:build !no_mcp

package mcp

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
)

// pub creates a collection with every rule public.
func (e *env) pub(name string, fields ...kernel.Field) *kernel.Collection {
	e.t.Helper()
	c := kernel.NewBaseCollection(name)
	c.Fields.Add(fields...)
	c.ListRule, c.ViewRule, c.CreateRule, c.UpdateRule, c.DeleteRule = strp(""), strp(""), strp(""), strp(""), strp("")
	if err := e.app.Save(c); err != nil {
		e.t.Fatal(err)
	}
	return c
}

func (e *env) agentRate(name string, role Role, rate int, cols ...string) *Agent {
	e.t.Helper()
	a, _, err := CreateAgent(e.app, name, role, cols, rate)
	if err != nil {
		e.t.Fatal(err)
	}
	return a
}

func (e *env) corrupt(agent string, v any) {
	e.t.Helper()
	coll, _ := e.app.FindCollectionByNameOrId(CollectionName)
	rec, err := e.app.FindFirstRecordByData(coll, "name", agent)
	if err != nil {
		e.t.Fatal(err)
	}
	rec.Set("collections", v)
	if err := e.app.Save(rec); err != nil {
		e.t.Fatal(err)
	}
}

// H1
func TestH1ReservedCollectionsDenied(t *testing.T) {
	e := setup(t)
	e.pub("_webhooks", &kernel.TextField{Name: "url"}) // module collection without System
	sys := kernel.NewBaseCollection("sysconf")
	sys.System = true
	sys.Fields.Add(&kernel.TextField{Name: "v"})
	sys.ListRule, sys.ViewRule = strp(""), strp("")
	if err := e.app.Save(sys); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"_webhooks", "sysconf"} {
		for _, role := range []Role{RoleReader, RoleWriter} {
			cs, _ := e.connect(e.agent(fmt.Sprintf("a-%s-%s", role, name), role))
			mustFail(t, cs, "records.query", map[string]any{"collection": name}, "not found or not accessible")
			mustFail(t, cs, "schema.describe", map[string]any{"collection": name}, "not found or not accessible")
			if role == RoleWriter {
				mustFail(t, cs, "records.create", map[string]any{"collection": name, "data": map[string]any{"url": "https://x"}, "reason": "attack"}, "not found or not accessible")
				mustFail(t, cs, "records.batch", map[string]any{"ops": []any{map[string]any{"action": "create", "collection": name, "data": map[string]any{"url": "x"}}}, "reason": "attack"}, "not found or not accessible")
			}
		}
	}
	op, _ := e.connect(e.agent("op", RoleOperator))
	mustOK(t, op, "records.query", map[string]any{"collection": "_webhooks"})
	mustFail(t, op, "records.create", map[string]any{"collection": "_webhooks", "data": map[string]any{"url": "https://x"}, "reason": "nope"}, "not found or not accessible")
	rd, _ := e.connect(e.agent("rd", RoleReader))
	out := mustOK(t, rd, "schema.list", nil)
	for _, c := range out["collections"].([]any) {
		if n := c.(map[string]any)["name"].(string); n == "_webhooks" || n == "sysconf" {
			t.Fatalf("reader sees %s", n)
		}
	}
}

// H2
func TestH2MalformedAllowlistFailsClosed(t *testing.T) {
	for i, bad := range []any{"posts", map[string]any{"a": 1}, []any{"notes", 1}} {
		e := setup(t)
		a := e.agent("w", RoleWriter)
		cs, _ := e.connect(a)
		mustOK(t, cs, "records.query", map[string]any{"collection": "notes"})
		e.corrupt("w", bad)
		mustFail(t, cs, "schema.list", nil, "agent configuration is invalid")
		mustFail(t, cs, "records.query", map[string]any{"collection": "notes"}, "agent configuration is invalid")
		mustFail(t, cs, "records.create", map[string]any{"collection": "notes", "data": map[string]any{"title": "x"}, "reason": "try"}, "agent configuration is invalid")
		if n, _ := e.app.CountRecords("notes"); n != 0 {
			t.Fatalf("case %d: write went through", i)
		}
		if _, err := Authenticate(e.app, "tka_x"); err == nil {
			t.Fatal("bad key must fail")
		}
		rec, _ := e.app.FindFirstRecordByData(mustColl(t, e), "name", "w")
		if ag := agentFromRecord(rec); !ag.invalid || ag.allows("notes") {
			t.Fatalf("case %d: agent must be invalid and deny: %+v", i, ag)
		}
	}
}

func mustColl(t *testing.T, e *env) *kernel.Collection {
	c, err := e.app.FindCollectionByNameOrId(CollectionName)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// H3
func TestH3WriteResultsObeyReadCheck(t *testing.T) {
	e := setup(t)
	vault := e.seed("vault", map[string]any{"title": "top secret"})
	note := e.seed("notes", map[string]any{"title": "public note"})
	cs, _ := e.connect(e.agent("w", RoleWriter))

	out := mustOK(t, cs, "records.update", map[string]any{"collection": "vault", "id": vault.Id, "data": map[string]any{}, "reason": "probe"})
	rec := out["record"].(map[string]any)
	if _, leaked := rec["title"]; leaked || rec["id"] != vault.Id {
		t.Fatalf("update leaked a locked record: %v", rec)
	}
	out = mustOK(t, cs, "records.update", map[string]any{"collection": "notes", "id": note.Id, "data": map[string]any{}, "reason": "probe"})
	if out["record"].(map[string]any)["title"] != "public note" {
		t.Fatalf("readable record must be returned: %v", out)
	}
	out = mustOK(t, cs, "records.delete", map[string]any{"collection": "vault", "id": vault.Id, "reason": "probe"})
	prev := out["plan"].(map[string]any)["preview"].(map[string]any)
	if _, leaked := prev["title"]; leaked || prev["id"] != vault.Id {
		t.Fatalf("delete preview leaked: %v", prev)
	}
	out = mustOK(t, cs, "records.delete", map[string]any{"collection": "notes", "id": note.Id, "reason": "probe"})
	if out["plan"].(map[string]any)["preview"].(map[string]any)["title"] == nil {
		t.Fatalf("readable preview expected: %v", out)
	}
	cr := mustOK(t, cs, "records.create", map[string]any{"collection": "vault", "data": map[string]any{"title": "mine"}, "reason": "probe"})
	if _, leaked := cr["record"].(map[string]any)["title"]; leaked {
		t.Fatalf("create echoed a locked record: %v", cr)
	}
	op, _ := e.connect(e.agent("op", RoleOperator))
	out = mustOK(t, op, "records.update", map[string]any{"collection": "vault", "id": vault.Id, "data": map[string]any{}, "reason": "probe"})
	if out["record"].(map[string]any)["title"] != "top secret" {
		t.Fatalf("operator sees all: %v", out)
	}
}

// M1 / M2
func TestM1M2TraversalRestrictedToAllowlist(t *testing.T) {
	e := setup(t)
	people := e.pub("people", &kernel.TextField{Name: "name"}, &kernel.TextField{Name: "phone"})
	e.pub("private_notes", &kernel.TextField{Name: "body"})
	e.pub("posts", &kernel.TextField{Name: "title"},
		&kernel.RelationField{Name: "author", CollectionId: people.Id, MaxSelect: 1})
	for _, role := range []Role{RoleReader, RoleWriter, RoleOperator} {
		cs, _ := e.connect(e.agent("a-"+string(role), role, "posts"))
		for name, args := range map[string]struct {
			args map[string]any
			want string
		}{
			"expand":     {map[string]any{"collection": "posts", "expand": "author"}, "people"},
			"collection": {map[string]any{"collection": "posts", "filter": "@collection.private_notes.body ~ 'salary'"}, "private_notes"},
			"alias":      {map[string]any{"collection": "posts", "filter": "@collection.private_notes:x.body ~ 'a'"}, "private_notes"},
			"relation":   {map[string]any{"collection": "posts", "filter": "author.phone ~ '+62'"}, "people"},
			"sort":       {map[string]any{"collection": "posts", "sort": "-author.name"}, "people"},
			"via":        {map[string]any{"collection": "posts", "filter": "people_via_nothing.name = 'x'"}, "people"},
		} {
			_, msg := callTool(t, cs, "records.query", args.args)
			if !strings.Contains(msg, "outside the agent allowlist") || !strings.Contains(msg, args.want) {
				t.Fatalf("%s/%s: want rejection naming %s, got %q", role, name, args.want, msg)
			}
		}
		mustFail(t, cs, "records.get", map[string]any{"collection": "posts", "id": "x", "expand": "author"}, "outside the agent allowlist")
		// quoted text and plain fields are fine
		mustOK(t, cs, "records.query", map[string]any{"collection": "posts", "filter": "title ~ '@collection.private_notes.body' && author = ''"})
	}
	// allowed relation target expands
	cs, _ := e.connect(e.agent("both", RoleWriter, "posts", "people"))
	mustOK(t, cs, "records.query", map[string]any{"collection": "posts", "expand": "author", "filter": "author.phone ~ '1'"})
}

// M4
func TestM4CascadeDelete(t *testing.T) {
	e := setup(t)
	projects := e.pub("projects", &kernel.TextField{Name: "name"})
	e.pub("invoices", &kernel.TextField{Name: "n"},
		&kernel.RelationField{Name: "project", CollectionId: projects.Id, MaxSelect: 1, CascadeDelete: true})
	p := e.seed("projects", map[string]any{"name": "p"})
	inv := e.seed("invoices", map[string]any{"n": "1", "project": p.Id})

	narrow, _ := e.connect(e.agent("narrow", RoleWriter, "projects"))
	_, msg := callTool(t, narrow, "records.delete", map[string]any{"collection": "projects", "id": p.Id, "reason": "cleanup"})
	if !strings.Contains(msg, "cascade") || strings.Contains(msg, "invoices") {
		t.Fatalf("denial must not name the target: %q", msg)
	}
	_, msg = callTool(t, narrow, "records.batch", map[string]any{"ops": []any{map[string]any{"action": "delete", "collection": "projects", "id": p.Id}}, "reason": "cleanup"})
	if !strings.Contains(msg, "cascade") || strings.Contains(msg, "invoices") {
		t.Fatalf("batch denial: %q", msg)
	}
	if _, err := e.app.FindRecordById("invoices", inv.Id); err != nil {
		t.Fatal("invoice must survive")
	}

	wide, _ := e.connect(e.agent("wide", RoleWriter, "projects", "invoices"))
	out := mustOK(t, wide, "records.delete", map[string]any{"collection": "projects", "id": p.Id, "reason": "cleanup"})
	w := out["plan"].(map[string]any)["warnings"].([]any)
	if len(w) != 1 || !strings.Contains(w[0].(string), "other collections") {
		t.Fatalf("plan must warn generically: %v", w)
	}

	op, _ := e.connect(e.agent("op", RoleOperator, "projects"))
	mustOK(t, op, "records.delete", map[string]any{"collection": "projects", "id": p.Id, "reason": "cleanup"})
}

// M5
func TestM5DeniedCallsRateLimitedAndAuditThrottled(t *testing.T) {
	e := setup(t)
	a := e.agentRate("r", RoleReader, 5)
	cs, srv := e.connect(a)
	now := time.Now()
	srv.now = func() time.Time { return now }
	args := map[string]any{"collection": "notes", "data": map[string]any{"title": "x"}, "reason": "spam"}
	var perm, rate int
	for i := 0; i < 20; i++ {
		_, msg := callTool(t, cs, "records.create", args)
		switch {
		case strings.Contains(msg, "needs role writer"):
			perm++
		case strings.Contains(msg, "rate limit exceeded"):
			rate++
		default:
			t.Fatalf("unexpected: %q", msg)
		}
	}
	if perm != 5 || rate != 15 {
		t.Fatalf("denied calls must consume the budget: perm=%d rate=%d", perm, rate)
	}
	rows := e.rows("agent.records.create")
	if len(rows) != 1 || rows[0].details["denied"] != true {
		t.Fatalf("want exactly 1 audit row for the burst, got %d", len(rows))
	}
	now = now.Add(2 * time.Minute)
	callTool(t, cs, "records.create", args)
	rows = e.rows("agent.records.create")
	if len(rows) != 2 || rows[1].details["denied_suppressed_since_last"] != 19 {
		t.Fatalf("second row must carry the counter: %d %v", len(rows), rows[len(rows)-1].details)
	}
}

// M7
func TestM7WritesRefusedWithoutAudit(t *testing.T) {
	e := setup(t)
	SetAuditSink(nil)
	cs, _ := e.connect(e.agent("w", RoleWriter))
	args := map[string]any{"collection": "notes", "data": map[string]any{"title": "x"}, "reason": "try"}
	mustFail(t, cs, "records.create", args, "audit module is disabled")
	mustOK(t, cs, "records.query", map[string]any{"collection": "notes"})
	t.Setenv(EnvUnaudited, "1")
	mustOK(t, cs, "records.create", args)
}

// M8: the module hooks of the core request pipeline apply (fieldperm style).
func TestM8FieldHooksApply(t *testing.T) {
	e := setup(t)
	e.app.OnRecordEnrich("notes").BindFunc(func(ev *core.RecordEnrichEvent) error {
		if err := ev.Next(); err != nil {
			return err
		}
		if ev.RequestInfo != nil && !ev.RequestInfo.HasSuperuserAuth() {
			ev.Record.Hide("api_key")
		}
		return nil
	})
	e.app.OnRecordCreateRequest("notes").BindFunc(func(ev *core.RecordRequestEvent) error {
		info, err := ev.RequestInfo()
		if err != nil {
			return err
		}
		if _, ok := info.Body["api_key"]; ok {
			return errors.New("field api_key is write protected")
		}
		return ev.Next()
	})
	e.app.OnRecordUpdateRequest("notes").BindFunc(func(ev *core.RecordRequestEvent) error {
		info, _ := ev.RequestInfo()
		if _, ok := info.Body["api_key"]; ok {
			return errors.New("field api_key is write protected")
		}
		return ev.Next()
	})
	n := e.seed("notes", map[string]any{"title": "t", "api_key": "sekret-1"})
	rd, _ := e.connect(e.agent("rd", RoleReader))
	got := mustOK(t, rd, "records.get", map[string]any{"collection": "notes", "id": n.Id})["record"].(map[string]any)
	if _, ok := got["api_key"]; ok || got["title"] != "t" {
		t.Fatalf("api_key must be hidden from readers: %v", got)
	}
	q := mustOK(t, rd, "records.query", map[string]any{"collection": "notes"})["items"].([]any)
	if _, ok := q[0].(map[string]any)["api_key"]; ok {
		t.Fatalf("query leaked api_key: %v", q)
	}
	w, _ := e.connect(e.agent("w", RoleWriter))
	mustFail(t, w, "records.create", map[string]any{"collection": "notes", "data": map[string]any{"api_key": "x"}, "reason": "try"}, "write protected")
	mustFail(t, w, "records.update", map[string]any{"collection": "notes", "id": n.Id, "data": map[string]any{"api_key": "x"}, "reason": "try"}, "write protected")
	mustOK(t, w, "records.create", map[string]any{"collection": "notes", "data": map[string]any{"title": "ok"}, "reason": "fine"})
	mustFail(t, w, "records.batch", map[string]any{"ops": []any{map[string]any{"action": "create", "collection": "notes", "data": map[string]any{"api_key": "x"}}}, "reason": "try"}, "write protected")
	op, _ := e.connect(e.agent("op", RoleOperator))
	got = mustOK(t, op, "records.get", map[string]any{"collection": "notes", "id": n.Id})["record"].(map[string]any)
	if got["api_key"] != "sekret-1" {
		t.Fatalf("operator is superuser-like: %v", got)
	}
}

// M9
func TestM9SchemaExposure(t *testing.T) {
	e := setup(t)
	people := e.pub("people", &kernel.TextField{Name: "name"})
	e.pub("posts", &kernel.TextField{Name: "title"}, &kernel.RelationField{Name: "author", CollectionId: people.Id, MaxSelect: 1})
	v := kernel.NewViewCollection("v_posts")
	v.ViewQuery = "SELECT id, title FROM posts"
	v.ListRule = strp("")
	if err := e.app.Save(v); err != nil {
		t.Fatal(err)
	}
	e.seed("vault", map[string]any{"title": "x"})
	rd, _ := e.connect(e.agent("rd", RoleReader, "posts", "v_posts", "vault"))
	for _, it := range mustOK(t, rd, "schema.list", nil)["collections"].([]any) {
		if _, ok := it.(map[string]any)["records"]; ok {
			t.Fatalf("reader must not see counts: %v", it)
		}
	}
	d := mustOK(t, rd, "schema.describe", map[string]any{"collection": "v_posts"})
	if _, ok := d["viewQuery"]; ok {
		t.Fatalf("viewQuery leaked: %v", d)
	}
	d = mustOK(t, rd, "schema.describe", map[string]any{"collection": "posts"})
	if strings.Contains(fmt.Sprint(d["fields"]), people.Id) {
		t.Fatalf("foreign relation target id leaked: %v", d["fields"])
	}
	op, _ := e.connect(e.agent("op", RoleOperator))
	if d := mustOK(t, op, "schema.describe", map[string]any{"collection": "v_posts"}); d["viewQuery"] == nil {
		t.Fatal("operator gets viewQuery")
	}
	found := false
	for _, it := range mustOK(t, op, "schema.list", nil)["collections"].([]any) {
		if it.(map[string]any)["name"] == "vault" {
			found = it.(map[string]any)["records"] != nil
		}
	}
	if !found {
		t.Fatal("operator gets counts")
	}
}

// M10
func TestM10HumanConfirm(t *testing.T) {
	e := setup(t)
	t.Setenv(EnvRequireHumanConfirm, "1")
	n := e.seed("notes", map[string]any{"title": "t"})
	w, _ := e.connect(e.agent("w", RoleWriter))
	args := map[string]any{"collection": "notes", "id": n.Id, "reason": "cleanup"}
	out := mustOK(t, w, "records.delete", args)
	tok := out["confirm_token"].(string)
	if !strings.Contains(out["next"].(string), "toki agent confirm") {
		t.Fatalf("next hint: %v", out["next"])
	}
	args["confirm_token"] = tok
	mustFail(t, w, "records.delete", args, "human approval")
	if _, err := e.app.FindRecordById("notes", n.Id); err != nil {
		t.Fatal("not approved: record must remain")
	}
	if _, err := ApproveConfirm(e.app.DataDir(), "nonsense", time.Now()); err == nil {
		t.Fatal("unknown token must fail")
	}
	if _, err := ApproveConfirm(e.app.DataDir(), tok, time.Now()); err != nil {
		t.Fatal(err)
	}
	mustOK(t, w, "records.delete", args)
	if _, err := e.app.FindRecordById("notes", n.Id); err == nil {
		t.Fatal("approved delete must run")
	}
	n2 := e.seed("notes", map[string]any{"title": "t2"})
	args["id"] = n2.Id
	mustFail(t, w, "records.delete", args, "unknown or already used")
}

// LOW
func TestLowRedaction(t *testing.T) {
	got := sanitize(map[string]any{
		"Authorization": "Bearer abc", "headers": map[string]any{"X": "1"}, "Cookie": "s=1",
		"api_key": "k", "Password": "p", "keywords": "k", "nested": []any{map[string]any{"token": "t", "ok": "v"}},
		"title": "fine",
	}, 0).(map[string]any)
	for _, k := range []string{"Authorization", "headers", "Cookie", "api_key", "Password"} {
		if got[k] != "[redacted]" {
			t.Fatalf("%s not redacted: %v", k, got[k])
		}
	}
	if got["title"] != "fine" || got["nested"].([]any)[0].(map[string]any)["token"] != "[redacted]" {
		t.Fatalf("%v", got)
	}
}

func TestLowRecoverPayloadTimeoutGenericErrors(t *testing.T) {
	e := setup(t)
	a := e.agent("w", RoleWriter)
	srv := NewServer(e.app, a, "test")
	addTool(srv, "test.panic", "panics", RoleReader, false, func(c *call, _ emptyIn) (map[string]any, error) {
		panic("boom: secret table name")
	})
	cs := e.connectServer(srv)
	_, msg := callTool(t, cs, "test.panic", nil)
	if msg != "internal error" {
		t.Fatalf("panic must be generic: %q", msg)
	}
	mustOK(t, cs, "schema.list", nil) // process still alive

	big := strings.Repeat("a", maxPayload+10)
	mustFail(t, cs, "records.create", map[string]any{"collection": "notes", "data": map[string]any{"title": big}, "reason": "big"}, "payload too large")

	old := queryTimeout
	queryTimeout = time.Nanosecond
	defer func() { queryTimeout = old }()
	mustFail(t, cs, "records.query", map[string]any{"collection": "notes"}, "timed out")
	queryTimeout = old

	err := srv.userErr(nil, "create failed", errors.New("UNIQUE constraint failed: _tbl.col"))
	if err.Error() != "internal error" {
		t.Fatalf("db errors must be generic: %v", err)
	}
	_, msg = callTool(t, cs, "records.query", map[string]any{"collection": "notes", "filter": "nonexistent_field = 1"})
	if strings.Contains(msg, "sql") || !strings.Contains(msg, "query failed") {
		t.Fatalf("query error: %q", msg)
	}
}

func TestLowRuneSafeTruncation(t *testing.T) {
	r := strp(strings.Repeat("é", 100))
	if c := ruleCell(r); !utf8.ValidString(c) {
		t.Fatalf("invalid utf8: %q", c)
	}
	if s := sanitize(strings.Repeat("日", 100), 40).(string); !utf8.ValidString(s) {
		t.Fatal("invalid utf8")
	}
}

func TestSensitiveFieldsRedactedForOperator(t *testing.T) {
	e := setup(t)
	c := e.pub("vaultmcp", &kernel.TextField{Name: "title"}, &kernel.TextField{Name: "enc"})
	kernel.RegisterSensitiveField(c.Id, "enc")
	t.Cleanup(func() { kernel.UnregisterSensitiveField(c.Id, "enc") })
	rec := e.seed("vaultmcp", map[string]any{"title": "t", "enc": "tkc1:1:CIPHERTEXT"})
	op, _ := e.connect(e.agent("op", RoleOperator))
	out := mustOK(t, op, "records.get", map[string]any{"collection": "vaultmcp", "id": rec.Id})
	r := out["record"].(map[string]any)
	if r["enc"] != "[encrypted]" || r["title"] != "t" {
		t.Fatalf("operator export: %v", r)
	}
	out = mustOK(t, op, "records.delete", map[string]any{"collection": "vaultmcp", "id": rec.Id, "reason": "probe"})
	if p := out["plan"].(map[string]any)["preview"].(map[string]any); p["enc"] != "[encrypted]" {
		t.Fatalf("preview: %v", p)
	}
}
