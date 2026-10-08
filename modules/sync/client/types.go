//go:build !no_sync

package client

import (
	"errors"
	"time"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
)

// Errors of the loop.
var (
	ErrOffline     = errors.New("sync: offline (conditions.online is false)")
	ErrPaused      = errors.New("sync: paused")
	ErrStopped     = errors.New("sync: the loop is not running")
	ErrRevoked     = errors.New("sync: this node was revoked")
	ErrRebootstrap = errors.New("sync: the hub requires a re-bootstrap (not supported before PR7)")
)

// Conditions describe the device (docs/SYNC_DESIGN.md §6.2).
type Conditions struct {
	// Online false means no attempts at all (no wasted radio).
	Online bool
	// Metered limits the pull page to 100 and uses the long interval.
	Metered bool
	// LowPower uses the long interval (5 min).
	LowPower bool
	// Background marks an OS-granted background slot (bounded cycles are PR10).
	Background bool
}

// PolicyView is what the client needs to know about the policy of a collection.
type PolicyView struct {
	Direction string
	// Types maps a field to "counter" or "set".
	Types   map[string]string
	Exclude map[string]struct{}
}

// Backend connects the loop to the sync module (policies and hashing live
// there and the client package must not import it).
type Backend interface {
	// Policy returns the policy of col, or nil when the collection is not
	// replicated on this node.
	Policy(col *core.Collection) *PolicyView
	// Rehash recomputes `_sync_meta.hash` of rec from the stored row. The loop
	// calls it after it corrected autodate columns by hand.
	Rehash(tx kernel.App, rec *core.Record) error
}

// Event types.
const (
	EventApplied        = "applied"
	EventPushed         = "pushed"
	EventRejected       = "rejected"
	EventSuperseded     = "superseded"
	EventError          = "error"
	EventRebootstrap    = "rebootstrap"
	EventRevoked        = "revoked"
	EventDigestMismatch = "digest_mismatch"
	EventSynced         = "synced"
)

// Event is emitted by the loop (non-blocking: slow readers lose events).
type Event struct {
	Type       string
	Time       time.Time
	ID         string
	Collection string
	Record     string
	Code       string
	Message    string
}

// Result is the outcome of one sync cycle.
type Result struct {
	Pushed     int
	Rejected   int
	Superseded int
	Pulled     int
	Applied    int
	Err        error
}

// Status is a snapshot of the loop.
type Status struct {
	State          string
	Online         bool
	Paused         bool
	Running        bool
	Pending        int64
	PullAfter      int64
	AckedOrigin    int64
	LastOK         time.Time
	LastError      string
	OffsetMs       int64
	Failures       int
	NextAttempt    time.Time
	ApplyErrors    int64
	DigestMismatch []string
}
