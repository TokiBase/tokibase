package kernel_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/tokibase/tokibase/kernel"
)

func TestDerivedFields(t *testing.T) {
	const id = "col_derived_test"
	t.Cleanup(func() {
		kernel.UnregisterDerivedField(id, "b")
		kernel.UnregisterDerivedField(id, "a")
	})
	if kernel.IsDerived(id, "a") || kernel.DerivedFieldsOf(id) != nil {
		t.Fatal("must start empty")
	}
	kernel.RegisterDerivedField(id, "b")
	kernel.RegisterDerivedField(id, "a")
	kernel.RegisterDerivedField(id, "a") // idempotent
	kernel.RegisterDerivedField("", "x") // ignored
	kernel.RegisterDerivedField(id, "")  // ignored
	if !kernel.IsDerived(id, "a") || kernel.IsDerived(id, "c") || kernel.IsDerived("other", "a") {
		t.Fatal("IsDerived")
	}
	if got := kernel.DerivedFieldsOf(id); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("sorted list: %v", got)
	}
	kernel.UnregisterDerivedField(id, "a")
	kernel.UnregisterDerivedField(id, "b")
	if kernel.IsDerived(id, "b") || kernel.DerivedFieldsOf(id) != nil {
		t.Fatal("unregister")
	}
	kernel.UnregisterDerivedField(id, "never") // no panic
}

func TestSyncOrigin(t *testing.T) {
	bg := context.Background()
	if kernel.SyncOriginFrom(bg) != nil || kernel.IsSyncReplica(bg) {
		t.Fatal("plain context is not a sync apply")
	}
	for mode, replica := range map[kernel.SyncApplyMode]bool{
		kernel.SyncModePush: false, kernel.SyncModePull: true,
		kernel.SyncModeSnapshot: true, kernel.SyncModeBundle: true,
	} {
		o := &kernel.SyncOrigin{Mode: mode, Node: "n1"}
		ctx := kernel.WithSyncOrigin(bg, o)
		if kernel.SyncOriginFrom(ctx) != o || kernel.IsSyncReplica(ctx) != replica {
			t.Fatalf("mode %d", mode)
		}
	}
	if kernel.SyncOriginFrom(kernel.WithSyncOrigin(nil, &kernel.SyncOrigin{Mode: kernel.SyncModePull})) == nil {
		t.Fatal("nil parent context")
	}
	if kernel.RequestInfoContextSync != "sync" {
		t.Fatal("reserved context value")
	}
}
