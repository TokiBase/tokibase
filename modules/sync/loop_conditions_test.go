//go:build !no_sync

package sync

import (
	"testing"
	"time"

	"github.com/tokibase/tokibase/modules/sync/client"
)

// Conditions end to end against a real hub (docs/SYNC_DESIGN.md §6.2): a metered
// link pushes but does not pull until SyncNow, a background slot runs one cycle.
func TestLoopMeteredPushOnlyAndBackgroundSlot(t *testing.T) {
	h, a, _ := hubFixture(t)
	fs := newFsched()
	cl := a.client(t, h, func(o *client.Options) {
		o.Backend = backend{a.m}
		o.Sched, o.Now = fs, fs.Now
		o.NoPoke, o.Interval = true, 30*time.Second
		o.Rand = func() float64 { return 0.5 }
	})
	local := a.create(t, map[string]any{"title": "from the phone"})
	onHub := h.create(t, map[string]any{"title": "from the hub"})

	haveLocal := func(id string) bool { _, err := a.app.FindRecordById("items", id); return err == nil }
	haveHub := func(id string) bool { _, err := h.app.FindRecordById("items", id); return err == nil }
	waitFor := func(what string, f func() bool) {
		t.Helper()
		for i := 0; i < 100; i++ {
			fs.Advance(0)
			if f() {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("timed out: %s", what)
	}

	cl.SetConditions(client.Conditions{Online: true, Metered: true})
	cl.Start(ctxb)
	t.Cleanup(func() { _ = cl.Stop(ctxb) })
	waitFor("push on a metered link", func() bool { return haveHub(local.Id) })
	if haveLocal(onHub.Id) {
		t.Fatal("a metered link must not pull in an automatic cycle")
	}
	fs.Advance(10 * time.Minute) // later automatic cycles still do not pull
	if haveLocal(onHub.Id) {
		t.Fatal("a metered link must not pull on the long interval either")
	}
	if r := <-cl.SyncNow(); r.Err != nil || r.Pulled == 0 {
		t.Fatalf("SyncNow on a metered link pulls: %+v", r)
	}
	if !haveLocal(onHub.Id) {
		t.Fatal("SyncNow must have pulled the hub record")
	}

	// background slot: one cycle (push and pull), then silence until SyncNow
	cl.SetConditions(client.Conditions{Online: true})
	second := h.create(t, map[string]any{"title": "second hub record"})
	waitFor("unmetered pull", func() bool { return haveLocal(second.Id) })
	cl.SetConditions(client.Conditions{Online: true, Background: true})
	third := h.create(t, map[string]any{"title": "third hub record"})
	waitFor("background slot cycle", func() bool { return haveLocal(third.Id) && cl.Status().BackgroundDone })
	fourth := h.create(t, map[string]any{"title": "fourth hub record"})
	fs.Advance(time.Hour)
	cl.Kick()
	time.Sleep(100 * time.Millisecond)
	if haveLocal(fourth.Id) {
		t.Fatal("a background slot runs one cycle only")
	}
	if r := <-cl.SyncNow(); r.Err != nil || !haveLocal(fourth.Id) {
		t.Fatalf("SyncNow in a slot: %+v", r)
	}
}
