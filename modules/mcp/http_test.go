//go:build !no_mcp

package mcp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tests"
)

type bearerRT struct{ key string }

func (b bearerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if b.key != "" {
		r.Header.Set("Authorization", "Bearer "+b.key)
	}
	return http.DefaultTransport.RoundTrip(r)
}

func (e *env) agentKey(name string, role Role, opts AgentOptions, cols ...string) (*Agent, string) {
	e.t.Helper()
	a, key, err := CreateAgentOpts(e.app, name, role, cols, 0, opts)
	if err != nil {
		e.t.Fatal(err)
	}
	return a, key
}

func (e *env) setEnabled(id string, v bool) {
	e.t.Helper()
	coll, _ := e.app.FindCollectionByNameOrId(CollectionName)
	rec, err := e.app.FindRecordById(coll, id)
	if err != nil {
		e.t.Fatal(err)
	}
	rec.Set("enabled", v)
	if err := e.app.Save(rec); err != nil {
		e.t.Fatal(err)
	}
}

const initBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`

func TestHTTPTransportAuth(t *testing.T) {
	t.Setenv("TOKI_MCP", "on")
	var okKey, offKey, expKey string
	factory := func(tb testing.TB) *tests.TestApp {
		app, err := tests.NewTestApp()
		if err != nil {
			tb.Fatal(err)
		}
		Register(app)
		RegisterHTTP(app)
		mk := func(name string, opts AgentOptions) (*Agent, string) {
			a, k, err := CreateAgentOpts(app, name, RoleReader, nil, 0, opts)
			if err != nil {
				tb.Fatal(err)
			}
			return a, k
		}
		_, okKey = mk("http-ok", AgentOptions{})
		off, k := mk("http-off", AgentOptions{})
		offKey = k
		coll, _ := app.FindCollectionByNameOrId(CollectionName)
		rec, _ := app.FindRecordById(coll, off.ID)
		rec.Set("enabled", false)
		if err := app.Save(rec); err != nil {
			tb.Fatal(err)
		}
		_, expKey = mk("http-exp", AgentOptions{Expires: time.Now().Add(-time.Hour)})
		return app
	}
	hdr := func(key string) map[string]string {
		h := map[string]string{"Content-Type": "application/json", "Accept": "application/json, text/event-stream"}
		if key != "" {
			h["Authorization"] = "Bearer " + key
		}
		return h
	}
	// keys are only known after the factory ran: resolve lazily through BeforeTestFunc-free closures
	scenarios := []struct {
		name   string
		key    func() string
		status int
		want   string
	}{
		{"missing key", func() string { return "" }, 401, "invalid agent key"},
		{"invalid key", func() string { return "tka_" + strings.Repeat("x", 40) }, 401, "invalid agent key"},
		{"not an agent key", func() string { return "eyJhbGciOi" }, 401, "invalid agent key"},
		{"disabled agent", func() string { return offKey }, 403, "revoked"},
		{"expired agent", func() string { return expKey }, 403, "expired"},
		{"valid key", func() string { return okKey }, 200, "protocolVersion"},
	}
	for _, sc := range scenarios {
		sc := sc
		var s tests.ApiScenario
		s = tests.ApiScenario{
			Name:            sc.name,
			Method:          http.MethodPost,
			URL:             "/api/mcp",
			Body:            strings.NewReader(initBody),
			TestAppFactory:  factory,
			ExpectedStatus:  sc.status,
			ExpectedContent: []string{sc.want},
			BeforeTestFunc: func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
				// headers are read after BeforeTestFunc when the request is built
				s.Headers = hdr(sc.key())
			},
		}
		s.Test(t)
	}
}

func TestHTTPToolsList(t *testing.T) {
	e := setup(t)
	_, key := e.agentKey("viahttp", RoleReader, AgentOptions{})
	srv := httptest.NewServer(NewHTTPHandler(e.app))
	defer srv.Close()

	connect := func(key string) (*sdk.ClientSession, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		t.Cleanup(cancel)
		return sdk.NewClient(&sdk.Implementation{Name: "t"}, nil).Connect(ctx, &sdk.StreamableClientTransport{
			Endpoint: srv.URL, HTTPClient: &http.Client{Transport: bearerRT{key}}, DisableStandaloneSSE: true,
		}, nil)
	}
	if _, err := connect(""); err == nil {
		t.Fatal("connect without key must fail")
	}
	if _, err := connect("tka_" + strings.Repeat("y", 40)); err == nil {
		t.Fatal("connect with a bad key must fail")
	}
	cs, err := connect(key)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil || len(tools.Tools) == 0 {
		t.Fatalf("tools/list: %v %v", tools, err)
	}
	out := mustOK(t, cs, "records.query", map[string]any{"collection": "notes"})
	if _, ok := out["items"]; !ok {
		t.Fatalf("records.query over HTTP: %v", out)
	}

	// revocation applies to the running session on its next call
	a, _ := Authenticate(e.app, key)
	e.setEnabled(a.ID, false)
	mustFail(t, cs, "records.query", map[string]any{"collection": "notes"}, "Forbidden")
}

func TestHTTPWriteIsAuditedWithTransport(t *testing.T) {
	e := setup(t)
	_, key := e.agentKey("httpw", RoleWriter, AgentOptions{})
	srv := httptest.NewServer(NewHTTPHandler(e.app))
	defer srv.Close()
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "t"}, nil).Connect(context.Background(), &sdk.StreamableClientTransport{
		Endpoint: srv.URL, HTTPClient: &http.Client{Transport: bearerRT{key}}, DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	mustOK(t, cs, "records.create", map[string]any{"collection": "notes", "data": map[string]any{"title": "x"}, "reason": "http test"})
	rows := e.rows("agent.records.create")
	if len(rows) != 1 || rows[0].details["transport"] != "http" {
		t.Fatalf("audit: %+v", rows)
	}
}

func (e *env) kindCollections() *kernel.Record {
	e.t.Helper()
	mk := func(name string, rule string) {
		c := kernel.NewBaseCollection(name)
		c.Fields.Add(&kernel.TextField{Name: "title"})
		c.ListRule, c.ViewRule = strp(rule), strp(rule)
		if err := e.app.Save(c); err != nil {
			e.t.Fatal(err)
		}
		e.seed(name, map[string]any{"title": name})
	}
	mk("only_agents", `@request.auth.kind = "agent"`)
	mk("only_users", `@request.auth.kind = "user"`)
	mk("only_readers", `@request.auth.kind = "agent" && @request.auth.agent.role = "reader"`)
	mk("only_writers", `@request.auth.kind = "agent" && @request.auth.agent.role = "writer"`)
	// user-field rules must not match an agent (namespaced attributes)
	mk("plain_role_writer", `@request.auth.role = "writer"`)
	mk("plain_name", `@request.auth.name != ""`)
	mk("plain_key_hash", `@request.auth.agent.key_hash != ""`)
	mk("any_auth", `@request.auth.id != ""`)

	members := kernel.NewAuthCollection("members")
	if err := e.app.Save(members); err != nil {
		e.t.Fatal(err)
	}
	u := kernel.NewRecord(members)
	u.SetEmail("m@example.com")
	u.SetPassword("1234567890")
	if err := e.app.Save(u); err != nil {
		e.t.Fatal(err)
	}
	return u
}

func TestAuthKindInRules(t *testing.T) {
	e := setup(t)
	user := e.kindCollections()
	cs, _ := e.connect(e.agent("kr", RoleReader))
	count := func(col string) float64 {
		out, msg := callTool(t, cs, "records.query", map[string]any{"collection": col})
		if msg != "" {
			t.Fatalf("%s: %s", col, msg)
		}
		return out["totalItems"].(float64)
	}
	for col, want := range map[string]float64{
		"only_agents": 1, "only_users": 0, "only_readers": 1, "only_writers": 0, "any_auth": 1,
		"plain_role_writer": 0, "plain_name": 0, "plain_key_hash": 0,
	} {
		if got := count(col); got != want {
			t.Errorf("agent on %s: got %v items, want %v", col, got, want)
		}
	}
	// a writer agent matches the writer rule only
	ws, _ := e.connect(e.agent("kw", RoleWriter))
	out := mustOK(t, ws, "records.query", map[string]any{"collection": "only_writers"})
	if out["totalItems"].(float64) != 1 {
		t.Fatalf("writer on only_writers: %v", out)
	}
	// `@request.auth.role = "writer"` must NOT match a writer agent
	out = mustOK(t, ws, "records.query", map[string]any{"collection": "plain_role_writer"})
	if out["totalItems"].(float64) != 0 {
		t.Fatalf("writer agent matched @request.auth.role: %v", out)
	}
	// get honours the same rule
	rec, _ := e.app.FindFirstRecordByData("only_users", "title", "only_users")
	mustFail(t, cs, "records.get", map[string]any{"collection": "only_users", "id": rec.Id}, "not accessible")

	// the same rules as seen by a user and a guest through the kernel
	eval := func(col string, info *kernel.RequestInfo) bool {
		c, _ := e.app.FindCollectionByNameOrId(col)
		r, _ := e.app.FindFirstRecordByData(c, "title", col)
		ok, err := e.app.CanAccessRecord(r, info, c.ListRule)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	ui := &kernel.RequestInfo{Auth: user, Query: map[string]string{}, Headers: map[string]string{}, Body: map[string]any{}}
	gi := &kernel.RequestInfo{Query: map[string]string{}, Headers: map[string]string{}, Body: map[string]any{}}
	for col, want := range map[string][2]bool{ // {user, guest}
		"only_agents": {false, false}, "only_users": {true, false}, "any_auth": {true, false},
	} {
		if got := eval(col, ui); got != want[0] {
			t.Errorf("user on %s: %v", col, got)
		}
		if got := eval(col, gi); got != want[1] {
			t.Errorf("guest on %s: %v", col, got)
		}
	}
}

func TestSandboxRollsBackWrites(t *testing.T) {
	e := setup(t)
	a, _ := e.agentKey("sbx", RoleWriter, AgentOptions{Sandbox: true})
	if !a.Sandbox {
		t.Fatal("sandbox flag not stored")
	}
	cs, _ := e.connect(a)
	count := func() int {
		n, err := e.app.CountRecords("notes")
		if err != nil {
			t.Fatal(err)
		}
		return int(n)
	}
	existing := e.seed("notes", map[string]any{"title": "keep"})

	out := mustOK(t, cs, "records.create", map[string]any{"collection": "notes", "data": map[string]any{"title": "ghost"}, "reason": "dry run"})
	if out["sandbox"] != true || out["dry_run"] != true || out["rolled_back"] != true {
		t.Fatalf("create must be flagged: %v", out)
	}
	rec, _ := out["record"].(map[string]any)
	if rec["title"] != "ghost" || rec["id"] == "" {
		t.Fatalf("create must return the would-be record: %v", out)
	}
	if count() != 1 {
		t.Fatalf("sandbox create persisted: %d", count())
	}

	out = mustOK(t, cs, "records.update", map[string]any{"collection": "notes", "id": existing.Id, "data": map[string]any{"title": "changed"}, "reason": "dry run"})
	if r, _ := out["record"].(map[string]any); r["title"] != "changed" {
		t.Fatalf("update must return the would-be record: %v", out)
	}
	fresh, _ := e.app.FindRecordById("notes", existing.Id)
	if fresh.GetString("title") != "keep" {
		t.Fatalf("sandbox update persisted: %q", fresh.GetString("title"))
	}

	args := map[string]any{"collection": "notes", "id": existing.Id, "reason": "dry run"}
	plan := mustOK(t, cs, "records.delete", args)
	args["confirm_token"] = plan["confirm_token"]
	out = mustOK(t, cs, "records.delete", args)
	if out["deleted"] != true || out["rolled_back"] != true {
		t.Fatalf("delete result: %v", out)
	}
	if count() != 1 {
		t.Fatalf("sandbox delete persisted: %d", count())
	}

	out = mustOK(t, cs, "records.batch", map[string]any{"reason": "dry run", "ops": []any{
		map[string]any{"action": "create", "collection": "notes", "data": map[string]any{"title": "b1"}},
		map[string]any{"action": "create", "collection": "notes", "data": map[string]any{"title": "b2"}},
	}})
	if out["applied"].(float64) != 2 || out["rolled_back"] != true {
		t.Fatalf("batch result: %v", out)
	}
	if count() != 1 {
		t.Fatalf("sandbox batch persisted: %d", count())
	}

	// the audit rows say it was a dry run
	rows := e.rows("agent.records.create")
	if len(rows) != 1 || rows[0].details["dry_run"] != true || rows[0].details["sandbox"] != true {
		t.Fatalf("audit: %+v", rows)
	}
	// a failing write still fails (validation runs inside the sandbox)
	mustFail(t, cs, "records.create", map[string]any{"collection": "notes", "data": map[string]any{"nope": 1}, "reason": "dry run"}, "unknown field")
}

func TestParseExpires(t *testing.T) {
	now := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	for in, ok := range map[string]bool{"": true, "90d": true, "2026-12-31": true, "2026-12-31T10:00:00Z": true, "soon": false, "0d": false} {
		if _, err := parseExpires(in, now); (err == nil) != ok {
			t.Errorf("%q: err=%v", in, err)
		}
	}
}

func TestHTTPHostBehindProxy(t *testing.T) {
	e := setup(t)
	_, key := e.agentKey("proxied", RoleReader, AgentOptions{})
	srv := httptest.NewServer(NewHTTPHandler(e.app)) // loopback listener
	defer srv.Close()
	do := func(host string) int {
		req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(initBody))
		req.Host = host
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		return res.StatusCode
	}
	if got := do("evil.example.org"); got != 403 {
		t.Fatalf("unknown public host: %d", got)
	}
	if got := do("localhost:8090"); got != 200 {
		t.Fatalf("loopback host: %d", got)
	}
	e.app.Settings().Meta.AppURL = "https://mcp.example.com"
	if got := do("mcp.example.com"); got != 200 {
		t.Fatalf("host of the app URL: %d", got)
	}
	t.Setenv(EnvAllowedHosts, "other.example.net, x.test")
	if got := do("other.example.net"); got != 200 {
		t.Fatalf("allowlisted host: %d", got)
	}
	if got := do("evil.example.org"); got != 403 {
		t.Fatalf("still unknown: %d", got)
	}
}

func TestHTTPThrottleKeying(t *testing.T) {
	e := setup(t)
	_, key := e.agentKey("thr", RoleReader, AgentOptions{})
	h := NewHTTPHandler(e.app)
	do := func(k, remote string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/mcp", strings.NewReader(initBody))
		req.RemoteAddr = remote
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if k != "" {
			req.Header.Set("Authorization", "Bearer "+k)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	// requests without a header never count
	for i := 0; i < failMax+5; i++ {
		if got := do("", "10.0.0.1:1"); got != 401 {
			t.Fatalf("no header: %d", got)
		}
	}
	bad := "tka_" + strings.Repeat("z", 40)
	for i := 0; i < failMax; i++ {
		do(bad, "10.0.0.1:1")
	}
	if got := do(bad, "10.0.0.1:2"); got != 429 {
		t.Fatalf("bad key over the limit: %d", got)
	}
	// a valid key from the same saturated address still passes
	if got := do(key, "10.0.0.1:3"); got != 200 {
		t.Fatalf("valid key must not be throttled: %d", got)
	}
	// another address has its own bucket
	if got := do(bad, "10.0.0.2:1"); got != 401 {
		t.Fatalf("other address: %d", got)
	}
	// IPv6 addresses of one /64 share a bucket
	for i := 0; i < failMax; i++ {
		do(bad, "[2001:db8::1]:1")
	}
	if got := do(bad, "[2001:db8::ffff]:1"); got != 429 {
		t.Fatalf("same /64: %d", got)
	}
}

func TestFailMapIsBounded(t *testing.T) {
	h := &httpHandler{now: time.Now, fails: map[string]*failState{}}
	for i := 0; i < failMaxEntries*3; i++ {
		h.fail(fmt.Sprintf("ip:%d", i))
	}
	if len(h.fails) > failMaxEntries {
		t.Fatalf("fails map grew to %d", len(h.fails))
	}
}

func TestParseExpiresStrict(t *testing.T) {
	now := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	for _, in := range []string{"7xd", "2020-01-01", "2020-01-01T00:00:00Z", "-3d"} {
		if _, err := parseExpires(in, now); err == nil {
			t.Errorf("%q must be rejected", in)
		}
	}
}
