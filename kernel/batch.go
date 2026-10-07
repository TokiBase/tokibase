package kernel

import "github.com/tokibase/tokibase/tools/hook"

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

// OnBatch is the process wide list of batch handlers. It lets modules
// (for example the WASM host) observe batches without importing the module
// that emits the events. Bind with hook.Handler; emitting is done by
// modules/batchguard when it is registered.
var OnBatch = &hook.Hook[*BatchEvent]{}
