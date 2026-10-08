package kernel

import (
	"context"
	"log/slog"
	"time"
)

const (
	// EnvWALMaxMB is the WAL size in MiB above which the periodic maintenance
	// escalates from a PASSIVE to a truncating checkpoint (default 256, 0 = never escalate).
	EnvWALMaxMB = "TOKI_WAL_MAX_MB"

	// DefaultWALMaxMB is the default of [EnvWALMaxMB].
	DefaultWALMaxMB = 256

	walMaintainJobId   = "__tokiWALMaintain__"
	walMaintainTimeout = 2 * time.Minute
)

// WALStatus is a snapshot of the WAL maintenance of one database (see [WALMaintainer]).
type WALStatus struct {
	// Disabled is true while an external replicator owns the checkpoints.
	Disabled bool `json:"disabled"`

	SizeBytes int64 `json:"sizeBytes"`
	MaxBytes  int64 `json:"maxBytes"`

	// Checkpoints counts PASSIVE runs, Escalations the truncating runs
	// and Failures the escalations that could not finish (readers never drained).
	Checkpoints int64 `json:"checkpoints"`
	Escalations int64 `json:"escalations"`
	Failures    int64 `json:"failures"`

	LastRunAt        time.Time `json:"lastRunAt"`
	LastMode         string    `json:"lastMode"`
	LastLogFrames    int64     `json:"lastLogFrames"`
	LastCheckpointed int64     `json:"lastCheckpointed"`
	LastTruncateAt   time.Time `json:"lastTruncateAt"`
	LastError        string    `json:"lastError,omitempty"`
}

// WALMaintainer is optionally implemented by a [DBConn] whose database has a
// write-ahead log that sustained overlapping readers can keep from resetting
// (the automatic checkpoint is PASSIVE and can never restart the log while a
// reader is always active). The kernel runs MaintainWAL every minute.
type WALMaintainer interface {
	// MaintainWAL runs one maintenance round: a PASSIVE checkpoint and, when the
	// WAL is still larger than maxBytes (0 = never), a truncating checkpoint
	// with a short bounded reader-drain wait and bounded retries.
	MaintainWAL(ctx context.Context, maxBytes int64) error

	// WALStatus returns the counters of the last rounds.
	WALStatus() WALStatus
}

// PoolStats is a snapshot of a connection pool.
type PoolStats struct {
	MaxOpen   int   `json:"maxOpen"`
	Open      int   `json:"open"`
	InUse     int   `json:"inUse"`
	Idle      int   `json:"idle"`
	WaitCount int64 `json:"waitCount"`
	WaitMs    int64 `json:"waitMs"`
}

// DBStatus is the runtime status of one database (data or auxiliary).
type DBStatus struct {
	Pool PoolStats  `json:"pool"`
	WAL  *WALStatus `json:"wal,omitempty"`
}

// walMaxBytes returns the escalation threshold in bytes.
func walMaxBytes() int64 {
	if v, set := envNonNegativeInt(EnvWALMaxMB); set {
		return int64(v) * 1024 * 1024
	}

	return DefaultWALMaxMB * 1024 * 1024
}

// DBStatus returns the pool and WAL status of the "data" and "auxiliary" databases
// (empty before the bootstrap).
func (app *BaseApp) DBStatus() map[string]DBStatus {
	result := map[string]DBStatus{}

	for name, conn := range map[string]DBConn{"data": app.dataConn, "auxiliary": app.auxConn} {
		if conn == nil {
			continue
		}

		var st DBStatus

		if db := conn.Concurrent(); db != nil && db.DB() != nil {
			s := db.DB().Stats()
			st.Pool = PoolStats{
				MaxOpen:   s.MaxOpenConnections,
				Open:      s.OpenConnections,
				InUse:     s.InUse,
				Idle:      s.Idle,
				WaitCount: s.WaitCount,
				WaitMs:    s.WaitDuration.Milliseconds(),
			}
		}

		if wm, ok := conn.(WALMaintainer); ok {
			w := wm.WALStatus()
			st.WAL = &w
		}

		result[name] = st
	}

	return result
}

func (app *BaseApp) registerWALMaintenance() {
	app.Cron().Add(walMaintainJobId, "* * * * *", func() {
		for name, conn := range map[string]DBConn{"data": app.dataConn, "auxiliary": app.auxConn} {
			wm, ok := conn.(WALMaintainer)
			if !ok || conn == nil {
				continue
			}

			ctx, cancel := context.WithTimeout(context.Background(), walMaintainTimeout)
			err := wm.MaintainWAL(ctx, walMaxBytes())
			cancel()

			if err != nil && app.Logger() != nil {
				app.Logger().Warn("WAL maintenance failed", slog.String("db", name), slog.String("error", err.Error()))
			}
		}
	})
}
