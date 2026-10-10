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
	ErrRebootstrap = errors.New("sync: the hub requires a re-bootstrap")
	// ErrPartial is the result of a background slot that ran out of time after it
	// made progress: what was done is committed, the next slot continues. It is not
	// a failure (no backoff) and not a success (last_ok does not move).
	ErrPartial = errors.New("sync: the background slot ended before the cycle finished")

	// errRebootstrapDeferred ends an automatic cycle on a metered link that learned
	// the hub wants a snapshot: the download waits (internal to the loop).
	errRebootstrapDeferred = errors.New("sync: re-bootstrap deferred until the link is unmetered")
)

// Conditions describe the device (docs/SYNC_DESIGN.md §6.2).
type Conditions struct {
	// Online false means no attempts at all (no wasted radio).
	Online bool
	// Metered: automatic cycles only push (a SyncNow also pulls, pages of at most
	// 100 changes) and the interval is the long one.
	Metered bool
	// LowPower uses the long interval (5 min).
	LowPower bool
	// Background marks an OS-granted background slot: one bounded cycle of at most
	// BackgroundBudget (20 s), then the loop stops until the conditions change or
	// SyncNow is called.
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

// DigestBackend is implemented by a Backend that can compute the per
// collection metadata digests compared with the hub (docs/SYNC_DESIGN.md §3.6).
// The loop sends them only with TOKI_SYNC_AUTO_HEAL=1.
type DigestBackend interface {
	// MetaDigests returns collection id -> digest for the collections the node pulls.
	MetaDigests() (map[string]string, error)
}

// Event types.
const (
	EventApplied        = "applied"
	EventPushed         = "pushed"
	EventRejected       = "rejected"
	EventSuperseded     = "superseded"
	EventParked         = "parked"
	EventError          = "error"
	EventRebootstrap    = "rebootstrap"
	EventRevoked        = "revoked"
	EventDigestMismatch = "digest_mismatch"
	EventSynced         = "synced"
	// EventPartial ends a background slot whose budget ran out in the middle of a cycle.
	EventPartial = "partial"
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
	Parked     int
	Pulled     int
	Applied    int
	// Partial is true when the bounded cycle of a background slot ran out of time
	// before it finished (what was done is committed).
	Partial bool
	// Bootstrapped is true when a snapshot bootstrap moved forward in this cycle.
	Bootstrapped bool
	Err          error
}

// Status is a snapshot of the loop.
type Status struct {
	State       string
	Online      bool
	Paused      bool
	Running     bool
	Pending     int64
	PullAfter   int64
	AckedOrigin int64
	LastOK      time.Time
	// LastPartial is the end of the last background slot that ran out of time after
	// making progress (LastOK does not move for it).
	LastPartial time.Time
	// PullDeferred is true while a metered link holds back the pull (or a snapshot
	// bootstrap) that an unmetered moment or SyncNow will do.
	PullDeferred bool
	LastError    string
	OffsetMs     int64
	Failures     int
	NextAttempt  time.Time
	ApplyErrors  int64
	// HashMismatches counts hash_mismatch checks (§4.7); HashStreak is the current run of them.
	HashMismatches int64
	HashStreak     int
	DigestMismatch []string
	// Heal is "heal_exhausted" when the auto-heal stopped after too many heals.
	Heal string
	// Conditions are the device conditions in force; BackgroundDone is true once
	// the bounded cycle of the current background slot ran.
	Conditions     Conditions
	BackgroundDone bool
}

// EventEpoch is emitted when the hub epoch changed.
const EventEpoch = "epoch"
