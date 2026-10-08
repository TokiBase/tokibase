//go:build !no_replica

package walreplica

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/benbjohnson/litestream"
	"github.com/tokibase/tokibase/kernel"
)

const storeKey = "@walreplica"

// healthyLagFactor and healthyLagFloor define when Healthy turns false:
// pending changes not replicated for more than max(floor, factor*SyncInterval).
const (
	healthyLagFactor = 10
	healthyLagFloor  = 30 * time.Second
)

// DBStatus is the replication status of one database.
type DBStatus struct {
	Name       string `json:"name"` // "data" or "aux"
	Path       string `json:"path"`
	ReplicaURL string `json:"replicaUrl"`

	// LocalTXID is the last transaction captured from the WAL,
	// ReplicaTXID the last one uploaded to the replica.
	LocalTXID   uint64 `json:"localTxid"`
	ReplicaTXID uint64 `json:"replicaTxid"`

	// LastSync is the last successful sync (zero when none yet).
	LastSync *time.Time `json:"lastSync,omitempty"`

	// LagSeconds is 0 when the replica has every captured transaction,
	// otherwise the seconds since the last successful sync.
	LagSeconds float64 `json:"lagSeconds"`

	LastError   string     `json:"lastError,omitempty"`
	LastErrorAt *time.Time `json:"lastErrorAt,omitempty"`
}

// lag computes the lag used by DBStatus.
func lag(localTXID, replicaTXID uint64, lastSync, startedAt, now time.Time) time.Duration {
	if localTXID <= replicaTXID {
		return 0
	}
	ref := lastSync
	if ref.Before(startedAt) {
		ref = startedAt
	}
	return max(now.Sub(ref), 0)
}

func get(app kernel.App) *replicator {
	if v, ok := app.Store().Get(storeKey).(*replicator); ok {
		return v
	}
	return nil
}

func getBlocked(app kernel.App) *blocked {
	b, _ := app.Store().Get(blockedKey).(*blocked)
	return b
}

// Active reports whether replication is configured for app: running, or
// refused because another node holds the lease (then Healthy is false).
func Active(app kernel.App) bool { return get(app) != nil || getBlocked(app) != nil }

// Status returns the per database replication status.
// It returns nil when replication is not active.
func Status(app kernel.App) []DBStatus {
	r := get(app)
	if r == nil {
		if getBlocked(app) != nil {
			return []DBStatus{}
		}
		return nil
	}

	now := time.Now()
	out := make([]DBStatus, 0, len(r.dbs))
	for _, st := range r.dbs {
		s := DBStatus{Name: st.name, Path: st.path, ReplicaURL: st.url}

		if pos, err := st.db.Pos(); err == nil {
			s.LocalTXID = uint64(pos.TXID)
		}
		s.ReplicaTXID = uint64(st.replica.Pos().TXID)

		last := st.db.LastSuccessfulSyncAt()
		if !last.IsZero() {
			t := last
			s.LastSync = &t
		}
		s.LagSeconds = lag(s.LocalTXID, s.ReplicaTXID, last, r.startedAt, now).Seconds()

		st.mu.Lock()
		// an error is stale once a later sync succeeded
		if st.lastError != "" && !last.After(st.lastErrAt) {
			s.LastError = st.lastError
			t := st.lastErrAt
			s.LastErrorAt = &t
		}
		st.mu.Unlock()

		out = append(out, s)
	}
	return out
}

// Healthy reports whether replication is working. It is true when the module
// is inactive (nothing to be unhealthy about). When false, reason says why.
func Healthy(app kernel.App) (bool, string) {
	r := get(app)
	if r == nil {
		if b := getBlocked(app); b != nil {
			return false, b.reason
		}
		return true, ""
	}

	limit := max(healthyLagFloor, healthyLagFactor*r.cfg.SyncInterval)
	for _, s := range Status(app) {
		if s.LastError != "" {
			return false, fmt.Sprintf("%s: %s", s.Name, s.LastError)
		}
		if time.Duration(s.LagSeconds*float64(time.Second)) > limit {
			return false, fmt.Sprintf("%s: replica is %.0fs behind", s.Name, s.LagSeconds)
		}
	}
	return true, ""
}

// Snapshot forces a full snapshot of every replicated database now
// (after syncing pending changes). It requires active replication.
func Snapshot(ctx context.Context, app kernel.App) (map[string]uint64, error) {
	r := get(app)
	if r == nil {
		return nil, fmt.Errorf("walreplica: replication is not active (set %s)", EnvURL)
	}
	if err := r.syncNow(ctx); err != nil {
		return nil, err
	}

	out := map[string]uint64{}
	for _, st := range r.dbs {
		txid, err := snapshotDB(ctx, st)
		if err != nil {
			return out, fmt.Errorf("%s: %w", st.name, err)
		}
		out[st.name] = txid
	}
	return out, nil
}

// snapshotAttempts and snapshotRetryDelay bound the retries of snapshotDB.
const (
	snapshotAttempts   = 6
	snapshotRetryDelay = 100 * time.Millisecond
)

// snapshotDB writes a snapshot of one database. Litestream's own snapshot
// monitor (started by Store.Open, it also fires right after startup) may write
// the very same snapshot file at the same moment. The file replica client
// stages every upload in "<name>.tmp" and renames it, so two writers of the
// same snapshot (same level and TXID) clobber each other's staging file and
// the loser fails with ENOENT on rename. The data written by the winner is
// identical, so a failed attempt is retried after a short backoff (the retry
// either writes the file again or finds the concurrent writer done) and the
// call only fails when the error persists.
func snapshotDB(ctx context.Context, st *dbState) (uint64, error) {
	for attempt := 1; ; attempt++ {
		info, err := st.db.Snapshot(ctx)
		if err == nil {
			return uint64(info.MaxTXID), nil
		}
		if ctx.Err() != nil || !errors.Is(err, fs.ErrNotExist) || attempt == snapshotAttempts {
			return 0, err
		}
		select {
		case <-ctx.Done():
			return 0, err
		case <-time.After(time.Duration(attempt) * snapshotRetryDelay):
		}
	}
}

// ReplicaInfo summarizes the content of one database inside a replica.
type ReplicaInfo struct {
	Name string `json:"name"`
	URL  string `json:"url"`

	LatestTXID       uint64     `json:"latestTxid"`
	LatestAt         *time.Time `json:"latestAt,omitempty"` // creation time of the newest file
	Snapshots        int        `json:"snapshots"`
	LatestSnapshotAt *time.Time `json:"latestSnapshotAt,omitempty"`
	OldestRestoreAt  *time.Time `json:"oldestRestoreAt,omitempty"` // oldest snapshot
	OldestAt         *time.Time `json:"oldestAt,omitempty"` // creation time of the oldest file (oldest segment)
	Files            int        `json:"files"`
	Bytes            int64      `json:"bytes"`
	Levels           []LevelInfo `json:"levels"`
}

// LevelInfo is the size of one LTX level (9 = snapshots).
type LevelInfo struct {
	Level int   `json:"level"`
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`
}

// Inspect reads the replica at url (no running app needed) and summarizes
// each database: snapshots, newest transaction and sizes.
func Inspect(ctx context.Context, url string) ([]ReplicaInfo, error) {
	var out []ReplicaInfo
	for _, name := range []string{dataName, auxName} {
		sub, err := subURL(url, name)
		if err != nil {
			return nil, err
		}
		client, err := litestream.NewReplicaClientFromURL(sub)
		if err != nil {
			return nil, fmt.Errorf("walreplica: %w", err)
		}
		if err := client.Init(ctx); err != nil {
			return nil, fmt.Errorf("walreplica: %s: %w", name, err)
		}

		info := ReplicaInfo{Name: name, URL: redactURL(sub)}
		var newest, oldest time.Time

		for _, level := range []int{0, 1, 2, 3, litestream.SnapshotLevel} {
			itr, err := client.LTXFiles(ctx, level, 0, false)
			if err != nil {
				return nil, fmt.Errorf("walreplica: %s: list level %d: %w", name, level, err)
			}
			lv := LevelInfo{Level: level}
			for itr.Next() {
				f := itr.Item()
				info.Files++
				info.Bytes += f.Size
				lv.Files++
				lv.Bytes += f.Size
				if oldest.IsZero() || f.CreatedAt.Before(oldest) {
					oldest = f.CreatedAt
				}
				if uint64(f.MaxTXID) > info.LatestTXID {
					info.LatestTXID = uint64(f.MaxTXID)
				}
				if f.CreatedAt.After(newest) {
					newest = f.CreatedAt
				}
				if level == litestream.SnapshotLevel {
					info.Snapshots++
					t := f.CreatedAt
					if info.LatestSnapshotAt == nil || t.After(*info.LatestSnapshotAt) {
						info.LatestSnapshotAt = &t
					}
					if info.OldestRestoreAt == nil || t.Before(*info.OldestRestoreAt) {
						info.OldestRestoreAt = &t
					}
				}
			}
			err = itr.Err()
			_ = itr.Close()
			info.Levels = append(info.Levels, lv)
			if err != nil {
				return nil, fmt.Errorf("walreplica: %s: list level %d: %w", name, level, err)
			}
		}

		if !newest.IsZero() {
			info.LatestAt = &newest
			info.OldestAt = &oldest
		}
		out = append(out, info)
	}
	return out, nil
}
