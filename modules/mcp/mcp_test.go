//go:build !no_mcp

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tests"
)

type auditRow struct {
	action, collection, record string
	details                    map[string]any
}

type env struct {
	t     *testing.T
	app   *tests.TestApp
	mu    sync.Mutex
	audit []auditRow
}

func (e *env) rows(action string) []auditRow {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []auditRow
	for _, r := range e.audit {
		if r.action == action {
			out = append(out, r)
		}
	}
	return out
}

func strp(s string) *string { return &s }

func setup(t *testing.T) *env {
	t.Helper()
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)
	e := &env{t: t, app: app}
	SetAuditSink(func(action, collection, record string, details map[string]any) {
		e.mu.Lock()
		e.audit = append(e.audit, auditRow{action, collection, record, details})
		e.mu.Unlock()
	})
	t.Cleanup(func() { SetAuditSink(nil); SetProviders(Providers{}) })
	Register(app)

	mk := func(name string, list, view, create, update, del *string, fields ...kernel.Field) {
		c := kernel.NewBaseCollection(name)
		c.Fields.Add(fields...)
		c.ListRule, c.ViewRule, c.CreateRule, c.UpdateRule, c.DeleteRule = list, view, create, update, del
		if err := app.Save(c); err != nil {
			t.Fatal(err)
		}
	}
	mk("notes", strp(""), strp(""), strp(""), strp(""), strp(""),
		&kernel.TextField{Name: "title"}, &kernel.TextField{Name: "api_key"})
	mk("docs", strp("published = true"), strp("published = true"), nil, nil, nil,
		&kernel.TextField{Name: "title"}, &kernel.BoolField{Name: "published"})
	mk("vault", nil, nil, nil, nil, nil, &kernel.TextField{Name: "title"})
	return e
}

func (e *env) seed(collection string, data map[string]any) *kernel.Record {
	e.t.Helper()
	col, err := e.app.FindCollectionByNameOrId(collection)
	if err != nil {
		e.t.Fatal(err)
	}
	r := kernel.NewRecord(col)
	for k, v := range data {
		r.Set(k, v)
	}
	if err := e.app.Save(r); err != nil {
		e.t.Fatal(err)
	}
	return r
}

func (e *env) agent(name string, role Role, cols ...string) *Agent {
	e.t.Helper()
	a, _, err := CreateAgent(e.app, name, role, cols, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	return a
}

func (e *env) connect(a *Agent) (*sdk.ClientSession, *Server) {
	e.t.Helper()
	srv := NewServer(e.app, a, "test")
	ct, st := sdk.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	ss, err := srv.SDK().Connect(ctx, st, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { cs.Close(); ss.Close(); cancel() })
	return cs, srv
}

// call invokes a tool and returns the structured result and the error text ("" when ok).
func callTool(t *testing.T, cs *sdk.ClientSession, tool string, args map[string]any) (map[string]any, string) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return nil, err.Error()
	}
	if res.IsError {
		var sb strings.Builder
		for _, c := range res.Content {
			if tc, ok := c.(*sdk.TextContent); ok {
				sb.WriteString(tc.Text)
			}
		}
		return nil, sb.String()
	}
	raw, _ := json.Marshal(res.StructuredContent)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out, ""
}

func mustOK(t *testing.T, cs *sdk.ClientSession, tool string, args map[string]any) map[string]any {
	t.Helper()
	out, msg := callTool(t, cs, tool, args)
	if msg != "" {
		t.Fatalf("%s: unexpected error: %s", tool, msg)
	}
	return out
}

func mustFail(t *testing.T, cs *sdk.ClientSession, tool string, args map[string]any, contains string) {
	t.Helper()
	out, msg := callTool(t, cs, tool, args)
	if msg == "" {
		t.Fatalf("%s: expected error containing %q, got %v", tool, contains, out)
	}
	if !strings.Contains(msg, contains) {
		t.Fatalf("%s: error %q does not contain %q", tool, msg, contains)
	}
}

func TestAgentCreateRevoke(t *testing.T) {
	e := setup(t)
	if _, err := e.app.FindCollectionByNameOrId(CollectionName); err != nil {
		t.Fatalf("_agents not created: %v", err)
	}
	a, key, err := CreateAgent(e.app, "bot-1", RoleWriter, []string{"notes"}, 60)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key, "tka_") || a.RatePerMin != 60 || a.Role != RoleWriter {
		t.Fatalf("unexpected agent/key: %+v %q", a, key)
	}
	// only the hash is stored
	rec, _ := e.app.FindRecordById(CollectionName, a.ID)
	if rec.GetString("key_hash") != HashKey(key) || rec.GetString("key_hash") == key {
		t.Fatal("key must be stored as sha256 hash")
	}
	got, err := Authenticate(e.app, key)
	if err != nil || got.Name != "bot-1" || len(got.Collections) != 1 {
		t.Fatalf("authenticate: %v %+v", err, got)
	}
	if _, err := Authenticate(e.app, "tka_wrong"); err == nil {
		t.Fatal("wrong key must fail")
	}
	if _, _, err := CreateAgent(e.app, "bot-1", RoleReader, nil, 0); err == nil {
		t.Fatal("duplicate name must fail")
	}
	if _, _, err := CreateAgent(e.app, "bot-2", Role("god"), nil, 0); err == nil {
		t.Fatal("invalid role must fail")
	}
	if _, _, err := CreateAgent(e.app, "bot-3", RoleReader, []string{"nope"}, 0); err == nil {
		t.Fatal("unknown collection must fail")
	}
	if _, _, err := CreateAgent(e.app, "bot-4", RoleReader, []string{CollectionName}, 0); err == nil {
		t.Fatal("_agents can not be granted")
	}

	if _, err := RevokeAgent(e.app, "bot-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := Authenticate(e.app, key); err == nil {
		t.Fatal("revoked key must fail")
	}
	list, _ := ListAgents(e.app)
	if len(list) != 1 || list[0].Enabled {
		t.Fatalf("list after revoke: %+v", list)
	}
}

func TestRevokeAffectsRunningSession(t *testing.T) {
	e := setup(t)
	a := e.agent("live", RoleReader)
	cs, _ := e.connect(a)
	mustOK(t, cs, "schema.list", nil)
	if _, err := RevokeAgent(e.app, "live"); err != nil {
		t.Fatal(err)
	}
	mustFail(t, cs, "schema.list", nil, "revoked")
}

func TestReaderCannotCreate(t *testing.T) {
	e := setup(t)
	cs, _ := e.connect(e.agent("r", RoleReader))
	mustFail(t, cs, "records.create", map[string]any{
		"collection": "notes", "data": map[string]any{"title": "x"}, "reason": "try",
	}, "needs role writer")
	n, _ := e.app.CountRecords("notes")
	if n != 0 {
		t.Fatalf("record created by reader: %d", n)
	}
	rows := e.rows("agent.records.create")
	if len(rows) != 1 || rows[0].details["denied"] != true || rows[0].details["agent"] != "r" {
		t.Fatalf("denied attempt must be audited: %+v", rows)
	}
}

func TestWriterCreateAudited(t *testing.T) {
	e := setup(t)
	cs, srv := e.connect(e.agent("w", RoleWriter))
	mustFail(t, cs, "records.create", map[string]any{
		"collection": "notes", "data": map[string]any{"title": "x"},
	}, "reason")
	out := mustOK(t, cs, "records.create", map[string]any{
		"collection": "notes", "data": map[string]any{"title": "hello", "api_key": "s3cr3t"}, "reason": "seed data",
	})
	id := out["record"].(map[string]any)["id"].(string)
	if _, err := e.app.FindRecordById("notes", id); err != nil {
		t.Fatal(err)
	}
	mustFail(t, cs, "records.create", map[string]any{
		"collection": "notes", "data": map[string]any{"nope": 1}, "reason": "bad field",
	}, "unknown field")

	rows := e.rows("agent.records.create")
	var ok *auditRow
	for i := range rows {
		if rows[i].record == id {
			ok = &rows[i]
		}
	}
	if ok == nil {
		t.Fatalf("create not audited: %+v", rows)
	}
	d := ok.details
	if d["agent"] != "w" || d["reason"] != "seed data" || d["session"] != srv.Session() || ok.collection != "notes" {
		t.Fatalf("bad audit details: %+v", d)
	}
	if data := d["data"].(map[string]any); data["api_key"] != "[redacted]" || data["title"] != "hello" {
		t.Fatalf("data must be redacted: %+v", data)
	}

	// update + system collection write refusal
	mustOK(t, cs, "records.update", map[string]any{
		"collection": "notes", "id": id, "data": map[string]any{"title": "new"}, "reason": "rename",
	})
	if r, _ := e.app.FindRecordById("notes", id); r.GetString("title") != "new" {
		t.Fatal("update not applied")
	}
	if len(e.rows("agent.records.update")) != 1 {
		t.Fatal("update not audited")
	}
	mustFail(t, cs, "records.create", map[string]any{
		"collection": CollectionName, "data": map[string]any{"name": "x"}, "reason": "evil",
	}, "not found or not accessible")
}

func TestQueryRespectsAllowlist(t *testing.T) {
	e := setup(t)
	e.seed("notes", map[string]any{"title": "a"})
	e.seed("docs", map[string]any{"title": "d", "published": true})
	cs, _ := e.connect(e.agent("scoped", RoleWriter, "notes"))

	out := mustOK(t, cs, "records.query", map[string]any{"collection": "notes"})
	if out["totalItems"].(float64) != 1 {
		t.Fatalf("notes: %v", out)
	}
	mustFail(t, cs, "records.query", map[string]any{"collection": "docs"}, "not found or not accessible")
	mustFail(t, cs, "records.get", map[string]any{"collection": "docs", "id": "x"}, "not found or not accessible")
	mustFail(t, cs, "records.create", map[string]any{
		"collection": "docs", "data": map[string]any{"title": "z"}, "reason": "no",
	}, "not found or not accessible")
	mustFail(t, cs, "schema.describe", map[string]any{"collection": "docs"}, "not found or not accessible")
	mustFail(t, cs, "records.query", map[string]any{"collection": "notes", "perPage": 500}, "perPage")

	list := mustOK(t, cs, "schema.list", nil)["collections"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["name"] != "notes" {
		t.Fatalf("schema.list must only show the allowlist: %v", list)
	}
}

func TestGuestRuleEvaluationAndOperatorBypass(t *testing.T) {
	e := setup(t)
	pub := e.seed("docs", map[string]any{"title": "pub", "published": true})
	draft := e.seed("docs", map[string]any{"title": "draft", "published": false})
	e.seed("vault", map[string]any{"title": "secret"})

	reader, _ := e.connect(e.agent("r", RoleReader))
	out := mustOK(t, reader, "records.query", map[string]any{"collection": "docs"})
	if out["totalItems"].(float64) != 1 {
		t.Fatalf("reader must only see published docs: %v", out)
	}
	mustOK(t, reader, "records.get", map[string]any{"collection": "docs", "id": pub.Id})
	mustFail(t, reader, "records.get", map[string]any{"collection": "docs", "id": draft.Id}, "not accessible")
	mustFail(t, reader, "records.query", map[string]any{"collection": "vault"}, "locked list rule")

	op, _ := e.connect(e.agent("op", RoleOperator))
	out = mustOK(t, op, "records.query", map[string]any{"collection": "docs"})
	if out["totalItems"].(float64) != 2 {
		t.Fatalf("operator bypasses rules: %v", out)
	}
	mustOK(t, op, "records.query", map[string]any{"collection": "vault"})
	// operator reads are audited
	if len(e.rows("agent.records.query")) < 2 {
		t.Fatalf("operator reads must be audited: %+v", e.audit)
	}
	// _agents is never readable, not even for operators
	mustFail(t, op, "records.query", map[string]any{"collection": CollectionName}, "not found or not accessible")
}

func TestDeleteNeedsConfirmToken(t *testing.T) {
	e := setup(t)
	r := e.seed("notes", map[string]any{"title": "bye"})
	other := e.seed("notes", map[string]any{"title": "stay"})
	cs, _ := e.connect(e.agent("w", RoleWriter))

	plan := mustOK(t, cs, "records.delete", map[string]any{"collection": "notes", "id": r.Id, "reason": "cleanup"})
	tok, _ := plan["confirm_token"].(string)
	if tok == "" || plan["confirm_required"] != true {
		t.Fatalf("expected a plan with a token: %v", plan)
	}
	if _, err := e.app.FindRecordById("notes", r.Id); err != nil {
		t.Fatal("dry run must not delete")
	}
	// token bound to the arguments
	mustFail(t, cs, "records.delete", map[string]any{"collection": "notes", "id": other.Id, "reason": "cleanup", "confirm_token": tok}, "does not match")
	// ... and consumed by the failed attempt
	mustFail(t, cs, "records.delete", map[string]any{"collection": "notes", "id": r.Id, "reason": "cleanup", "confirm_token": tok}, "unknown or already used")

	plan = mustOK(t, cs, "records.delete", map[string]any{"collection": "notes", "id": r.Id, "reason": "cleanup"})
	tok = plan["confirm_token"].(string)
	out := mustOK(t, cs, "records.delete", map[string]any{"collection": "notes", "id": r.Id, "reason": "cleanup", "confirm_token": tok})
	if out["deleted"] != true {
		t.Fatalf("delete: %v", out)
	}
	if _, err := e.app.FindRecordById("notes", r.Id); err == nil {
		t.Fatal("record must be gone")
	}
	mustFail(t, cs, "records.delete", map[string]any{"collection": "notes", "id": other.Id, "reason": "cleanup", "confirm_token": tok}, "unknown or already used")
	if len(e.rows("agent.records.delete")) < 3 {
		t.Fatalf("plans and deletes must be audited: %d", len(e.rows("agent.records.delete")))
	}
}

func TestPlanTokenExpires(t *testing.T) {
	e := setup(t)
	r := e.seed("notes", map[string]any{"title": "bye"})
	cs, srv := e.connect(e.agent("w", RoleWriter))
	plan := mustOK(t, cs, "records.delete", map[string]any{"collection": "notes", "id": r.Id, "reason": "cleanup"})
	tok := plan["confirm_token"].(string)
	srv.now = func() time.Time { return time.Now().Add(6 * time.Minute) }
	mustFail(t, cs, "records.delete", map[string]any{"collection": "notes", "id": r.Id, "reason": "cleanup", "confirm_token": tok}, "expired")
}

func TestBatch(t *testing.T) {
	e := setup(t)
	keep := e.seed("notes", map[string]any{"title": "k"})
	cs, _ := e.connect(e.agent("w", RoleWriter))

	// small create-only batch runs directly
	out := mustOK(t, cs, "records.batch", map[string]any{"reason": "bulk", "ops": []any{
		map[string]any{"action": "create", "collection": "notes", "data": map[string]any{"title": "b1"}},
		map[string]any{"action": "create", "collection": "notes", "data": map[string]any{"title": "b2"}},
	}})
	if out["applied"].(float64) != 2 {
		t.Fatalf("batch: %v", out)
	}

	// atomic: the second op fails, the first is rolled back
	n0, _ := e.app.CountRecords("notes")
	mustFail(t, cs, "records.batch", map[string]any{"reason": "bulk", "ops": []any{
		map[string]any{"action": "create", "collection": "notes", "data": map[string]any{"title": "x"}},
		map[string]any{"action": "update", "collection": "notes", "id": "doesnotexist", "data": map[string]any{"title": "y"}},
	}}, "not found")
	n1, _ := e.app.CountRecords("notes")
	if n0 != n1 {
		t.Fatalf("batch must be atomic: %d -> %d", n0, n1)
	}

	// a delete forces a plan
	ops := []any{map[string]any{"action": "delete", "collection": "notes", "id": keep.Id}}
	plan := mustOK(t, cs, "records.batch", map[string]any{"reason": "purge", "ops": ops})
	tok, _ := plan["confirm_token"].(string)
	if tok == "" {
		t.Fatalf("expected plan: %v", plan)
	}
	if _, err := e.app.FindRecordById("notes", keep.Id); err != nil {
		t.Fatal("dry run must not delete")
	}
	mustOK(t, cs, "records.batch", map[string]any{"reason": "purge", "ops": ops, "confirm_token": tok})
	if _, err := e.app.FindRecordById("notes", keep.Id); err == nil {
		t.Fatal("record must be deleted after confirm")
	}

	// more than 100 operations force a plan too
	many := make([]any, 101)
	for i := range many {
		many[i] = map[string]any{"action": "create", "collection": "notes", "data": map[string]any{"title": fmt.Sprint(i)}}
	}
	plan = mustOK(t, cs, "records.batch", map[string]any{"reason": "bulk", "ops": many})
	if plan["confirm_required"] != true {
		t.Fatalf(">100 ops must need a token: %v", plan)
	}
}

func TestRuleExplain(t *testing.T) {
	e := setup(t)
	locked := e.seed("vault", map[string]any{"title": "v"})
	draft := e.seed("docs", map[string]any{"title": "d", "published": false})

	reader, _ := e.connect(e.agent("r", RoleReader))
	out := mustOK(t, reader, "rule.explain", map[string]any{"collection": "vault", "operation": "list"})
	if out["state"] != "locked" || out["rule"] != nil || !strings.Contains(out["meaning"].(string), "superusers") {
		t.Fatalf("locked explain: %v", out)
	}
	out = mustOK(t, reader, "rule.explain", map[string]any{"collection": "notes", "operation": "view"})
	if out["state"] != "public" {
		t.Fatalf("public explain: %v", out)
	}
	out = mustOK(t, reader, "rule.explain", map[string]any{"collection": "docs", "operation": "list"})
	if out["state"] != "expression" || out["rule"] != "published = true" {
		t.Fatalf("expression explain: %v", out)
	}
	mustFail(t, reader, "rule.explain", map[string]any{"collection": "vault", "operation": "view", "record_id": locked.Id}, "needs role operator")
	mustFail(t, reader, "rule.explain", map[string]any{"collection": "vault", "operation": "bogus"}, "unknown operation")

	op, _ := e.connect(e.agent("op", RoleOperator))
	out = mustOK(t, op, "rule.explain", map[string]any{"collection": "vault", "operation": "view", "record_id": locked.Id})
	passes := out["passes"].(map[string]any)
	if passes["guest"] != false || passes["agent"] != true {
		t.Fatalf("locked vault: guest must fail, operator agent passes: %v", passes)
	}
	out = mustOK(t, op, "rule.explain", map[string]any{"collection": "docs", "operation": "view", "record_id": draft.Id})
	if out["passes"].(map[string]any)["guest"] != false {
		t.Fatalf("draft must not pass for guest: %v", out)
	}
}

func TestOperatorOnlyTools(t *testing.T) {
	e := setup(t)
	called := 0
	SetProviders(Providers{
		AuditVerify: func(_ core.App) (any, error) { called++; return map[string]any{"ok": true}, nil },
	})
	w, _ := e.connect(e.agent("w", RoleWriter))
	for _, tool := range []string{"audit.tail", "audit.verify", "backup.verify", "replica.status", "deny.tail", "lockout.list"} {
		mustFail(t, w, tool, nil, "needs role operator")
	}
	op, _ := e.connect(e.agent("op", RoleOperator))
	mustOK(t, op, "audit.verify", nil)
	if called != 1 {
		t.Fatal("provider not called")
	}
	mustFail(t, op, "lockout.list", nil, "not enabled")
}

func TestDescribeSamples(t *testing.T) {
	e := setup(t)
	e.seed("notes", map[string]any{"title": strings.Repeat("x", 100), "api_key": "topsecret"})
	reader, _ := e.connect(e.agent("r", RoleReader))
	out := mustOK(t, reader, "schema.describe", map[string]any{"collection": "notes"})
	if _, isStr := out["samples"].(string); !isStr {
		t.Fatalf("reader must not get samples: %v", out["samples"])
	}
	if out["rules"].(map[string]any)["list"] != "" {
		t.Fatalf("rules missing: %v", out["rules"])
	}
	op, _ := e.connect(e.agent("op", RoleOperator))
	out = mustOK(t, op, "schema.describe", map[string]any{"collection": "notes"})
	s := out["samples"].([]any)[0].(map[string]any)
	if s["api_key"] != "[redacted]" {
		t.Fatalf("api_key must be redacted: %v", s)
	}
	if got := s["title"].(string); len([]rune(got)) != 41 {
		t.Fatalf("title must be cut to 40 chars + ellipsis: %q", got)
	}
}

func TestRateLimit(t *testing.T) {
	e := setup(t)
	a, _, err := CreateAgent(e.app, "fast", RoleReader, nil, 3)
	if err != nil {
		t.Fatal(err)
	}
	cs, _ := e.connect(a)
	for i := 0; i < 3; i++ {
		mustOK(t, cs, "schema.list", nil)
	}
	mustFail(t, cs, "schema.list", nil, "rate limit exceeded")
}

func TestBucketRefills(t *testing.T) {
	var b bucket
	now := time.Now()
	for i := 0; i < 60; i++ {
		if !b.allow(now, 60) {
			t.Fatalf("call %d should pass", i)
		}
	}
	if b.allow(now, 60) {
		t.Fatal("bucket must be empty")
	}
	if !b.allow(now.Add(2*time.Second), 60) {
		t.Fatal("bucket must refill 1 token/second")
	}
}

func TestResourcesAndPrompts(t *testing.T) {
	e := setup(t)
	SetProviders(Providers{Version: "v-test", Profile: "solo", Modules: []string{"audit", "mcp"}})
	cs, _ := e.connect(e.agent("scoped", RoleReader, "notes"))
	ctx := context.Background()

	res, err := cs.ReadResource(ctx, &sdk.ReadResourceParams{URI: "toki://schema"})
	if err != nil {
		t.Fatal(err)
	}
	txt := res.Contents[0].Text
	if !strings.Contains(txt, `"notes"`) || strings.Contains(txt, `"vault"`) || strings.Contains(txt, CollectionName) {
		t.Fatalf("schema resource must follow the allowlist: %s", txt)
	}
	res, err = cs.ReadResource(ctx, &sdk.ReadResourceParams{URI: "toki://docs/rules"})
	if err != nil || !strings.Contains(res.Contents[0].Text, "@request.auth") {
		t.Fatalf("rules doc: %v", err)
	}
	res, err = cs.ReadResource(ctx, &sdk.ReadResourceParams{URI: "toki://instance"})
	if err != nil || !strings.Contains(res.Contents[0].Text, "v-test") || !strings.Contains(res.Contents[0].Text, "audit") {
		t.Fatalf("instance: %v %v", err, res)
	}

	pr, err := cs.GetPrompt(ctx, &sdk.GetPromptParams{Name: "write-rule", Arguments: map[string]string{
		"collection": "notes", "operation": "list", "intent": "owners only",
	}})
	if err != nil || !strings.Contains(pr.Messages[0].Content.(*sdk.TextContent).Text, "owners only") {
		t.Fatalf("write-rule prompt: %v", err)
	}
	for _, name := range []string{"debug-403", "design-schema"} {
		if _, err := cs.GetPrompt(ctx, &sdk.GetPromptParams{Name: name, Arguments: map[string]string{"collection": "notes", "operation": "view", "goal": "blog"}}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestToolNames(t *testing.T) {
	e := setup(t)
	cs, _ := e.connect(e.agent("r", RoleReader))
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"schema.list", "schema.describe", "records.query", "records.get", "records.create", "records.update",
		"records.delete", "records.batch", "rule.lint", "rule.explain", "audit.tail", "audit.verify", "backup.verify",
		"replica.status", "deny.tail", "lockout.list"}
	have := map[string]bool{}
	for _, tl := range res.Tools {
		have[tl.Name] = true
	}
	for _, n := range want {
		if !have[n] {
			t.Errorf("missing tool %s", n)
		}
	}
}

func TestGenerateDocs(t *testing.T) {
	e := setup(t)
	SetProviders(Providers{Modules: []string{"audit", "mcp"}})
	md, llms, err := GenerateDocs(e.app)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"### notes", "### vault", "toki mcp serve", "- audit", "locked", "PUBLIC"} {
		if !strings.Contains(md, want) {
			t.Errorf("AGENTS.md misses %q", want)
		}
	}
	if strings.Contains(md, CollectionName) || strings.Contains(md, "_superusers") {
		t.Error("system collections must not be listed")
	}
	if !strings.Contains(llms, "notes") || !strings.Contains(llms, "claude mcp add") {
		t.Errorf("llms.txt: %s", llms)
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Time{
		"":           {},
		"1h":         now.Add(-time.Hour),
		"2d":         now.Add(-48 * time.Hour),
		"2026-10-01": time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
	} {
		got, err := parseSince(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	if _, err := parseSince("soon", now); err == nil {
		t.Error("invalid since must fail")
	}
}
