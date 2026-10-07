package embed_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tokibase/tokibase/embed"
	"github.com/tokibase/tokibase/kernel"
)

func start(t *testing.T, dir string, mut func(*embed.Options)) *embed.Instance {
	t.Helper()
	o := embed.Options{DataDir: dir, LogLevel: "error"}
	if mut != nil {
		mut(&o)
	}
	inst, err := embed.Start(o)
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

func stop(t *testing.T, inst *embed.Instance) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := inst.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

func call(t *testing.T, inst *embed.Instance, method, path, token string, body any) (int, map[string]any) {
	t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	h := map[string]string{}
	if token != "" {
		h["Authorization"] = token
	}
	status, _, rb, err := inst.Call(method, path, h, raw)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]any{}
	_ = json.Unmarshal(rb, &out)
	return status, out
}

func login(t *testing.T, inst *embed.Instance) string {
	t.Helper()
	if err := inst.Superuser("admin@example.com", "supersecret123"); err != nil {
		t.Fatal(err)
	}
	st, out := call(t, inst, "POST", "/api/collections/_superusers/auth-with-password", "",
		map[string]string{"identity": "admin@example.com", "password": "supersecret123"})
	tok, _ := out["token"].(string)
	if st != 200 || tok == "" {
		t.Fatalf("auth: %d %v", st, out)
	}
	return tok
}

func TestLifecycle(t *testing.T) {
	dir := t.TempDir()
	inst := start(t, dir, nil)

	if !strings.HasPrefix(inst.URL(), "http://127.0.0.1:") {
		t.Fatalf("url: %q", inst.URL())
	}
	if st, _ := call(t, inst, "GET", "/api/health", "", nil); st != 200 {
		t.Fatalf("health: %d", st)
	}

	// a second instance on the same dir is refused
	if _, err := embed.Start(embed.Options{DataDir: dir}); err == nil {
		t.Fatal("expected error for a second instance on the same DataDir")
	}

	tok := login(t, inst)

	st, out := call(t, inst, "POST", "/api/collections", tok, map[string]any{
		"name": "records", "type": "base",
		"listRule": "", "viewRule": "", "createRule": "",
		"fields": []map[string]any{{"name": "title", "type": "text"}},
	})
	if st != 200 {
		t.Fatalf("create collection: %d %v", st, out)
	}

	events := make(chan []byte, 4)
	cancel, err := inst.Subscribe("records/*", func(b []byte) { events <- b })
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()

	st, out = call(t, inst, "POST", "/api/collections/records/records", tok, map[string]any{"title": "hello"})
	if st != 200 || out["title"] != "hello" {
		t.Fatalf("create record: %d %v", st, out)
	}
	select {
	case ev := <-events:
		var m map[string]any
		if err := json.Unmarshal(ev, &m); err != nil || m["action"] != "create" {
			t.Fatalf("event: %s", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no realtime event")
	}
	cancel()
	cancel() // idempotent

	// export produces a zip
	var buf bytes.Buffer
	if err := inst.Export(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(buf.Bytes(), []byte("PK")) {
		t.Fatal("export is not a zip")
	}

	stop(t, inst)
	if _, _, _, err := inst.Call("GET", "/api/health", nil, nil); err == nil {
		t.Fatal("Call after Stop must fail")
	}
	if _, err := inst.Subscribe("records/*", func([]byte) {}); err == nil {
		t.Fatal("Subscribe after Stop must fail")
	}
	if err := inst.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop: %v", err)
	}

	// restart on the same dir keeps data
	inst = start(t, dir, nil)
	defer stop(t, inst)
	tok = login(t, inst)
	st, out = call(t, inst, "GET", "/api/collections/records/records", tok, nil)
	if st != 200 || out["totalItems"] != float64(1) {
		t.Fatalf("data lost after restart: %d %v", st, out)
	}
}

func TestBodyLimit(t *testing.T) {
	inst := start(t, t.TempDir(), func(o *embed.Options) { o.MaxBodyBytes = 1 << 20 })
	defer stop(t, inst)
	tok := login(t, inst)

	body := bytes.Repeat([]byte("a"), 5<<20)
	st, _, _, err := inst.Call("POST", "/api/collections/_superusers/records", map[string]string{"Authorization": tok}, body)
	if err != nil || st != 413 {
		t.Fatalf("want 413, got %d err=%v", st, err)
	}
}

func TestNoListener(t *testing.T) {
	inst := start(t, t.TempDir(), func(o *embed.Options) { o.Listen = "-" })
	defer stop(t, inst)
	if inst.URL() != "" {
		t.Fatalf("url: %q", inst.URL())
	}
	if st, _ := call(t, inst, "GET", "/api/health", "", nil); st != 200 {
		t.Fatalf("health: %d", st)
	}
}

func TestBadOptions(t *testing.T) {
	if _, err := embed.Start(embed.Options{}); err == nil {
		t.Fatal("DataDir required")
	}
	if _, err := embed.Start(embed.Options{DataDir: t.TempDir(), Profile: "nope"}); err == nil {
		t.Fatal("unknown profile")
	}
	if _, err := embed.Start(embed.Options{DataDir: t.TempDir(), LogLevel: "loud"}); err == nil {
		t.Fatal("unknown log level")
	}
}

func TestRealtimeCallRefused(t *testing.T) {
	inst := start(t, t.TempDir(), func(o *embed.Options) { o.Listen = "-" })
	defer stop(t, inst)
	for _, m := range []string{"GET", "get", "Post", "HEAD"} {
		if _, _, _, err := inst.Call(m, "/api/realtime", nil, nil); err == nil {
			t.Fatalf("%s /api/realtime must be refused", m)
		}
	}
}

func TestCallContextTimeout(t *testing.T) {
	inst := start(t, t.TempDir(), func(o *embed.Options) { o.Listen = "-" })
	defer stop(t, inst)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, _, err := inst.CallContext(ctx, "GET", "/api/health", nil, nil); err == nil {
		t.Fatal("cancelled ctx must error")
	}
}

func TestStopWithExpiredCtxKeepsLock(t *testing.T) {
	dir := t.TempDir()
	inst := start(t, dir, func(o *embed.Options) { o.Listen = "-" })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := inst.Stop(ctx)
	if err == nil {
		// shutdown happened to finish instantly: the dir may be reused
		inst = start(t, dir, func(o *embed.Options) { o.Listen = "-" })
		stop(t, inst)
		return
	}
	if _, serr := embed.Start(embed.Options{DataDir: dir, Listen: "-"}); serr == nil {
		t.Fatal("data dir must stay locked while the stop is incomplete")
	}
	stop(t, inst) // a later Stop finishes the job
	again := start(t, dir, func(o *embed.Options) { o.Listen = "-" })
	stop(t, again)
}

// nanoKey returns a nano profile switch whose module is compiled in (the
// switches of modules compiled out by build tags are deliberately not set).
func nanoKey(t *testing.T) string {
	t.Helper()
	stubbed := map[string]bool{}
	for _, m := range kernel.ModuleMarkers() {
		if m.Stubbed {
			for _, e := range m.Envs {
				stubbed[e] = true
			}
		}
	}
	for _, k := range []string{"TOKI_AUDIT", "TOKI_TLS_CHECK", "TOKI_WEBHOOKS", "TOKI_PUSH", "TOKI_BACKUP_VERIFY", "TOKI_ADMIN_UI", "TOKI_WASM"} {
		if !stubbed[k] {
			return k
		}
	}
	t.Skip("every nano switch is compiled out in this build")
	return ""
}

func TestProfileEnvDoesNotLeak(t *testing.T) {
	key := nanoKey(t)
	a := start(t, t.TempDir(), func(o *embed.Options) { o.Listen = "-"; o.Profile = "nano" })
	if os.Getenv(key) != "off" {
		t.Fatalf("nano must set %s=off while running", key)
	}
	stop(t, a)
	if v, ok := os.LookupEnv(key); ok && v == "off" {
		t.Fatalf("%s leaked after Stop", key)
	}
	b := start(t, t.TempDir(), func(o *embed.Options) { o.Listen = "-"; o.Profile = "team" })
	defer stop(t, b)
	if v, ok := os.LookupEnv(key); ok && v == "off" {
		t.Fatal("team inherited the nano profile")
	}
}

func TestHostGuard(t *testing.T) {
	inst := start(t, t.TempDir(), nil)
	defer stop(t, inst)
	req, _ := http.NewRequest("GET", inst.URL()+"/api/health", nil)
	req.Host = "evil.example.com"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("foreign Host: want 403, got %d", resp.StatusCode)
	}
	req, _ = http.NewRequest("GET", inst.URL()+"/api/health", nil)
	req.Header.Set("Origin", "http://evil.example.com")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Access-Control-Allow-Origin") == "*" {
		t.Fatalf("health %d, CORS %q", resp.StatusCode, resp.Header.Get("Access-Control-Allow-Origin"))
	}
}

func TestTwoInstances(t *testing.T) {
	a := start(t, t.TempDir(), func(o *embed.Options) { o.Listen = "-" })
	b := start(t, t.TempDir(), func(o *embed.Options) { o.Listen = "-" })
	defer stop(t, a)
	defer stop(t, b)
	if a.App() == b.App() {
		t.Fatal("instances share an app")
	}
}
