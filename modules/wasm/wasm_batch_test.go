//go:build !no_wasm && !no_batchguard

package wasm

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/batchguard"
)

func batchTOML(mode, events string, timeoutMS ...int) string {
	to := 1500
	if len(timeoutMS) > 0 {
		to = timeoutMS[0]
	}
	return "events = [" + events + "]\ntimeout_ms = " + fmt.Sprint(to) + "\n[env]\nMODE = \"" + mode + "\"\nMAX = \"5\"\n"
}

// batchEnv is newEnv plus batchguard (the emitter of the batch events), the
// order_items collection and batches switched on.
func batchEnv(t *testing.T, mods ...modSpec) *env {
	t.Helper()
	e := newEnv(t, mods...)
	batchguard.Register(e.app)
	if err := batchguard.EnsureCollection(e.app); err != nil {
		t.Fatal(err)
	}
	open := ""
	c := core.NewBaseCollection("order_items")
	c.Fields.Add(&core.NumberField{Name: "qty"}, &core.TextField{Name: "vault"})
	c.CreateRule, c.ListRule, c.ViewRule, c.UpdateRule, c.DeleteRule = &open, &open, &open, &open, &open
	if err := e.app.Save(c); err != nil {
		t.Fatal(err)
	}
	s := e.app.Settings()
	s.Batch.Enabled = true
	s.Batch.MaxRequests = 50
	if err := e.app.Save(s); err != nil {
		t.Fatal(err)
	}
	return e
}

func itemsBatch(qtys ...int) string {
	var reqs []string
	for _, q := range qtys {
		reqs = append(reqs, fmt.Sprintf(`{"method":"POST","url":"/api/collections/order_items/records","body":{"qty":%d}}`, q))
	}
	return `{"requests":[` + strings.Join(reqs, ",") + `]}`
}

func rowCount(t *testing.T, e *env, coll string) int {
	t.Helper()
	n, err := e.app.CountRecords(coll)
	if err != nil {
		t.Fatal(err)
	}
	return int(n)
}

func TestBatchGuestRejectsAndAllows(t *testing.T) {
	for _, phase := range []string{"batch.before", "batch.after", "batch.*"} {
		t.Run(phase, func(t *testing.T) {
			e := batchEnv(t, modSpec{"cap", guest(t, "batch"), batchTOML("maxqty", `"`+phase+`"`)})
			code, body := e.do("POST", "/api/batch", itemsBatch(2, 3), "")
			if code != 200 {
				t.Fatalf("under the cap: %d %s", code, body)
			}
			if n := rowCount(t, e, "order_items"); n != 2 {
				t.Fatalf("rows %d", n)
			}
			code, body = e.do("POST", "/api/batch", itemsBatch(4, 3), "")
			if code != 422 || !strings.Contains(body, "oo many items: 7 > 5") {
				t.Fatalf("over the cap: %d %s", code, body)
			}
			if n := rowCount(t, e, "order_items"); n != 2 {
				t.Fatalf("rejected batch must roll back, rows %d", n)
			}
			// the same cap on plain (non batch) writes is not involved
			if code, body := e.do("POST", "/api/collections/order_items/records", `{"qty":99}`, ""); code != 200 {
				t.Fatalf("single write: %d %s", code, body)
			}
		})
	}
}

func TestBatchAfterSeesStoredValuesAndRollsBack(t *testing.T) {
	e := batchEnv(t, modSpec{"cap", guest(t, "batch"), batchTOML("echo", `"batch.after"`)})
	code, body := e.do("POST", "/api/batch", itemsBatch(2), "")
	if code != 418 {
		t.Fatalf("want guest rejection, got %d %s", code, body)
	}
	var out struct{ Message string }
	_ = json.Unmarshal([]byte(body), &out)
	var ev struct {
		Phase string
		Batch struct{ Requests []BatchRequestIn }
	}
	if err := json.Unmarshal([]byte(strings.TrimSuffix(out.Message, ".")), &ev); err != nil {
		t.Fatalf("%v: %s", err, out.Message)
	}
	r := ev.Batch.Requests
	if ev.Phase != "after" || len(r) != 1 || r[0].ID == "" || r[0].Body["qty"] != float64(2) || r[0].Collection != "order_items" ||
		r[0].Path != "/api/collections/order_items/records/"+r[0].ID || r[0].Method != "POST" {
		t.Fatalf("after payload: %s", out.Message)
	}
	if n := rowCount(t, e, "order_items"); n != 0 {
		t.Fatalf("batch.after rejection must roll back, rows %d", n)
	}
}

func TestBatchAllowGuestPassesAndTrapFailsClosed(t *testing.T) {
	e := batchEnv(t, modSpec{"ok", guest(t, "batch"), batchTOML("allow", `"batch.before"`)})
	if code, body := e.do("POST", "/api/batch", itemsBatch(50), ""); code != 200 {
		t.Fatalf("allow: %d %s", code, body)
	}
	e = batchEnv(t, modSpec{"bad", guest(t, "batch"), batchTOML("trap", `"batch.before"`)})
	code, body := e.do("POST", "/api/batch", itemsBatch(1), "")
	if code != 500 || !strings.Contains(body, "Hook failed.") || strings.Contains(body, "exit") {
		t.Fatalf("trap must fail closed with a generic message: %d %s", code, body)
	}
	if n := rowCount(t, e, "order_items"); n != 0 {
		t.Fatalf("rows %d", n)
	}
}

func TestBatchTimeoutFailsClosed(t *testing.T) {
	e := batchEnv(t, modSpec{"spin", guest(t, "batch"), batchTOML("spin", `"batch.before"`, 300)})
	start := time.Now()
	var code int
	var body string
	within(t, 10*time.Second, "batch with a spinning guest", func() {
		code, body = e.do("POST", "/api/batch", itemsBatch(1), "")
	})
	if code != 500 || !strings.Contains(body, "Hook failed.") {
		t.Fatalf("timeout must fail closed: %d %s", code, body)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("timeout not enforced: %s", d)
	}
	if n := rowCount(t, e, "order_items"); n != 0 {
		t.Fatalf("rows %d", n)
	}
	if _, errs := e.calls("spin"); errs != 1 {
		t.Fatalf("errors %d", errs)
	}
}

func TestBatchPayloadIsRedacted(t *testing.T) {
	e := batchEnv(t, modSpec{"echo", guest(t, "batch"), batchTOML("echo", `"batch.before"`)})
	col, err := e.app.FindCollectionByNameOrId("order_items")
	if err != nil {
		t.Fatal(err)
	}
	kernel.RegisterSensitiveField(col.Id, "vault")
	t.Cleanup(func() { kernel.UnregisterSensitiveField(col.Id, "vault") })

	body := `{"requests":[{"method":"POST","url":"/api/collections/order_items/records","body":{"qty":1,"vault":"s3cr3t-value","password":"pw-123","note":{"token":"tok-9","keep":"visible"}}},
		{"method":"POST","url":"/api/collections/order_items/records","body":{"qty":2,"vault":"","vault:x":"s3cr3t-2"}}]}`
	code, resp := e.do("POST", "/api/batch", body, "")
	if code != 418 {
		t.Fatalf("%d %s", code, resp)
	}
	for _, leak := range []string{"s3cr3t", "pw-123", "tok-9"} {
		if strings.Contains(resp, leak) {
			t.Errorf("payload leaks %q: %s", leak, resp)
		}
	}
	if !strings.Contains(resp, kernel.SensitiveMarker) || !strings.Contains(resp, "visible") {
		t.Errorf("expected marker and harmless values: %s", resp)
	}
}

func TestBuildBatchInRedactionAndAuth(t *testing.T) {
	e := batchEnv(t)
	col, _ := e.app.FindCollectionByNameOrId("order_items")
	kernel.RegisterSensitiveField(col.Id, "vault")
	defer kernel.UnregisterSensitiveField(col.Id, "vault")
	orig := map[string]any{"vault+": "x", "+vault": "y", "vault-": "z", "qty": 1, "Password": "p", "vault": ""}
	su, err := e.app.FindAuthRecordByEmail(core.CollectionNameSuperusers, "test@example.com")
	if err != nil {
		t.Fatal(err)
	}
	in := buildBatchIn(&kernel.BatchEvent{Name: kernel.BatchBefore, App: e.app, Auth: su,
		Requests: []kernel.BatchRequest{{Index: 0, Collection: "order_items", Method: "PATCH", ID: "abc", Body: orig}}})
	b := in.Requests[0].Body
	for _, k := range []string{"vault+", "+vault", "vault-"} {
		if b[k] != kernel.SensitiveMarker {
			t.Errorf("%s not redacted: %v", k, b)
		}
	}
	if _, ok := b["Password"]; ok || b["qty"] != 1 || b["vault"] != "" {
		t.Errorf("unexpected body: %v", b)
	}
	if orig["vault+"] != "x" {
		t.Error("original body mutated")
	}
	if in.Auth == nil || !in.Auth.Superuser || in.Auth.ID != su.Id || in.Requests[0].Path != "/api/collections/order_items/records/abc" {
		t.Errorf("auth/path: %+v", in)
	}
	if g := buildBatchIn(&kernel.BatchEvent{Name: kernel.BatchBefore, App: e.app}); g.Auth != nil || len(g.Requests) != 0 {
		t.Errorf("anonymous: %+v", g)
	}
}

func TestBatchHandlerOnlyBoundWhileNeeded(t *testing.T) {
	e := batchEnv(t)
	hooks := kernel.OnBatchFor(e.app)
	if hooks.Length() != 0 {
		t.Fatalf("no batch module: handler must not be bound (%d)", hooks.Length())
	}
	// hot reload: dropping a module in binds the handler, removing it unbinds
	copyFile(t, guest(t, "batch"), filepath.Join(e.dir, "cap.wasm"))
	if err := os.WriteFile(filepath.Join(e.dir, "cap.toml"), []byte(batchTOML("maxqty", `"batch.before"`)), 0o644); err != nil {
		t.Fatal(err)
	}
	e.h.Reload()
	if hooks.Length() != 1 {
		t.Fatalf("bound handlers %d: %v", hooks.Length(), e.h.LoadErrors())
	}
	if code, body := e.do("POST", "/api/batch", itemsBatch(9), ""); code != 422 {
		t.Fatalf("live module: %d %s", code, body)
	}
	os.Remove(filepath.Join(e.dir, "cap.wasm"))
	e.h.Reload()
	if hooks.Length() != 0 {
		t.Fatalf("handler still bound after the module was removed")
	}
	if code, body := e.do("POST", "/api/batch", itemsBatch(9), ""); code != 200 {
		t.Fatalf("after removal: %d %s", code, body)
	}
}

func TestBatchOversizedPayloadRefused(t *testing.T) {
	e := batchEnv(t, modSpec{"ok", guest(t, "batch"), batchTOML("allow", `"batch.before"`)})
	big := strings.Repeat("x", 1<<20)
	var reqs []string
	for i := 0; i < 5; i++ {
		reqs = append(reqs, fmt.Sprintf(`{"method":"POST","url":"/api/collections/order_items/records","body":{"qty":1,"vault":"%s"}}`, big))
	}
	code, body := e.do("POST", "/api/batch", `{"requests":[`+strings.Join(reqs, ",")+`]}`, "")
	if code != 413 {
		t.Fatalf("want 413, got %d %.200s", code, body)
	}
	if n := rowCount(t, e, "order_items"); n != 0 {
		t.Fatalf("rows %d", n)
	}
}

func TestParseBatchEventsAndHTTPAllow(t *testing.T) {
	for _, ok := range []string{"batch.before", "batch.after", "batch.*"} {
		if ev, err := ParseEvent(ok); err != nil || ev.Kind != KindBatch {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"batch.", "batch.during", "batch"} {
		if _, err := ParseEvent(bad); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
	m, err := ParseManifest("x", "x.wasm", `http_allow = ["api.example.com", "*.cdn.example.com"]`)
	if err != nil || len(m.HTTPAllow) != 2 {
		t.Fatalf("%+v %v", m, err)
	}
	for _, bad := range []string{`http_allow = ["https://x.com"]`, `http_allow = ["a*.com"]`, `http_allow = [""]`, `http_allow = "x"`} {
		if _, err := ParseManifest("x", "x.wasm", bad); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
	if !hostMatches(m.HTTPAllow, "API.example.com") || !hostMatches(m.HTTPAllow, "a.cdn.example.com") ||
		hostMatches(m.HTTPAllow, "cdn.example.com") || hostMatches(m.HTTPAllow, "evil.com") {
		t.Error("hostMatches")
	}
}

func TestBatchAfterHidesHiddenFields(t *testing.T) {
	e := batchEnv(t)
	col, _ := e.app.FindCollectionByNameOrId("order_items")
	col.Fields.Add(&core.TextField{Name: "private_note", Hidden: true})
	if err := e.app.Save(col); err != nil {
		t.Fatal(err)
	}
	data := map[string]any{"qty": 1, "private_note": "s3cret", "tokenKey": "k", "vault": ""}
	after := buildBatchIn(&kernel.BatchEvent{Name: kernel.BatchAfter, App: e.app,
		Requests: []kernel.BatchRequest{{Collection: "order_items", Method: "POST", ID: "abc", Body: data}}})
	b := after.Requests[0].Body
	if _, ok := b["private_note"]; ok {
		t.Fatalf("hidden field reached the guest: %v", b)
	}
	if _, ok := b["tokenKey"]; ok || b["qty"] != 1 {
		t.Fatalf("unexpected body: %v", b)
	}
	// the submission (batch.before) is shown as sent
	before := buildBatchIn(&kernel.BatchEvent{Name: kernel.BatchBefore, App: e.app,
		Requests: []kernel.BatchRequest{{Collection: "order_items", Method: "POST", Body: data}}})
	if before.Requests[0].Body["private_note"] != "s3cret" {
		t.Fatalf("before must keep the submitted body: %v", before.Requests[0].Body)
	}
}

func TestBatchTotalBudgetFailsClosed(t *testing.T) {
	t.Setenv(EnvBatchBudget, "300ms")
	e := batchEnv(t, modSpec{"spin", guest(t, "batch"), batchTOML("spin", `"batch.before"`, 60000)})
	start := time.Now()
	var code int
	var body string
	within(t, 15*time.Second, "batch with a spinning guest", func() {
		code, body = e.do("POST", "/api/batch", itemsBatch(1), "")
	})
	if code != 500 || !strings.Contains(body, "Hook failed.") {
		t.Fatalf("budget overrun must fail closed: %d %s", code, body)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("budget not enforced: %s", d)
	}
	if n := rowCount(t, e, "order_items"); n != 0 {
		t.Fatalf("rows %d", n)
	}
}

func TestHTTPAllowWildcardNeedsLabel(t *testing.T) {
	if hostMatches([]string{"*.example.com"}, "evil.com.") {
		t.Error("trailing dot host matched")
	}
	if !hostMatches([]string{"*.example.com"}, "a.example.com.") {
		t.Error("fqdn form must match")
	}
	if _, err := ParseManifest("m", "m.toml", "events = [\"batch.before\"]\nhttp_allow = [\"*.\"]\n"); err == nil {
		t.Error("`*.` must be refused")
	}
}
