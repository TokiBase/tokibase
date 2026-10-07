package embed_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/tokibase/tokibase/embed"
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
	cancel := inst.Subscribe("records/*", func(b []byte) { events <- b })
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
