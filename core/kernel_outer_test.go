package core_test

import (
	"testing"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tests"
)

// The kernel hands the "outer" core app to hook handlers, transaction
// callbacks and UnsafeWithoutHooks() so that the server hooks stay reachable.
func TestKernelExposesOuterApp(t *testing.T) {
	t.Parallel()

	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	// transaction callback app
	err := app.RunInTransaction(func(txApp kernel.App) error {
		coreTxApp := core.AsApp(txApp)
		if coreTxApp == nil {
			t.Fatalf("Expected the tx app to be a core.App, got %T", txApp)
		}

		if !coreTxApp.IsTransactional() {
			t.Fatal("Expected a transactional app")
		}

		// request hooks are shared with the parent app
		var calls int
		app.OnRecordCreateRequest().BindFunc(func(e *core.RecordRequestEvent) error {
			calls++
			return nil
		})
		_ = coreTxApp.OnRecordCreateRequest().Trigger(new(core.RecordRequestEvent))
		if calls != 1 {
			t.Fatalf("Expected the request hooks to be shared with the tx app, got %d calls", calls)
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// model event app
	var eventApp kernel.App
	app.OnModelValidate().BindFunc(func(e *kernel.ModelEvent) error {
		eventApp = e.App
		return e.Next()
	})
	if err := app.Validate(core.NewRecord(core.NewBaseCollection("test"))); err != nil && eventApp == nil {
		t.Fatal(err)
	}
	if core.AsApp(eventApp) == nil {
		t.Fatalf("Expected the event app to be a core.App, got %T", eventApp)
	}

	// unsafe app without hooks has its own (empty) request hooks
	unsafeApp := core.AsApp(app.UnsafeWithoutHooks())
	if unsafeApp == nil {
		t.Fatal("Expected UnsafeWithoutHooks() to return a core.App")
	}
	if unsafeApp.OnRecordCreateRequest().Length() != 0 {
		t.Fatal("Expected no request hooks for the unsafe app")
	}
	if app.OnRecordCreateRequest().Length() == 0 {
		t.Fatal("The original app request hooks must not be affected")
	}
}
