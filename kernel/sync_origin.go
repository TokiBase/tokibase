package kernel

import "context"

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
	// Fields carries origin values for autodate fields (created/updated).
	Fields map[string]any
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
