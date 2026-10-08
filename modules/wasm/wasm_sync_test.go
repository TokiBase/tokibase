//go:build !no_wasm

package wasm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tokibase/tokibase/kernel"
)

func syncTOML(mode, events string, timeoutMS ...int) string {
	to := 1500
	if len(timeoutMS) > 0 {
		to = timeoutMS[0]
	}
	return "events = [" + events + "]\ntimeout_ms = " + itoa(to) + "\n[env]\nMODE = \"" + mode + "\"\n"
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func conflictEvent(e *env, hookName string) *kernel.SyncConflictEvent {
	col, _ := e.app.FindCollectionByNameOrId("posts")
	return &kernel.SyncConflictEvent{
		App: e.app, Collection: col, RecordID: "rec1", Hook: hookName,
		Current: map[string]any{"title": "hub", "status": "paid"}, CurrentHLC: 0x10, CurrentNode: "nHub",
		Incoming: kernel.SyncIncoming{Op: "u", Node: "nPhone", HLC: 0x20, BaseHLC: 0x5,
			Patch: map[string]any{"title": "phone"}, ActorKind: "auth", ActorID: "u1", ActorCollection: "officers"},
		FieldClocks: map[string]uint64{"title": 0x10},
	}
}

func TestSyncConflictResolutions(t *testing.T) {
	for _, mode := range []string{"accept", "reject", "merge", "park"} {
		t.Run(mode, func(t *testing.T) {
			e := newEnv(t, modSpec{"pay", guest(t, "syncconflict"), syncTOML(mode, `"sync.conflict.posts"`)})
			ev := conflictEvent(e, "")
			if err := kernel.OnSyncConflictFor(e.app).Trigger(ev); err != nil {
				t.Fatal(err)
			}
			if ev.Resolution != mode {
				t.Fatalf("resolution %q, want %q", ev.Resolution, mode)
			}
			if mode == "merge" && ev.Patch["note"] != "merged by guest" {
				t.Fatalf("patch %v", ev.Patch)
			}
		})
	}
}

func TestSyncConflictPayloadAndWildcard(t *testing.T) {
	e := newEnv(t, modSpec{"pay", guest(t, "syncconflict"), syncTOML("echo", `"sync.conflict.*"`)})
	ev := conflictEvent(e, "pay")
	if err := kernel.OnSyncConflictFor(e.app).Trigger(ev); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Event, Kind, Collection string
		Sync                    SyncIn
	}
	if err := json.Unmarshal([]byte(ev.Message), &got); err != nil {
		t.Fatalf("%v: %s", err, ev.Message)
	}
	s := got.Sync
	if got.Event != "sync.conflict.posts" || got.Kind != "sync" || got.Collection != "posts" ||
		s.RecordID != "rec1" || s.CurrentHLC != "0000000000000010" || s.CurrentNode != "nHub" ||
		s.Incoming.HLC != "0000000000000020" || s.Incoming.BaseHLC != "0000000000000005" || s.Incoming.Op != "u" ||
		s.Incoming.Patch["title"] != "phone" || s.Incoming.Actor.Kind != "auth" || s.Incoming.Actor.ID != "u1" ||
		s.Incoming.Actor.Collection != "officers" || s.FieldClocks["title"] != "0000000000000010" || s.Current["status"] != "paid" {
		t.Fatalf("payload: %s", ev.Message)
	}
}

func TestSyncConflictFailsClosed(t *testing.T) {
	cases := []struct {
		mode string
		toMS int
	}{{"trap", 1500}, {"fail", 1500}, {"bogus", 1500}, {"spin", 300}}
	for _, c := range cases {
		t.Run(c.mode, func(t *testing.T) {
			e := newEnv(t, modSpec{"pay", guest(t, "syncconflict"), syncTOML(c.mode, `"sync.conflict.posts"`, c.toMS)})
			ev := conflictEvent(e, "")
			start := time.Now()
			err := kernel.OnSyncConflictFor(e.app).Trigger(ev)
			if err == nil || ev.Resolution != "" {
				t.Fatalf("%s must fail closed (no resolution, an error): err=%v resolution=%q", c.mode, err, ev.Resolution)
			}
			if time.Since(start) > 8*time.Second {
				t.Fatalf("too slow: %s", time.Since(start))
			}
		})
	}
}

func TestSyncConflictNoOpinionAndOtherCollections(t *testing.T) {
	e := newEnv(t, modSpec{"pay", guest(t, "syncconflict"), syncTOML("none", `"sync.conflict.posts"`)})
	ev := conflictEvent(e, "")
	if err := kernel.OnSyncConflictFor(e.app).Trigger(ev); err != nil || ev.Resolution != "" {
		t.Fatalf("no opinion leaves the event undecided: %v %q", err, ev.Resolution)
	}
	// a module for another collection, or a policy naming another module, is not asked
	e = newEnv(t, modSpec{"pay", guest(t, "syncconflict"), syncTOML("accept", `"sync.conflict.orders"`)})
	if err := kernel.OnSyncConflictFor(e.app).Trigger(conflictEvent(e, "")); err != nil {
		t.Fatal(err)
	}
	e = newEnv(t, modSpec{"pay", guest(t, "syncconflict"), syncTOML("accept", `"sync.conflict.posts"`)})
	ev = conflictEvent(e, "other")
	if err := kernel.OnSyncConflictFor(e.app).Trigger(ev); err != nil || ev.Resolution != "" {
		t.Fatalf("policy hook name: %v %q", err, ev.Resolution)
	}
}

func TestSyncConflictHandlerOnlyBoundWhileNeeded(t *testing.T) {
	e := newEnv(t)
	hooks := kernel.OnSyncConflictFor(e.app)
	if hooks.Length() != 0 {
		t.Fatalf("no module: handler must not be bound (%d)", hooks.Length())
	}
	copyFile(t, guest(t, "syncconflict"), filepath.Join(e.dir, "pay.wasm"))
	if err := os.WriteFile(filepath.Join(e.dir, "pay.toml"), []byte(syncTOML("accept", `"sync.conflict.posts"`)), 0o644); err != nil {
		t.Fatal(err)
	}
	e.h.Reload()
	if hooks.Length() != 1 {
		t.Fatalf("bound %d: %v", hooks.Length(), e.h.LoadErrors())
	}
	os.Remove(filepath.Join(e.dir, "pay.wasm"))
	e.h.Reload()
	if hooks.Length() != 0 {
		t.Fatal("handler still bound after the module was removed")
	}
}

func TestParseSyncConflictEvents(t *testing.T) {
	for _, ok := range []string{"sync.conflict.payments", "sync.conflict.*"} {
		ev, err := ParseEvent(ok)
		if err != nil || ev.Kind != KindSync {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"sync.conflict.", "sync.conflict", "sync.conflict.a b", "sync.conflict.a.b"} {
		if _, err := ParseEvent(bad); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
	ev, _ := ParseEvent("sync.conflict.*")
	if !ev.matchSync("payments") || ev.matchSync("_sync_nodes") {
		t.Error("wildcard match")
	}
	ev, _ = ParseEvent("sync.conflict.payments")
	if !ev.matchSync("payments") || ev.matchSync("orders") || strings.Contains(ev.Raw, " ") {
		t.Error("exact match")
	}
}
