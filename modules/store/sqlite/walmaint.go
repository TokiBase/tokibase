package sqlite

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/kernel"
)

const (
	// walTruncateAttempts bounds the truncating checkpoint retries of one round.
	walTruncateAttempts = 3

	// walTruncateTimeout bounds one truncating attempt. SQLite blocks the
	// writers and waits (busy handler) for the readers that still use an older
	// snapshot; new readers already read the database file only, so the wait
	// is about the longest running read query.
	walTruncateTimeout = 15 * time.Second
)

var _ kernel.WALMaintainer = (*conn)(nil)

// walSize returns the size of the -wal file (0 if missing).
func (c *conn) walSize() int64 {
	fi, err := os.Stat(c.cfg.Path + "-wal")
	if err != nil {
		return 0
	}

	return fi.Size()
}

// WALStatus implements [kernel.WALMaintainer].
func (c *conn) WALStatus() kernel.WALStatus {
	c.walMu.Lock()
	st := c.wal
	c.walMu.Unlock()

	st.Disabled = c.cfg.CheckpointDisabled != nil && c.cfg.CheckpointDisabled()
	st.SizeBytes = c.walSize()

	return st
}

func (c *conn) updateWAL(fn func(st *kernel.WALStatus)) {
	c.walMu.Lock()
	fn(&c.wal)
	c.walMu.Unlock()
}

// walCheckpoint runs PRAGMA wal_checkpoint(mode) on db and returns
// the busy flag, the WAL frames and the checkpointed frames.
func walCheckpoint(ctx context.Context, db dbx.Builder, mode string) (busy, logFrames, checkpointed int64, err error) {
	err = db.NewQuery("PRAGMA wal_checkpoint("+mode+")").WithContext(ctx).Row(&busy, &logFrames, &checkpointed)

	return busy, logFrames, checkpointed, err
}

// MaintainWAL implements [kernel.WALMaintainer].
//
// The automatic checkpoint of SQLite is PASSIVE: it copies the frames but can
// only restart (reuse) the log when no reader uses it. With readers that
// always overlap, that never happens and the WAL only grows. Above maxBytes
// this runs a TRUNCATE checkpoint, which blocks the writers, lets the readers
// on the old snapshots finish while the new ones already read the database
// file only, and then resets the log.
func (c *conn) MaintainWAL(ctx context.Context, maxBytes int64) error {
	if c.cfg.CheckpointDisabled != nil && c.cfg.CheckpointDisabled() {
		return nil // an external replicator owns the WAL checkpoints
	}

	c.updateWAL(func(st *kernel.WALStatus) { st.MaxBytes = maxBytes })

	// PASSIVE on the read pool: it never blocks the writers and does not queue behind them
	_, frames, done, err := walCheckpoint(ctx, c.concurrentDB, "PASSIVE")
	if err != nil {
		c.updateWAL(func(st *kernel.WALStatus) { st.LastError = err.Error() })
		return fmt.Errorf("wal_checkpoint(PASSIVE): %w", err)
	}

	c.updateWAL(func(st *kernel.WALStatus) {
		st.Checkpoints++
		st.LastRunAt = time.Now()
		st.LastMode = "PASSIVE"
		st.LastLogFrames, st.LastCheckpointed = frames, done
		st.LastError = ""
	})

	if maxBytes <= 0 || c.walSize() <= maxBytes {
		return nil
	}

	var (
		lastErr error
		busy    int64
	)

	for attempt := 1; attempt <= walTruncateAttempts; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, walTruncateTimeout)
		busy, frames, done, lastErr = walCheckpoint(attemptCtx, c.nonconcurrentDB, "TRUNCATE")
		cancel()

		if lastErr == nil && busy == 0 {
			c.updateWAL(func(st *kernel.WALStatus) {
				st.Escalations++
				st.LastRunAt = time.Now()
				st.LastTruncateAt = st.LastRunAt
				st.LastMode = "TRUNCATE"
				st.LastLogFrames, st.LastCheckpointed = frames, done
				st.LastError = ""
			})

			return nil
		}

		if lastErr == nil {
			lastErr = fmt.Errorf("readers did not drain (attempt %d/%d)", attempt, walTruncateAttempts)
		}

		select {
		case <-ctx.Done():
			attempt = walTruncateAttempts
		case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
		}
	}

	c.updateWAL(func(st *kernel.WALStatus) {
		st.Failures++
		st.LastError = lastErr.Error()
	})

	return fmt.Errorf("wal_checkpoint(TRUNCATE): %w", lastErr)
}
