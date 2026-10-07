//go:build !no_wasm

package wasm

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/router"
)

func probeTOML(mode, events string) string {
	return "events = [" + events + "]\nneeds = [\"records\"]\ntimeout_ms = 5000\n[env]\nMODE = \"" + mode + "\"\n"
}

func addNoteCollection(t *testing.T, e *env, names ...string) {
	t.Helper()
	empty := ""
	for _, n := range names {
		c := core.NewBaseCollection(n)
		c.Fields.Add(&core.TextField{Name: "note"})
		c.CreateRule, c.ListRule, c.ViewRule = &empty, &empty, &empty
		if err := e.app.Save(c); err != nil {
			t.Fatal(err)
		}
	}
}

func within(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not finish within %s (deadlock)", what, d)
	}
}

func countRows(t *testing.T, e *env, table string) int {
	t.Helper()
	var n int
	if err := e.app.DB().NewQuery("SELECT COUNT(*) FROM " + table).Row(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// W1: host record functions use the app of the event (transaction aware).
func TestW1HostRecordsInsideTransactionAndBatch(t *testing.T) {
	e := newEnv(t, modSpec{"probe", guest(t, "probe"), probeTOML("save", `"record.create.posts"`)})
	addNoteCollection(t, e, "audit")

	within(t, 10*time.Second, "save inside RunInTransaction", func() {
		err := e.app.RunInTransaction(func(tx kernel.App) error {
			rec := core.NewRecord(mustCol(t, core.AsApp(tx), "posts"))
			rec.Set("title", "in-tx")
			return tx.Save(rec)
		})
		if err != nil {
			t.Errorf("tx: %v", err)
		}
	})
	if n := countRows(t, e, "audit"); n != 1 {
		t.Fatalf("audit rows after tx: %d", n)
	}

	s := e.app.Settings()
	s.Batch.Enabled = true
	if err := e.app.Save(s); err != nil {
		t.Fatal(err)
	}
	within(t, 10*time.Second, "/api/batch", func() {
		code, body := e.do("POST", "/api/batch", `{"requests":[{"method":"POST","url":"/api/collections/posts/records","body":{"title":"in-batch"}}]}`, "")
		if code != 200 {
			t.Errorf("batch: %d %s", code, body)
		}
	})
	if n := countRows(t, e, "audit"); n != 2 {
		t.Fatalf("audit rows after batch: %d", n)
	}
}

// W2: credentials never reach guests.
func TestW2RequestInfoIsScrubbed(t *testing.T) {
	e := newEnv(t, modSpec{"probe", guest(t, "probe"), probeTOML("echo", `"record.create.posts"`)})
	req, _ := http.NewRequest("POST", e.srv.URL+"/api/collections/posts/records",
		strings.NewReader(`{"title":"t","password":"hunter2pw","passwordConfirm":"hunter2pw","oldPassword":"o1d","token":"tok-123","secret":"sec-456","nested":{"Password":"nested-pw"},"keep":"visible"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer eyJ-secret-jwt")
	req.Header.Set("Cookie", "sid=cookie-secret")
	req.Header.Set("X-Toki-Key", "toki-secret")
	req.Header.Set("X-Other", "keep-me")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var rec struct{ Title string }
	_ = json.NewDecoder(resp.Body).Decode(&rec)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d (%s)", resp.StatusCode, rec.Title)
	}
	got := rec.Title
	for _, leak := range []string{"eyJ-secret-jwt", "cookie-secret", "toki-secret", "hunter2pw", "o1d", "tok-123", "sec-456", "nested-pw"} {
		if strings.Contains(got, leak) {
			t.Errorf("request_info leaks %q: %s", leak, got)
		}
	}
	if !strings.Contains(got, "keep-me") || !strings.Contains(got, "visible") {
		t.Errorf("harmless values were removed: %s", got)
	}
}

func TestW2SanitizeDoesNotMutateOriginal(t *testing.T) {
	info := &core.RequestInfo{Headers: map[string]string{"authorization": "x", "x_toki_a": "y", "ok": "1"},
		Body: map[string]any{"password": "p", "a": map[string]any{"token": "t", "b": 1}}}
	out := sanitizeRequestInfo(info)
	if _, ok := out.Headers["authorization"]; ok || out.Headers["ok"] != "1" || len(out.Headers) != 1 {
		t.Fatalf("headers %v", out.Headers)
	}
	if _, ok := out.Body["password"]; ok {
		t.Fatal("password kept")
	}
	if _, ok := out.Body["a"].(map[string]any)["token"]; ok {
		t.Fatal("nested token kept")
	}
	if info.Body["password"] != "p" || info.Body["a"].(map[string]any)["token"] != "t" {
		t.Fatal("original request info was modified")
	}
}

// W3: host functions stop at the call deadline.
func TestW3HostFunctionsRespectContext(t *testing.T) {
	e := newEnv(t, modSpec{"misc", guest(t, "misc"), strings.Replace(miscTOML, "400", "5000", 1)})
	c := &call{h: e.h, mod: e.h.Module("misc")}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for name, fn := range map[string]func() (map[string]any, error){
		"find": func() (map[string]any, error) {
			return e.h.recordsFind(ctx, c, []byte(`{"collection":"posts","filter":"title != ''"}`))
		},
		"find-id": func() (map[string]any, error) {
			return e.h.recordsFind(ctx, c, []byte(`{"collection":"posts","id":"abc"}`))
		},
		"save": func() (map[string]any, error) {
			return e.h.recordsSave(ctx, c, []byte(`{"collection":"posts","data":{"title":"x"}}`))
		},
		"kv_get": func() (map[string]any, error) { return e.h.kvGet(ctx, c, []byte(`{"key":"k"}`)) },
		"kv_set": func() (map[string]any, error) { return e.h.kvSet(ctx, c, []byte(`{"key":"k","value":"v"}`)) },
	} {
		if _, err := fn(); err == nil {
			t.Errorf("%s: canceled context must fail the host call", name)
		}
	}
	if n := countRows(t, e, "posts"); n != 0 {
		t.Fatalf("a canceled save wrote %d rows", n)
	}
}

// W4: fail closed on reload.
func TestW4ReloadKeepsPreviousModuleOnBrokenFile(t *testing.T) {
	e := newEnv(t, modSpec{"upper", guest(t, "hookcreate"), hookTOML})
	create := func() string {
		_, body := e.do("POST", "/api/collections/posts/records", `{"title":"abc"}`, "")
		var r struct{ Title string }
		_ = json.Unmarshal([]byte(body), &r)
		return r.Title
	}
	if create() != "ABC" {
		t.Fatal("setup")
	}
	good, _ := os.ReadFile(filepath.Join(e.dir, "upper.wasm"))

	// half copied file
	if err := os.WriteFile(filepath.Join(e.dir, "upper.wasm"), good[:len(good)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	e.h.Reload()
	if e.h.Module("upper") == nil || create() != "ABC" {
		t.Fatal("broken wasm removed the previous hook (fail open)")
	}
	if e.h.LoadErrors()["upper"] == nil {
		t.Fatal("load error not reported")
	}

	// TOML typo
	_ = os.WriteFile(filepath.Join(e.dir, "upper.wasm"), good, 0o644)
	_ = os.WriteFile(filepath.Join(e.dir, "upper.toml"), []byte(`events = ["nope"]`), 0o644)
	e.h.Reload()
	if e.h.Module("upper") == nil || create() != "ABC" {
		t.Fatal("broken sidecar removed the previous hook")
	}

	// hooks dir temporarily unreadable
	moved := e.dir + ".moved"
	if err := os.Rename(e.dir, moved); err != nil {
		t.Fatal(err)
	}
	e.h.Reload()
	_ = os.Rename(moved, e.dir)
	if e.h.Module("upper") == nil {
		t.Fatal("missing hooks dir dropped the modules")
	}

	// fixed again: new version loads and the error clears
	_ = os.WriteFile(filepath.Join(e.dir, "upper.toml"), []byte(hookTOML), 0o644)
	e.h.Reload()
	if e.h.LoadErrors()["upper"] != nil || create() != "ABC" {
		t.Fatalf("recovery failed: %v", e.h.LoadErrors())
	}
}

func TestW4WaitStable(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "m.wasm")
	_ = os.WriteFile(f, []byte("a"), 0o644)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // a copy in progress
		defer wg.Done()
		for i := 0; i < 8; i++ {
			select {
			case <-stop:
				return
			case <-time.After(40 * time.Millisecond):
			}
			fh, _ := os.OpenFile(f, os.O_APPEND|os.O_WRONLY, 0o644)
			fh.WriteString("more")
			fh.Close()
		}
	}()
	if waitStable(dir, 60*time.Millisecond, 3) {
		t.Fatal("a growing file must not count as stable")
	}
	close(stop)
	wg.Wait()
	if !waitStable(dir, 20*time.Millisecond, 5) {
		t.Fatal("an idle directory must be stable")
	}
}

// W5: needs=records is not superuser.
func TestW5RecordsCannotTouchSystemCollectionsOrReservedFields(t *testing.T) {
	e := newEnv(t, modSpec{"probe", guest(t, "probe"), probeTOML("sys", `"record.create.posts"`)})
	addNoteCollection(t, e, "audit")
	code, body := e.do("POST", "/api/collections/posts/records", `{"title":"x"}`, "")
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	var r struct{ Title string }
	_ = json.Unmarshal([]byte(body), &r)
	var out map[string]string
	if err := json.Unmarshal([]byte(r.Title), &out); err != nil {
		t.Fatalf("guest report %q: %v", r.Title, err)
	}
	for _, k := range []string{"find", "save", "reserved"} {
		if out[k] == "" {
			t.Errorf("%s must be denied, report %v", k, out)
		}
	}
	if !strings.Contains(out["find"], "system collections") || !strings.Contains(out["save"], "system collections") {
		t.Errorf("unexpected reasons %v", out)
	}
	var n int
	_ = e.app.DB().NewQuery("SELECT COUNT(*) FROM _superusers WHERE email='evil@example.com'").Row(&n)
	if n != 0 {
		t.Fatal("a guest created a superuser")
	}
}

// W5: the request hook chain of other modules applies to guest writes.
func TestW5GuestWritesRunRequestHooks(t *testing.T) {
	e := newEnv(t, modSpec{"probe", guest(t, "probe"), probeTOML("save", `"record.create.posts"`)})
	addNoteCollection(t, e, "audit")
	var seen atomic.Int32
	e.app.OnRecordCreateRequest("audit").BindFunc(func(ev *core.RecordRequestEvent) error {
		seen.Add(1)
		if ev.Auth != nil {
			t.Error("guest writes must run as a guest")
		}
		return ev.Next()
	})
	if code, body := e.do("POST", "/api/collections/posts/records", `{"title":"x"}`, ""); code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	if seen.Load() != 1 {
		t.Fatalf("request hook ran %d times", seen.Load())
	}
	// a guard rejecting the write fails the guest call
	e.app.OnRecordCreateRequest("audit").BindFunc(func(ev *core.RecordRequestEvent) error {
		return router.NewForbiddenError("nope", nil)
	})
	code, body := e.do("POST", "/api/collections/posts/records", `{"title":"y"}`, "")
	if code == 200 {
		t.Fatalf("guard bypassed: %s", body)
	}
	if n := countRows(t, e, "audit"); n != 1 {
		t.Fatalf("audit rows %d, want 1", n)
	}
}

// W5: call depth travels in the context of guest writes and is capped.
func TestW5CallDepthIsCarriedAndCapped(t *testing.T) {
	e := newEnv(t, modSpec{"probe", guest(t, "probe"), probeTOML("save", `"record.create.posts"`)})
	addNoteCollection(t, e, "audit")
	depth := int32(-1)
	e.app.OnRecordCreate("audit").BindFunc(func(ev *core.RecordEvent) error {
		if c, ok := ev.Context.Value(callKey{}).(*call); ok {
			atomic.StoreInt32(&depth, int32(c.depth))
		}
		return ev.Next()
	})
	if code, body := e.do("POST", "/api/collections/posts/records", `{"title":"x"}`, ""); code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	if atomic.LoadInt32(&depth) != 0 {
		t.Fatalf("hook triggered by a guest write did not see the call in its context (depth=%d)", depth)
	}

	m := e.h.Module("probe")
	parent := func(d int) context.Context {
		return context.WithValue(context.Background(), callKey{}, &call{depth: d})
	}
	ev := func() *EventIn {
		return &EventIn{Event: "record.create.posts", Kind: "record", Record: map[string]any{"title": "x"}}
	}
	if _, err := e.h.Invoke(parent(MaxCallDepth-1), m, ev(), CallOpts{}); err != nil {
		t.Fatalf("depth %d must be allowed: %v", MaxCallDepth, err)
	}
	_, err := e.h.Invoke(parent(MaxCallDepth), m, ev(), CallOpts{})
	if ce, ok := err.(*CallError); !ok || !strings.Contains(ce.Detail, "depth") {
		t.Fatalf("depth %d must be refused, got %v", MaxCallDepth+1, err)
	}
}

// W5: ping/pong between two collections terminates.
func TestW5PingPongTerminates(t *testing.T) {
	e := newEnv(t, modSpec{"probe", guest(t, "probe"), probeTOML("pingpong", `"record.create.ping", "record.create.pong"`)})
	addNoteCollection(t, e, "ping", "pong")
	within(t, 10*time.Second, "ping/pong", func() {
		if code, body := e.do("POST", "/api/collections/ping/records", `{"note":"a"}`, ""); code != 200 {
			t.Errorf("%d %s", code, body)
		}
	})
	if a, b := countRows(t, e, "ping"), countRows(t, e, "pong"); a+b > 4 {
		t.Fatalf("hook loop: ping=%d pong=%d", a, b)
	}
}

type fakeQueue struct {
	mu   sync.Mutex
	opts []kernel.EnqueueOptions
}

func (f *fakeQueue) Enqueue(_ context.Context, _ string, _ any, o ...kernel.EnqueueOption) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opts = append(f.opts, kernel.ResolveEnqueueOptions(o...))
	return "id", nil
}
func (f *fakeQueue) Register(string, kernel.JobHandler)             {}
func (f *fakeQueue) Stats(context.Context) (kernel.JobStats, error) { return kernel.JobStats{}, nil }

// W6: one cron slot key (kept after completion), no retries.
func TestW6CronUsesSlotKeyAndSingleAttempt(t *testing.T) {
	e := newEnv(t, modSpec{"misc", guest(t, "misc"), strings.Replace(miscTOML, "400", "5000", 1)})
	q := &fakeQueue{}
	kernel.SetJobs(e.app, q)
	e.h.cronTick("misc", "*/5 * * * *")
	if len(q.opts) != 1 {
		t.Fatalf("enqueued %d", len(q.opts))
	}
	o := q.opts[0]
	if !strings.HasPrefix(o.CronKey, "wasm.cron:misc:*/5 * * * *:") || o.MaxAttempts != 1 || o.UniqueKey != "" {
		t.Fatalf("options %+v", o)
	}
	if got := q.opts[0].CronKey; len(got) < 12 {
		t.Fatalf("slot missing in %q", got)
	}
}

// W7: cache dir hardening.
func TestW7CacheDir(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))

	fresh := filepath.Join(t.TempDir(), "cache")
	c := openCompilationCache(fresh, log)
	c.Close(context.Background())
	fi, err := os.Stat(fresh)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("cache dir not created 0700: %v %v", fi, err)
	}
	if strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("unexpected warning: %s", buf.String())
	}

	open := filepath.Join(t.TempDir(), "open")
	_ = os.Mkdir(open, 0o777)
	_ = os.Chmod(open, 0o777)
	buf.Reset()
	c = openCompilationCache(open, log)
	c.Close(context.Background())
	if !strings.Contains(buf.String(), "level=WARN") || !strings.Contains(buf.String(), "in-memory") {
		t.Fatalf("world writable dir must fall back with a WARN: %q", buf.String())
	}

	link := filepath.Join(t.TempDir(), "link")
	_ = os.Symlink(fresh, link)
	buf.Reset()
	c = openCompilationCache(link, log)
	c.Close(context.Background())
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("symlink must be refused: %q", buf.String())
	}
}

// W8-W10: body and header caps of routes.
func TestRouteBodyAndHeaderCaps(t *testing.T) {
	e := newEnv(t, modSpec{"misc", guest(t, "misc"), strings.Replace(miscTOML, "400", "5000", 1)})
	if code, _ := e.do("GET", "/api/misc?cmd=nop", strings.Repeat("a", MaxRouteBodyBytes+10), ""); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("big body: %d", code)
	}
	req, _ := http.NewRequest("GET", e.srv.URL+"/api/misc?cmd=nop", nil)
	req.Header.Set("X-Big", strings.Repeat("h", MaxRouteHeaderBytes+10))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
		t.Fatalf("big headers: %d", resp.StatusCode)
	}
	if code, _ := e.do("GET", "/api/misc?cmd=nop", "", ""); code != 200 {
		t.Fatalf("normal request: %d", code)
	}
}
