package kernel

import (
	"sync"
	"time"

	"github.com/tokibase/tokibase/tools/hook"
)

// SyncConflictEventName is the name of the sync conflict event as guests and
// logs see it (the full guest event is "sync.conflict.<collection>").
const SyncConflictEventName = "sync.conflict"

// Resolutions a [SyncConflictEvent] handler can set (docs/SYNC_DESIGN.md §4.6).
const (
	// SyncResolveAccept applies the incoming patch as it is.
	SyncResolveAccept = "accept"
	// SyncResolveReject refuses the incoming change; the node gets a revert.
	SyncResolveReject = "reject"
	// SyncResolveMerge applies [SyncConflictEvent.Patch] instead of the incoming patch.
	SyncResolveMerge = "merge"
	// SyncResolvePark applies nothing and leaves the conflict open for an admin.
	SyncResolvePark = "park"
)

// SyncIncoming is the pushed change that conflicts with the hub state.
// Treat every field as read-only.
type SyncIncoming struct {
	// Op is c (create) or u (update).
	Op      string
	Node    string
	HLC     uint64
	BaseHLC uint64
	// Patch is the pushed patch; the value of a sensitive field is [SensitiveMarker].
	Patch map[string]any
	// ActorKind is auth, superuser or system; ActorID and ActorCollection
	// (collection NAME) are set for auth actors.
	ActorKind       string
	ActorID         string
	ActorCollection string
}

// SyncConflictEvent is emitted by modules/sync on the hub for a concurrent
// change in a collection whose strategy is `hook`. Handlers decide by setting
// Resolution (and Patch for merge); they run inside the apply transaction (App
// is the transaction app). A handler error, an empty or unknown Resolution all
// mean "no decision": the hub parks the change (fail closed, code hook_failed).
type SyncConflictEvent struct {
	hook.Event

	App        App
	Collection *Collection
	RecordID   string
	// Hook is the module named by the policy (`hook` field); "" = every
	// module that subscribes to the collection.
	Hook string
	// Current is the hub record (synced fields, sensitive ones redacted).
	Current     map[string]any
	CurrentHLC  uint64
	CurrentNode string
	Incoming    SyncIncoming
	// FieldClocks are the per-field clocks of the record (only filled for
	// collections that keep them).
	FieldClocks map[string]uint64
	// Deadline is the end of the hook time budget shared by all conflicts of one
	// push (zero = no limit); handlers must not run past it.
	Deadline time.Time

	// Set by handlers.
	Resolution string
	Patch      map[string]any
	Message    string
}

var syncConflictHooks sync.Map // App -> *hook.Hook[*SyncConflictEvent]

// OnSyncConflictFor returns the sync conflict handler list of ONE app instance.
// It lets modules/wasm answer the conflicts of that app without importing
// modules/sync, which emits the event. Use the same App value that was passed
// to sync.Register.
func OnSyncConflictFor(app App) *hook.Hook[*SyncConflictEvent] {
	if h, ok := syncConflictHooks.Load(app); ok {
		return h.(*hook.Hook[*SyncConflictEvent])
	}
	h, _ := syncConflictHooks.LoadOrStore(app, &hook.Hook[*SyncConflictEvent]{})
	return h.(*hook.Hook[*SyncConflictEvent])
}

// ReleaseSyncHooks drops the handler list of app (called on terminate).
func ReleaseSyncHooks(app App) { syncConflictHooks.Delete(app) }
