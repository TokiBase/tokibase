package kernel

import (
	"context"
	"log/slog"
	"time"

	"github.com/pocketbase/dbx"
)

const (
	// EnvLogsMaxMB caps the live size of auxiliary.db in MiB (default 512,
	// 0 = no size cap). The oldest request logs are pruned when it is exceeded,
	// in addition to the age based retention (settings logs.maxDays).
	EnvLogsMaxMB = "TOKI_LOGS_MAX_MB"

	// DefaultLogsMaxMB is the default of [EnvLogsMaxMB].
	DefaultLogsMaxMB = 512

	logsCapJobId = "__tokiLogsSizeCap__"

	// logsCapLowWater is the fraction of the cap that a prune run shrinks the
	// database to, so that the next runs do not delete a few rows every minute.
	logsCapLowWater = 0.8

	// logsCapChunk is the number of rows deleted by one statement
	// (keeps the write lock of a single statement short).
	logsCapChunk = 10000

	logsCapMaxChunks = 1000
)

// logsMaxBytes returns the auxiliary.db size cap in bytes (0 = disabled).
func logsMaxBytes() int64 {
	if v, set := envNonNegativeInt(EnvLogsMaxMB); set {
		return int64(v) * 1024 * 1024
	}

	return DefaultLogsMaxMB * 1024 * 1024
}

// auxLiveBytes returns the size of the used pages of auxiliary.db
// (the file size minus the free list, the free pages are reused by new logs).
func (app *BaseApp) auxLiveBytes() (int64, error) {
	var row struct {
		Bytes int64 `db:"bytes"`
	}

	err := app.AuxConcurrentDB().NewQuery(
		"SELECT (SELECT page_count FROM pragma_page_count) * (SELECT page_size FROM pragma_page_size)" +
			" - (SELECT freelist_count FROM pragma_freelist_count) * (SELECT page_size FROM pragma_page_size) AS bytes",
	).One(&row)

	return row.Bytes, err
}

// PruneLogsBySize deletes the oldest logs (by insertion order) while the live
// size of auxiliary.db is above maxBytes, down to 80 % of it. It returns the
// number of deleted rows. maxBytes <= 0 does nothing.
//
// The file itself does not shrink (the free pages are reused), so the size of
// auxiliary.db stays bounded by about maxBytes plus the logs written between
// two runs of the job (one minute).
func (app *BaseApp) PruneLogsBySize(ctx context.Context, maxBytes int64) (int64, error) {
	if maxBytes <= 0 {
		return 0, nil
	}

	used, err := app.auxLiveBytes()
	if err != nil || used <= maxBytes {
		return 0, err
	}

	var total int64
	if err := app.AuxConcurrentDB().NewQuery("SELECT count(*) FROM {{" + LogsTableName + "}}").Row(&total); err != nil {
		return 0, err
	}
	if total == 0 {
		return 0, nil
	}

	// the other auxiliary tables are small, so the bytes/row estimate is dominated by the logs
	target := int64(float64(maxBytes) * logsCapLowWater)
	toDelete := int64(float64(total) * float64(used-target) / float64(used))
	if toDelete > total {
		toDelete = total
	}

	var deleted int64

	for i := 0; i < logsCapMaxChunks && deleted < toDelete; i++ {
		if err := ctx.Err(); err != nil {
			return deleted, err
		}

		n := min(int64(logsCapChunk), toDelete-deleted)

		res, err := app.AuxNonconcurrentDB().NewQuery(
			"DELETE FROM {{" + LogsTableName + "}} WHERE rowid IN (SELECT rowid FROM {{" + LogsTableName + "}} ORDER BY rowid LIMIT {:n})",
		).Bind(dbx.Params{"n": n}).WithContext(ctx).Execute()
		if err != nil {
			return deleted, err
		}

		affected, _ := res.RowsAffected()
		if affected == 0 {
			break
		}

		deleted += affected
	}

	return deleted, nil
}

func (app *BaseApp) registerLogsSizeCap() {
	app.Cron().Add(logsCapJobId, "* * * * *", func() {
		if app.auxConn == nil || !app.IsBootstrapped() {
			return
		}

		max := logsMaxBytes()
		if max <= 0 || app.Settings().Logs.MaxDays == 0 {
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		deleted, err := app.PruneLogsBySize(ctx, max)
		if err != nil {
			app.Logger().Warn("Failed to prune logs by size", slog.String("error", err.Error()))
		} else if deleted > 0 {
			app.Logger().Debug("Pruned logs over the size cap", slog.Int64("deleted", deleted), slog.Int64("maxBytes", max))
		}
	})
}
