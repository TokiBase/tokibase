package kernel_test

import (
	"errors"
	"testing"

	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tests"
)

func TestOnSyncConflictForIsPerApp(t *testing.T) {
	a, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer a.Cleanup()
	b, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Cleanup()

	if kernel.OnSyncConflictFor(a) != kernel.OnSyncConflictFor(a) || kernel.OnSyncConflictFor(a) == kernel.OnSyncConflictFor(b) {
		t.Fatal("one handler list per app instance")
	}
	kernel.OnSyncConflictFor(a).BindFunc(func(e *kernel.SyncConflictEvent) error {
		e.Resolution, e.Message = kernel.SyncResolveMerge, "m"
		e.Patch = map[string]any{"x": 1}
		return nil
	})
	ev := &kernel.SyncConflictEvent{App: a, RecordID: "r"}
	if err := kernel.OnSyncConflictFor(a).Trigger(ev); err != nil || ev.Resolution != "merge" || ev.Patch["x"] != 1 {
		t.Fatalf("%v %+v", err, ev)
	}
	other := &kernel.SyncConflictEvent{App: b}
	if err := kernel.OnSyncConflictFor(b).Trigger(other); err != nil || other.Resolution != "" {
		t.Fatalf("handlers of one app must not see the conflicts of another: %+v", other)
	}
	kernel.OnSyncConflictFor(b).BindFunc(func(e *kernel.SyncConflictEvent) error { return errors.New("boom") })
	if err := kernel.OnSyncConflictFor(b).Trigger(other); err == nil {
		t.Fatal("handler errors propagate")
	}
	kernel.ReleaseSyncHooks(a)
	if kernel.OnSyncConflictFor(a).Length() != 0 {
		t.Fatal("released")
	}
}
