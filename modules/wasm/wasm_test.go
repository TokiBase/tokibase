//go:build !no_wasm

package wasm

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/tests"
	"github.com/tokibase/tokibase/tools/router"
)

// ---- guest build cache ----

var (
	guestMu   sync.Mutex
	guestDir  string
	guestOnce = map[string]string{}
)

func TestMain(m *testing.M) {
	code := m.Run()
	if guestDir != "" {
		os.RemoveAll(guestDir)
	}
	os.Exit(code)
}

// guest builds testdata/<name> with GOOS=wasip1 GOARCH=wasm (cached for the
// whole test run in a temp dir) and skips the test when that is impossible.
func guest(t *testing.T, name string, ldflags ...string) string {
	t.Helper()
	guestMu.Lock()
	defer guestMu.Unlock()
	key := name + "|" + strings.Join(ldflags, " ")
	if p, ok := guestOnce[key]; ok {
		if p == "" {
			t.Skip("wasip1 toolchain unavailable")
		}
		return p
	}
	if guestDir == "" {
		d, err := os.MkdirTemp("", "toki-wasm-guests-")
		if err != nil {
			t.Skip(err)
		}
		guestDir = d
	}
	_, file, _, _ := runtime.Caller(0)
	pkgDir := filepath.Dir(file)
	out := filepath.Join(guestDir, strings.NewReplacer("|", "_", " ", "_", "=", "_", "-", "_").Replace(key)+".wasm")
	args := []string{"build", "-o", out}
	if len(ldflags) > 0 {
		args = append(args, "-ldflags", strings.Join(ldflags, " "))
	}
	args = append(args, "./testdata/"+name)
	cmd := exec.Command("go", args...)
	cmd.Dir = pkgDir
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm", "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		guestOnce[key] = ""
		t.Skipf("cannot build guest %s: %v\n%s", name, err, b)
	}
	guestOnce[key] = out
	return out
}

// ---- harness ----

type modSpec struct {
	name string
	wasm string
	toml string
}

type env struct {
	t    *testing.T
	app  *tests.TestApp
	h    *Host
	dir  string
	srv  *httptest.Server
	user string // user auth token
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func newEnv(t *testing.T, mods ...modSpec) *env {
	t.Helper()
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)
	dir := t.TempDir()
	for _, m := range mods {
		copyFile(t, m.wasm, filepath.Join(dir, m.name+".wasm"))
		if m.toml != "" {
			if err := os.WriteFile(filepath.Join(dir, m.name+".toml"), []byte(m.toml), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	e := &env{t: t, app: app, dir: dir}

	posts := core.NewBaseCollection("posts")
	posts.Fields.Add(&core.TextField{Name: "title"})
	empty := ""
	posts.CreateRule, posts.ListRule, posts.ViewRule, posts.UpdateRule, posts.DeleteRule = &empty, &empty, &empty, &empty, &empty
	if err := app.Save(posts); err != nil {
		t.Fatal(err)
	}

	e.h = RegisterWithConfig(app, Config{Dir: dir})
	t.Cleanup(e.h.Close)
	if len(mods) > 0 && len(e.h.Modules()) != len(mods) {
		t.Fatalf("loaded %d of %d modules: %v", len(e.h.Modules()), len(mods), e.h.LoadErrors())
	}

	router, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	se := &core.ServeEvent{App: app, Router: router}
	if err := app.OnServe().Trigger(se, func(se *core.ServeEvent) error { return nil }); err != nil {
		t.Fatal(err)
	}
	mux, err := router.BuildMux()
	if err != nil {
		t.Fatal(err)
	}
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)

	if u, err := app.FindAuthRecordByEmail("users", "test@example.com"); err == nil {
		e.user, _ = u.NewAuthToken()
	}
	return e
}

func (e *env) do(method, path, body, token string) (int, string) {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (e *env) calls(name string) (calls, errs int64) {
	m := e.h.Module(name)
	return m.Stats.calls.Load(), m.Stats.errors.Load()
}

const (
	hookTOML = `events = ["record.create.posts"]` + "\n"
	miscTOML = `events = ["route:GET /api/misc", "cron:*/5 * * * *"]
timeout_ms = 400
needs = ["http", "kv", "records"]
`
)

// ---- tests ----

func TestParseManifest(t *testing.T) {
	m, err := ParseManifest("x", "x.wasm", `
# comment
events = [
  "record.create.posts",   # trailing
  "record.update.*",
  "cron:*/5 * * * *",
  "route:POST /api/hello",
]
timeout_ms = 2_000
memory_pages = 64
needs = ["http", "records", "mail"]
env = { A = "1", B = 'two' }
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Events) != 4 || m.TimeoutMS != 2000 || m.MemoryPages != 64 || m.Env["B"] != "two" || !m.Has("mail") {
		t.Fatalf("unexpected manifest: %+v", m)
	}
	for _, bad := range []string{
		`events = ["bogus"]`, `events = ["cron:not a cron"]`, `events = ["route:post /x"]`,
		`needs = ["root"]`, `timeout_ms = 0`, `memory_pages = 99999`, `nope = 1`, `events = "x"`,
	} {
		if _, err := ParseManifest("x", "x.wasm", bad); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
	d, err := ParseManifest("y", "y.wasm", "")
	if err != nil || d.TimeoutMS != DefaultTimeoutMS || d.MemoryPages != DefaultMemoryPages {
		t.Fatalf("defaults: %+v %v", d, err)
	}
}

func TestBeforeCreateHook(t *testing.T) {
	e := newEnv(t, modSpec{"upper", guest(t, "hookcreate"), hookTOML})

	// rejected through the API with the guest's status, message and data
	code, body := e.do("POST", "/api/collections/posts/records", `{"title":"  "}`, "")
	if code != 400 || !strings.Contains(body, "itle is required") || !strings.Contains(body, "validation_required") {
		t.Fatalf("want 400 rejection, got %d %s", code, body)
	}
	// accepted and rewritten
	code, body = e.do("POST", "/api/collections/posts/records", `{"title":"hello"}`, "")
	if code != 200 || !strings.Contains(body, `"title":"HELLO"`) {
		t.Fatalf("want uppercased title, got %d %s", code, body)
	}
	// direct app-level save (no HTTP request) is covered too, actor = system
	rec := core.NewRecord(mustCol(t, e.app, "posts"))
	rec.Set("title", "")
	err := e.app.Save(rec)
	var apiErr *router.ApiError
	if !errors.As(err, &apiErr) || apiErr.Status != 400 {
		t.Fatalf("want ApiError 400 from app.Save, got %v", err)
	}
	rec.Set("title", "direct")
	if err := e.app.Save(rec); err != nil || rec.GetString("title") != "DIRECT" {
		t.Fatalf("app.Save: %v title=%q", err, rec.GetString("title"))
	}
	if c, errs := e.calls("upper"); c != 4 || errs != 0 {
		t.Fatalf("stats calls=%d errors=%d", c, errs)
	}
}

func mustCol(t *testing.T, app core.App, name string) *core.Collection {
	t.Helper()
	c, err := app.FindCollectionByNameOrId(name)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRouteEchoesAuthID(t *testing.T) {
	e := newEnv(t, modSpec{"hello", guest(t, "route"), `events = ["route:POST /api/hello"]`})
	if e.user == "" {
		t.Skip("no users fixture")
	}
	u, _ := e.app.FindAuthRecordByEmail("users", "test@example.com")

	code, body := e.do("POST", "/api/hello?x=1", `{"a":1}`, e.user)
	var out map[string]any
	_ = json.Unmarshal([]byte(body), &out)
	if code != 201 || out["actor"] != u.Id || out["kind"] != "auth" || out["body"] != `{"a":1}` || out["query_x"] != "1" {
		t.Fatalf("got %d %s (user %s)", code, body, u.Id)
	}
	code, body = e.do("POST", "/api/hello", `{}`, "")
	if code != 201 || !strings.Contains(body, `"kind":"guest"`) {
		t.Fatalf("guest: %d %s", code, body)
	}
}

func TestTimeoutDoesNotCrashServer(t *testing.T) {
	e := newEnv(t, modSpec{"misc", guest(t, "misc"), miscTOML})
	start := time.Now()
	code, body := e.do("GET", "/api/misc?cmd=loop", "", "")
	if code != 500 || strings.Contains(body, "timeout") { // generic message only
		t.Fatalf("want generic 500, got %d %s", code, body)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("timeout not enforced, took %s", d)
	}
	// the server and the module still work
	code, _ = e.do("GET", "/api/misc?cmd=nop", "", "")
	if code != 200 {
		t.Fatalf("module unusable after timeout: %d", code)
	}
	if c, errs := e.calls("misc"); c != 2 || errs != 1 {
		t.Fatalf("stats calls=%d errors=%d", c, errs)
	}
}

func TestTrapAndExitAndStdoutCap(t *testing.T) {
	e := newEnv(t, modSpec{"misc", guest(t, "misc"), strings.Replace(miscTOML, "400", "5000", 1)})
	for _, cmd := range []string{"panic", "exit", "stdout"} {
		code, body := e.do("GET", "/api/misc?cmd="+cmd, "", "")
		if code != 500 || !strings.Contains(body, "Hook failed.") {
			t.Errorf("%s: want 500 Hook failed., got %d %s", cmd, code, body)
		}
	}
	if code, _ := e.do("GET", "/api/misc?cmd=nop", "", ""); code != 200 {
		t.Fatalf("module unusable after traps: %d", code)
	}
}

func TestTrapInBeforeHookRejectsWrite(t *testing.T) {
	// the route guest dereferences ev.Route, which is nil for record events: it panics
	e := newEnv(t, modSpec{"bad", guest(t, "route"), hookTOML})
	code, body := e.do("POST", "/api/collections/posts/records", `{"title":"x"}`, "")
	if code != 500 || !strings.Contains(body, "Hook failed.") || strings.Contains(body, "nil pointer") {
		t.Fatalf("want generic 500, got %d %s", code, body)
	}
	var n int
	e.app.DB().NewQuery("SELECT COUNT(*) FROM posts").Row(&n)
	if n != 0 {
		t.Fatalf("record was written despite the failed hook")
	}
}

func TestMemoryLimit(t *testing.T) {
	spec := strings.Replace(miscTOML, "400", "5000", 1) + "memory_pages = 256\n"
	e := newEnv(t, modSpec{"misc", guest(t, "misc"), spec})
	code, _ := e.do("GET", "/api/misc?cmd=alloc", "", "")
	if code != 500 {
		t.Fatalf("512 MB allocation must fail under a 16 MB limit, got %d", code)
	}
	if code, _ := e.do("GET", "/api/misc?cmd=nop", "", ""); code != 200 {
		t.Fatalf("module unusable after OOM: %d", code)
	}
}

func TestHTTPAllowlist(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "pong") }))
	defer up.Close()
	host := strings.TrimPrefix(up.URL, "http://")
	hostname := strings.Split(host, ":")[0] // 127.0.0.1

	spec := strings.Replace(miscTOML, "400", "5000", 1)
	e := newEnv(t, modSpec{"misc", guest(t, "misc"), spec},
		modSpec{"nohttp", guest(t, "misc"), `events = ["route:GET /api/nohttp"]`})

	fetch := func(path string) string {
		_, body := e.do("GET", path+"?cmd=http&u="+up.URL, "", "")
		return body
	}
	// 1. empty allowlist denies everything
	if b := fetch("/api/misc"); !strings.Contains(b, "TOKI_WASM_HTTP_ALLOW") {
		t.Fatalf("expected allowlist denial, got %s", b)
	}
	// 2. allowlisted but loopback: SSRF guard
	t.Setenv("TOKI_WASM_HTTP_ALLOW", hostname)
	if b := fetch("/api/misc"); !strings.Contains(b, "private, loopback") {
		t.Fatalf("expected SSRF denial, got %s", b)
	}
	// 3. private allowed explicitly
	t.Setenv("TOKI_WASM_ALLOW_PRIVATE", "1")
	if b := fetch("/api/misc"); !strings.Contains(b, `"status":200`) || !strings.Contains(b, "pong") {
		t.Fatalf("expected success, got %s", b)
	}
	// 4. a different host is not allowlisted
	_, b := e.do("GET", "/api/misc?cmd=http&u=http://localhost:"+strings.Split(host, ":")[1], "", "")
	if !strings.Contains(b, "not in TOKI_WASM_HTTP_ALLOW") {
		t.Fatalf("expected host denial, got %s", b)
	}
	// 5. module without needs=http is refused even for an allowlisted host
	if b := fetch("/api/nohttp"); !strings.Contains(b, "capability") {
		t.Fatalf("expected capability denial, got %s", b)
	}
	t.Setenv("TOKI_WASM_HTTP_ALLOW", "*.example.com,exact.org")
	for host, want := range map[string]bool{"a.example.com": true, "example.com": false, "exact.org": true, "x.exact.org": false, "evilexample.com": false} {
		if hostAllowed(host) != want {
			t.Errorf("hostAllowed(%q) = %v, want %v", host, !want, want)
		}
	}
}

func TestKVRoundTripAndIsolation(t *testing.T) {
	spec := strings.Replace(miscTOML, "400", "5000", 1)
	e := newEnv(t, modSpec{"misc", guest(t, "misc"), spec},
		modSpec{"other", guest(t, "misc"), `events = ["route:GET /api/other"]
needs = ["kv"]`})
	_, body := e.do("GET", "/api/misc?cmd=kv&v=hello", "", "")
	if !strings.Contains(body, `"value":"hello"`) || !strings.Contains(body, `"found":true`) {
		t.Fatalf("kv round trip: %s", body)
	}
	// other module has its own namespace
	_, body = e.do("GET", "/api/other?cmd=kv&v=theirs", "", "")
	if !strings.Contains(body, `"value":"theirs"`) {
		t.Fatalf("other: %s", body)
	}
	var v string
	if err := e.app.AuxDB().NewQuery(`SELECT value FROM _wasm_kv WHERE module='misc' AND key='counter'`).Row(&v); err != nil || v != "hello" {
		t.Fatalf("stored value %q err %v", v, err)
	}
}

func TestRecordsHostCallAndLoopGuard(t *testing.T) {
	spec := strings.Replace(miscTOML, "400", "5000", 1)
	e := newEnv(t,
		modSpec{"misc", guest(t, "misc"), spec},
		modSpec{"upper", guest(t, "hookcreate"), hookTOML})
	_, body := e.do("GET", "/api/misc?cmd=records&t=lower", "", "")
	// the guest's own write must not run the uppercase hook (loop guard)
	if !strings.Contains(body, `"saved":"lower"`) || !strings.Contains(body, `"found":1`) {
		t.Fatalf("records host calls: %s", body)
	}
	if c, _ := e.calls("upper"); c != 0 {
		t.Fatalf("guest write re-triggered wasm hooks (%d calls)", c)
	}
}

func TestHotReload(t *testing.T) {
	e := newEnv(t, modSpec{"upper", guest(t, "hookcreate"), hookTOML})
	create := func() string {
		_, body := e.do("POST", "/api/collections/posts/records", `{"title":"abc"}`, "")
		var r struct{ Title string }
		_ = json.Unmarshal([]byte(body), &r)
		return r.Title
	}
	if got := create(); got != "ABC" {
		t.Fatalf("v1: %q", got)
	}
	v2 := guest(t, "hookcreate", "-X main.suffix=-v2")
	copyFile(t, v2, filepath.Join(e.dir, "upper.wasm"))
	e.h.Reload()
	if got := create(); got != "ABC-v2" {
		t.Fatalf("after Reload: %q", got)
	}

	// file watcher path
	if err := e.h.startWatcher(); err != nil {
		t.Fatal(err)
	}
	copyFile(t, guest(t, "hookcreate"), filepath.Join(e.dir, "upper.wasm"))
	deadline := time.Now().Add(15 * time.Second)
	for create() != "ABC" {
		if time.Now().After(deadline) {
			t.Fatal("watcher did not reload the module")
		}
		time.Sleep(200 * time.Millisecond)
	}
	// removing the file unloads the module
	os.Remove(filepath.Join(e.dir, "upper.wasm"))
	e.h.Reload()
	if len(e.h.Modules()) != 0 {
		t.Fatal("module should be unloaded")
	}
	if got := create(); got != "abc" {
		t.Fatalf("no hook expected, got %q", got)
	}
}

func TestBrokenModuleIsSkipped(t *testing.T) {
	e := newEnv(t, modSpec{"good", guest(t, "hookcreate"), hookTOML})
	os.WriteFile(filepath.Join(e.dir, "junk.wasm"), []byte("not wasm"), 0o644)
	os.WriteFile(filepath.Join(e.dir, "badtoml.wasm"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(e.dir, "badtoml.toml"), []byte(`events = ["nope"]`), 0o644)
	e.h.Reload()
	if len(e.h.Modules()) != 1 || len(e.h.LoadErrors()) != 2 {
		t.Fatalf("modules=%d errors=%v", len(e.h.Modules()), e.h.LoadErrors())
	}
}

func TestCronViaFallbackAndStats(t *testing.T) {
	spec := strings.Replace(miscTOML, "400", "5000", 1)
	e := newEnv(t, modSpec{"misc", guest(t, "misc"), spec})
	found := false
	for _, j := range e.app.Cron().Jobs() {
		if strings.HasPrefix(j.Id(), "__tokiWasm__misc__") {
			found = true
		}
	}
	if !found {
		t.Fatal("cron job not scheduled")
	}
	e.h.cronTick("misc", "*/5 * * * *") // no job queue in this app: runs inline on its own goroutine
	deadline := time.Now().Add(5 * time.Second)
	for c, _ := e.calls("misc"); c < 1 && time.Now().Before(deadline); c, _ = e.calls("misc") {
		time.Sleep(20 * time.Millisecond)
	}
	if c, errs := e.calls("misc"); c != 1 || errs != 0 {
		t.Fatalf("calls=%d errors=%d", c, errs)
	}
	e.h.FlushStats()
	snaps, err := e.h.StoredStats()
	if err != nil || len(snaps) != 1 || snaps[0].Calls != 1 {
		t.Fatalf("stored stats %+v %v", snaps, err)
	}
}

func execCmd(t *testing.T, root *cobra.Command, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func TestCLI(t *testing.T) {
	e := newEnv(t, modSpec{"upper", guest(t, "hookcreate"), hookTOML + "timeout_ms = 3000\nmemory_pages = 512\n"})
	payload := filepath.Join(t.TempDir(), "p.json")
	os.WriteFile(payload, []byte(`{"collection":"posts","action":"create","phase":"before","record":{"title":"cli"}}`), 0o644)

	out, err := execCmd(t, NewCommand(e.app), "list")
	if err != nil || !strings.Contains(out, "upper") || !strings.Contains(out, "3000ms") || !strings.Contains(out, "record.create.posts") {
		t.Fatalf("list: %v\n%s", err, out)
	}
	out, err = execCmd(t, NewCommand(e.app), "run", "upper", "--event", "record.create.posts", "--payload", payload)
	if err != nil || !strings.Contains(out, `"title": "CLI"`) {
		t.Fatalf("run: %v\n%s", err, out)
	}
	// nothing was written
	var n int
	e.app.DB().NewQuery("SELECT COUNT(*) FROM posts").Row(&n)
	if n != 0 {
		t.Fatalf("run wrote %d records", n)
	}
	out, err = execCmd(t, NewCommand(e.app), "stats")
	if err != nil || !strings.Contains(out, "upper") {
		t.Fatalf("stats: %v\n%s", err, out)
	}
	out, err = execCmd(t, NewCommand(e.app), "validate", filepath.Join(e.dir, "upper.wasm"))
	if err != nil || !strings.Contains(out, "OK (toki/1)") {
		t.Fatalf("validate: %v\n%s", err, out)
	}
	junk := filepath.Join(t.TempDir(), "junk.wasm")
	os.WriteFile(junk, []byte("nope"), 0o644)
	if _, err := execCmd(t, NewCommand(e.app), "validate", junk); err == nil {
		t.Fatal("validate must fail on junk")
	}
	// a plain Go hello world is a valid WASI command but does not export toki_alloc
	// only matters when it imports host calls; the sample guest does and exports it.
	out, _ = execCmd(t, NewCommand(e.app), "validate", filepath.Join(e.dir, "upper.wasm"))
	if !strings.Contains(out, "export toki_alloc: true") {
		t.Fatalf("expected toki_alloc export:\n%s", out)
	}
}

func TestDryRunSuppressesEffects(t *testing.T) {
	spec := strings.Replace(miscTOML, "400", "5000", 1)
	e := newEnv(t, modSpec{"misc", guest(t, "misc"), spec})
	m := e.h.Module("misc")
	ev := &EventIn{Event: "route:GET /api/misc", Kind: "route", Route: &RouteIn{Method: "GET", Query: map[string]string{"cmd": "records", "t": "dry"}}}
	var effects []map[string]any
	if _, err := e.h.Invoke(t.Context(), m, ev, CallOpts{DryRun: true, Effects: &effects}); err != nil {
		t.Fatal(err)
	}
	if len(effects) != 1 || effects[0]["effect"] != "records_save" {
		t.Fatalf("effects: %v", effects)
	}
	var n int
	e.app.DB().NewQuery("SELECT COUNT(*) FROM posts").Row(&n)
	if n != 0 {
		t.Fatal("dry run wrote a record")
	}
}

func TestInvokeLatency(t *testing.T) {
	e := newEnv(t, modSpec{"upper", guest(t, "hookcreate"), hookTOML})
	m := e.h.Module("upper")
	ev := func() *EventIn {
		return &EventIn{Event: "record.create.posts", Kind: "record", Record: map[string]any{"title": "x"}}
	}
	start := time.Now()
	const n = 50
	for i := 0; i < n; i++ {
		if _, err := e.h.Invoke(t.Context(), m, ev(), CallOpts{}); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("avg per call: %s", time.Since(start)/n)
}
