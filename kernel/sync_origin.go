package kernel

import (
	"context"
	"sync"
)

// SyncApplyMode tells why a record write is happening on behalf of
// modules/sync instead of a normal client or hook.
type SyncApplyMode uint8

const (
	// SyncModePush is a hub replaying a change pushed by a spoke.
	SyncModePush SyncApplyMode = iota + 1
	// SyncModePull is a spoke applying a change pulled from the hub.
	SyncModePull
	// SyncModeSnapshot is a spoke applying a bootstrap snapshot page.
	SyncModeSnapshot
	// SyncModeBundle is a spoke applying a schema bundle.
	SyncModeBundle
)

// SyncOrigin describes the origin of a write that is a sync apply. It travels
// in the context of the save (see [WithSyncOrigin]).
type SyncOrigin struct {
	Mode SyncApplyMode
	// Node is the origin node id of the change.
	Node string
	// HLC is the hybrid logical clock value of the change.
	HLC uint64
	// ChangeID is "<node>:<origin_seq>".
	ChangeID string
	// Actor is the actor grant id of the change.
	Actor string
	// Fields carries origin values for autodate fields (created/updated). The
	// key is "<collectionId>/<recordId>/<field>" (a replayed tx group can touch
	// several records); the value is the datetime string of the origin.
	Fields map[string]any

	// scratch lets the hooks of one apply pass values to each other (see
	// [SyncOrigin.Remember]).
	scratchMu sync.Mutex
	scratch   map[any]any
}

// Remember stores a value for the duration of this apply. modules/crypto uses
// it to carry the original ciphertext from the validate hook to the write hook.
func (o *SyncOrigin) Remember(key, val any) {
	o.scratchMu.Lock()
	if o.scratch == nil {
		o.scratch = map[any]any{}
	}
	o.scratch[key] = val
	o.scratchMu.Unlock()
}

// Recall returns a value stored by [SyncOrigin.Remember].
func (o *SyncOrigin) Recall(key any) (any, bool) {
	o.scratchMu.Lock()
	defer o.scratchMu.Unlock()
	v, ok := o.scratch[key]
	return v, ok
}

// Forget removes a value stored by [SyncOrigin.Remember].
func (o *SyncOrigin) Forget(key any) {
	o.scratchMu.Lock()
	delete(o.scratch, key)
	o.scratchMu.Unlock()
}

type syncOriginKey struct{}

// WithSyncOrigin returns a context that marks a write as a sync apply.
func WithSyncOrigin(ctx context.Context, o *SyncOrigin) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, syncOriginKey{}, o)
}

// SyncOriginFrom returns the origin set by [WithSyncOrigin], or nil when the
// context does not belong to a sync apply.
func SyncOriginFrom(ctx context.Context) *SyncOrigin {
	if ctx == nil {
		return nil
	}
	o, _ := ctx.Value(syncOriginKey{}).(*SyncOrigin)
	return o
}

// IsSyncReplica reports whether the write replicates state that already
// exists on the hub (pull, snapshot or bundle apply). Side effects such as
// outbound webhooks or WASM after-hooks must be skipped for these writes.
func IsSyncReplica(ctx context.Context) bool {
	o := SyncOriginFrom(ctx)
	if o == nil {
		return false
	}
	switch o.Mode {
	case SyncModePull, SyncModeSnapshot, SyncModeBundle:
		return true
	}
	return false
}
