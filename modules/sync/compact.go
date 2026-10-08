//go:build !no_sync

package sync

import (
	"context"
	"errors"
	"os"
	"strconv"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/sync/hlc"
	"github.com/tokibase/tokibase/tools/types"
)

// Compaction (docs/SYNC_DESIGN.md §3.6): the change log must not grow for ever.
// The hub keeps what its slowest ACTIVE node has not pulled yet (and at least
// TOKI_SYNC_MIN_KEEP), drops everything older than TOKI_SYNC_RETENTION, and
// marks nodes that were silent for longer than the retention `stale`; a stale
// node, or one whose cursor is below `low_water`, has to re-bootstrap.

// Env and defaults.
const (
	EnvMinKeep   = "TOKI_SYNC_MIN_KEEP"
	EnvSpokeKeep = "TOKI_SYNC_SPOKE_KEEP"

	DefaultMinKeep   = 24 * time.Hour
	DefaultSpokeKeep = 24 * time.Hour
	// ConflictKeep is how long resolved conflicts are kept.
	ConflictKeep = 90 * 24 * time.Hour

	// CompactKind is the job kind of the hourly compaction.
	CompactKind = "sync.compact"
	// compactCron runs at minute 0 of every hour.
	compactCron = "0 * * * *"
)

func envDuration(name string, def time.Duration) time.Duration {
	if d, ok := parseDuration(os.Getenv(name)); ok {
		return d
	}
	return def
}

// minKeep is TOKI_SYNC_MIN_KEEP: acknowledged hub changes are kept this long.
func minKeep() time.Duration { return envDuration(EnvMinKeep, DefaultMinKeep) }

// spokeKeep is TOKI_SYNC_SPOKE_KEEP: acked spoke rows are kept this long.
func spokeKeep() time.Duration { return envDuration(EnvSpokeKeep, DefaultSpokeKeep) }

// CompactReport is what one compaction did.
type CompactReport struct {
	Role              string `json:"role"`
	StaleNodes        int64  `json:"stale_nodes"`
	Safe              int64  `json:"safe"`
	ChangesDeleted    int64  `json:"changes_deleted"`
	LowWater          int64  `json:"low_water"`
	TombstonesDeleted int64  `json:"tombstones_deleted"`
	ConflictsDeleted  int64  `json:"conflicts_deleted"`
	SpokeRowsDeleted  int64  `json:"spoke_rows_deleted"`
	Vacuumed          bool   `json:"vacuumed"`
}

func cutoff(now time.Time, d time.Duration) string {
	return now.UTC().Add(-d).Format(types.DefaultDateLayout)
}

// Compact runs one compaction (hub: steps 1-4, spoke: steps 3 and 5). It is
// idempotent and safe to run concurrently with pushes (it takes the apply lock).
func (m *Module) Compact(ctx context.Context) (*CompactReport, error) {
	if !m.ready.Load() {
		return nil, errors.New("sync: the module is not initialized")
	}
	m.applyMu.Lock()
	defer m.applyMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	now := m.now()
	ret := retention()
	rep := &CompactReport{Role: string(m.role)}
	retCut := cutoff(now, ret)

	// the clock floor must survive the deletion of the rows that carry the newest HLCs
	if c := m.Clock(); c != nil {
		if err := hlc.SaveFloor(dbState{db: m.app.NonconcurrentDB()}, c.Last()); err != nil {
			return nil, err
		}
	}

	err := m.app.RunInTransaction(func(tx kernel.App) error {
		db := tx.NonconcurrentDB()
		if _, err := db.NewQuery("UPDATE _sync_state SET value=value WHERE key={:k}").Bind(dbx.Params{"k": keyNodeID}).Execute(); err != nil {
			return err
		}
		if m.role == RoleHub {
			if err := m.compactHub(tx, db, now, retCut, rep); err != nil {
				return err
			}
		}
		// 3. delete tombstones past the retention; legal ones are never pruned
		// (and the trigger would refuse it anyway)
		res, err := db.NewQuery("DELETE FROM _sync_tombstones WHERE kind='delete' AND created < {:c}").Bind(dbx.Params{"c": retCut}).Execute()
		if err != nil {
			return err
		}
		rep.TombstonesDeleted, _ = res.RowsAffected()

		if m.role == RoleHub {
			// 4. resolved conflicts older than 90 days
			if tx.HasTable(ConflictsCollection) {
				res, err := db.NewQuery("DELETE FROM {{" + ConflictsCollection + "}} WHERE [[status]]='resolved' AND COALESCE(NULLIF([[resolved_at]],''), [[created]]) < {:c}").
					Bind(dbx.Params{"c": cutoff(now, ConflictKeep)}).Execute()
				if err != nil {
					return err
				}
				rep.ConflictsDeleted, _ = res.RowsAffected()
			}
		} else {
			// 5. spoke: rows the hub acknowledged are only kept for a re-push after a hub restore
			res, err := db.NewQuery("DELETE FROM _changes WHERE status='acked' AND created < {:c}").
				Bind(dbx.Params{"c": cutoff(now, spokeKeep())}).Execute()
			if err != nil {
				return err
			}
			rep.SpokeRowsDeleted, _ = res.RowsAffected()
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	rep.LowWater = m.lowWater()
	return rep, nil
}

// compactHub runs the hub specific steps 1 and 2.
func (m *Module) compactHub(tx kernel.App, db dbx.Builder, now time.Time, retCut string, rep *CompactReport) error {
	// 1. silent nodes become stale and leave the minimum
	res, err := db.NewQuery("UPDATE " + NodesCollection + " SET status={:s}, updated={:u} WHERE status={:a} AND COALESCE(NULLIF(last_seen,''), created) < {:c}").
		Bind(dbx.Params{"s": NodeStale, "a": NodeActive, "c": retCut, "u": now.UTC().Format(types.DefaultDateLayout)}).Execute()
	if err != nil {
		return err
	}
	rep.StaleNodes, _ = res.RowsAffected()

	// 2. safe = the lowest seq every active node has pulled. Without an active
	// node nothing is known to be pulled, so only the retention deletes rows.
	var safe int64
	if err := db.NewQuery("SELECT COALESCE(MIN(COALESCE(pulled_seq,0)),0) FROM " + NodesCollection + " WHERE status={:a}").
		Bind(dbx.Params{"a": NodeActive}).Row(&safe); err != nil {
		return err
	}
	rep.Safe = safe
	// parked changes wait for an admin decision and are never compacted
	where := "status!='parked' AND ((seq <= {:safe} AND created < {:mk}) OR created < {:ret})"
	p := dbx.Params{"safe": safe, "mk": cutoff(now, minKeep()), "ret": retCut}
	var maxSeq int64
	if err := db.NewQuery("SELECT COALESCE(MAX(seq),0) FROM _changes WHERE " + where).Bind(p).Row(&maxSeq); err != nil {
		return err
	}
	if maxSeq == 0 {
		return nil
	}
	res, err = db.NewQuery("DELETE FROM _changes WHERE " + where).Bind(p).Execute()
	if err != nil {
		return err
	}
	rep.ChangesDeleted, _ = res.RowsAffected()
	// low_water is the highest seq that was removed: a cursor at or above it can
	// still resume (the node that acknowledged exactly `safe` must not be sent away)
	if cur := m.lowWater(); maxSeq > cur {
		return dbState{db: db}.Set(keyLowWater, strconv.FormatInt(maxSeq, 10))
	}
	return nil
}

// Vacuum rewrites data.db to give the freed pages back to the file system.
func (m *Module) Vacuum() error {
	_, err := m.app.NonconcurrentDB().NewQuery("VACUUM").Execute()
	return err
}

// bindCompaction registers the job handler and the hourly cron entry.
func (m *Module) bindCompaction() {
	app := m.app
	kernel.Jobs(app).Register(CompactKind, func(ctx context.Context, _ kernel.App, _ *kernel.Job) error {
		_, err := m.Compact(ctx)
		return err
	})
	_ = app.Cron().Add("__tokiSyncCompact", compactCron, func() { m.enqueueCompact(time.Now()) })
}

// compactKey is the cron slot of an hour (design: CronKey("sync.compact:<hour>")).
func compactKey(t time.Time) string { return CompactKind + ":" + t.UTC().Format("2006010215") }

// enqueueCompact queues the compaction of the hour of now through the job
// queue (deduplicated by the cron key across processes). Without a job queue
// (jobs module off) it runs inline.
func (m *Module) enqueueCompact(now time.Time) {
	_, err := kernel.Jobs(m.app).Enqueue(context.Background(), CompactKind, nil, kernel.CronKey(compactKey(now)), kernel.MaxAttempts(3))
	if errors.Is(err, kernel.ErrNoJobQueue) {
		_, err = m.Compact(context.Background())
	}
	if err != nil {
		m.app.Logger().Error("sync: compaction failed", "error", err)
	}
}
