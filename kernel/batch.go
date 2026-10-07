package kernel

import (
	"sync"

	"github.com/tokibase/tokibase/tools/hook"
)

// Names of the batch events.
const (
	BatchBefore = "batch.before"
	BatchAfter  = "batch.after"
)

// BatchRequest is one sub-request of a `/api/batch` call, reduced to what
// cross-record logic needs. Treat every field as read-only.
type BatchRequest struct {
	Index int
	// Collection is the canonical collection NAME ("" when the URL is not a records URL).
	Collection string
	// Method is POST, PATCH or DELETE (a PUT upsert is resolved to POST or PATCH).
	Method string
	// ID is the record id from the URL (PATCH, DELETE) or, after the batch ran, of the written record.
	ID string
	// Body is the submitted body (batch.before) or the STORED field values read back
	// inside the transaction (batch.after, nil for deleted records).
	Body    map[string]any
	Deleted bool
}

// BatchEvent is emitted around an atomic batch. batch.before runs inside the
// batch transaction before any sub-request, batch.after runs inside the same
// transaction after the last one. A handler error rejects the batch (the
// whole transaction rolls back). App is the transaction app.
type BatchEvent struct {
	hook.Event

	// Name is [BatchBefore] or [BatchAfter].
	Name     string
	Requests []BatchRequest
	// Auth is the authenticated record of the batch request (nil when anonymous).
	Auth *Record
	App  App
}

var batchHooks sync.Map // App -> *hook.Hook[*BatchEvent]

// OnBatchFor returns the batch handler list of ONE app instance. It lets
// modules (for example the WASM host) observe the batches of that app without
// importing the module that emits the events. Handlers bound for one app never
// see the batches of another app in the same process (embedded instances).
// Bind with hook.Handler; emitting is done by modules/batchguard.
// Use the same App value that was passed to batchguard.Register.
func OnBatchFor(app App) *hook.Hook[*BatchEvent] {
	if h, ok := batchHooks.Load(app); ok {
		return h.(*hook.Hook[*BatchEvent])
	}
	h, _ := batchHooks.LoadOrStore(app, &hook.Hook[*BatchEvent]{})
	return h.(*hook.Hook[*BatchEvent])
}

// ReleaseBatchHooks drops the handler list of app (called on terminate).
func ReleaseBatchHooks(app App) { batchHooks.Delete(app) }
