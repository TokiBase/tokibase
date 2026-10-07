//go:build !no_replica

package walreplica

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/benbjohnson/litestream"
	"github.com/benbjohnson/litestream/file"
	"github.com/tokibase/tokibase/kernel"
)

const (
	dataDBFile = "data.db"
	auxDBFile  = "auxiliary.db"

	shutdownSyncTimeout = 15 * time.Second
)

// dbState is the runtime state of one replicated database.
type dbState struct {
	name string // "data" or "aux"
	path string
	url  string // replica URL of this database (with credentials stripped by redactURL for display)

	db      *litestream.DB
	replica *litestream.Replica

	mu        sync.Mutex
	lastError string
	lastErrAt time.Time
}

func (s *dbState) recordError(msg string, at time.Time) {
	s.mu.Lock()
	s.lastError = msg
	s.lastErrAt = at
	s.mu.Unlock()
}

// replicator owns the litestream store of one app.
type replicator struct {
	cfg       Config
	store     *litestream.Store
	dbs       []*dbState
	startedAt time.Time
	lease     *leaseManager
}

func newReplicator(app kernel.App, cfg Config) (*replicator, error) {
	targets := []struct{ name, file string }{
		{dataName, dataDBFile},
		{auxName, auxDBFile},
	}

	appHandler := app.Logger().Handler()
	lim := &limiter{}

	r := &replicator{cfg: cfg, startedAt: time.Now()}
	var lsDBs []*litestream.DB

	for _, t := range targets {
		sub, err := subURL(cfg.URL, t.name)
		if err != nil {
			return nil, err
		}

		client, err := litestream.NewReplicaClientFromURL(sub)
		if err != nil {
			return nil, fmt.Errorf("walreplica: %w", err)
		}

		path := filepath.Join(app.DataDir(), t.file)
		db := litestream.NewDB(path)
		replica := litestream.NewReplicaWithClient(db, client)
		replica.SyncInterval = cfg.SyncInterval
		db.Replica = replica
		if fc, ok := client.(*file.ReplicaClient); ok {
			fc.Replica = replica
		}

		st := &dbState{name: t.name, path: path, url: redactURL(sub), db: db, replica: replica}
		r.dbs = append(r.dbs, st)
		lsDBs = append(lsDBs, db)
	}

	store := litestream.NewStore(lsDBs, litestream.DefaultCompactionLevels)
	store.SnapshotInterval = cfg.SnapshotInterval
	store.SnapshotRetention = cfg.Retention
	store.Logger = slog.New(&captureHandler{app: appHandler, lim: lim}).With("system", "walreplica")
	for _, st := range r.dbs {
		st := st
		h := &captureHandler{app: appHandler, lim: lim, onError: st.recordError}
		st.db.SetLogger(slog.New(h).With("system", "walreplica", "replica", st.name))
	}
	r.store = store

	return r, nil
}

func (r *replicator) open(ctx context.Context) error {
	return r.store.Open(ctx)
}

// syncNow uploads everything written so far (local WAL to LTX, LTX to replica).
func (r *replicator) syncNow(ctx context.Context) error {
	var firstErr error
	for _, st := range r.dbs {
		if err := st.db.SyncAndWait(ctx); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("%s: %w", st.name, err)
		}
	}
	return firstErr
}

// close syncs one last time and stops replication.
// The app must have closed its own database connections first.
func (r *replicator) close() error {
	if r.lease != nil {
		r.lease.release()
	}
	ctx, cancel := context.WithTimeout(context.Background(), shutdownSyncTimeout)
	defer cancel()
	return r.store.Close(ctx)
}
