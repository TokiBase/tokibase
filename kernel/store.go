package kernel

import (
	"context"
	"time"

	"github.com/pocketbase/dbx"
)

// DefaultMaxLockRetries is the max number of retry attempts applied
// by the kernel for the locked (SQLITE_BUSY like) db operations.
const DefaultMaxLockRetries = 12

// ErrKind is a driver independent classification of a store error.
type ErrKind int

const (
	// ErrKindOther is any error that is not classified by the store.
	ErrKindOther ErrKind = iota

	// ErrKindNotFound is a "no rows" error (aka. wraps [database/sql.ErrNoRows]).
	ErrKindNotFound

	// ErrKindUnique is a unique constraint violation.
	ErrKindUnique

	// ErrKindLocked is a transient "database/table is locked" (busy) error
	// that could succeed if retried.
	ErrKindLocked

	// ErrKindConstraint is any other constraint violation (not null, foreign key, check, etc.).
	ErrKindConstraint
)

// DBConfig defines the options for opening a single database (data or auxiliary).
type DBConfig struct {
	// Path is the database location (for file based stores).
	Path string

	// MaxOpenConns and MaxIdleConns configure the pool of the concurrent handle.
	// The nonconcurrent handle is always limited to a single connection.
	MaxOpenConns int
	MaxIdleConns int

	// OptimizeOnMaintain specifies whether DBConn.Maintain should also
	// run the store optimize step (in addition to the WAL checkpoint).
	OptimizeOnMaintain bool

	// CheckpointDisabled (optional) reports whether the manual WAL checkpoint
	// must be skipped (e.g. while a replicator owns the checkpoints).
	CheckpointDisabled func() bool
}

// DBOpener opens the connection handles for a database.
//
// It is the seam through which the kernel gets its (SQLite) driver
// and connection setup (see modules/store/sqlite).
type DBOpener interface {
	Open(ctx context.Context, cfg DBConfig) (DBConn, error)
}

// DBConn is an opened database with its concurrent and nonconcurrent handles.
//
// The kernel executes all queries through the dbx handles; everything that is
// specific to the driver (pool setup, pragmas, maintenance, lock retries and
// error classification) lives behind this interface.
type DBConn interface {
	// Concurrent returns the handle for concurrent (read) operations.
	Concurrent() *dbx.DB

	// Nonconcurrent returns the single connection handle for write operations.
	Nonconcurrent() *dbx.DB

	// Close closes both handles.
	Close() error

	// Maintain runs the periodic maintenance (WAL checkpoint and, if enabled
	// in the DBConfig, optimize).
	Maintain(ctx context.Context) error

	// Optimize runs the store optimize step on db
	// (db is usually the nonconcurrent handle or a transaction).
	Optimize(ctx context.Context, db dbx.Builder) error

	// Checkpoint runs a (truncating) WAL checkpoint on db.
	Checkpoint(ctx context.Context, db dbx.Builder) error

	// Vacuum reclaims the unused disk space using db.
	Vacuum(ctx context.Context, db dbx.Builder) error

	// VacuumInto writes a consistent live copy of the database to path using db.
	VacuumInto(ctx context.Context, db dbx.Builder, path string) error

	// ErrorKind classifies err.
	ErrorKind(err error) ErrKind

	// LockRetry runs op and retries it (up to maxRetries times, with backoff)
	// while it fails with an [ErrKindLocked] error.
	LockRetry(maxRetries int, op func(attempt int) error) error

	// ExecLockRetry returns an exec hook that applies the same retry
	// policy as LockRetry and sets a timeout context if the query has none.
	ExecLockRetry(timeout time.Duration, maxRetries int) dbx.ExecHookFunc

	// Route returns a builder that routes the read queries to the concurrent
	// builder and everything else to the nonconcurrent one
	// (both are expected to use the same driver).
	Route(concurrent, nonconcurrent dbx.Builder) dbx.Builder
}
