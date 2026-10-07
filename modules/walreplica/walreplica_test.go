//go:build !no_replica

package walreplica_test

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	_ "github.com/tokibase/tokibase/migrations"
	"github.com/tokibase/tokibase/modules/store/sqlite"
	"github.com/tokibase/tokibase/modules/walreplica"
)

func newApp(t *testing.T, dir string, cfg *walreplica.Config) core.App {
	t.Helper()
	app := core.NewBaseApp(core.BaseAppConfig{DataDir: dir, EncryptionEnv: "pb_test_env"})
	if cfg != nil {
		walreplica.RegisterWithConfig(app, *cfg)
	} else {
		walreplica.Register(app)
	}
	if err := app.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	if err := app.RunAllMigrations(); err != nil {
		t.Fatal(err)
	}
	return app
}

func stop(t *testing.T, app core.App) {
	t.Helper()
	if err := app.ResetBootstrapState(); err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func fileURL(dir string) string { return (&url.URL{Scheme: "file", Path: dir}).String() }

func TestInactiveWithoutEnv(t *testing.T) {
	t.Setenv(walreplica.EnvURL, "")
	app := newApp(t, t.TempDir(), nil)
	defer stop(t, app)

	if walreplica.Active(app) || walreplica.Status(app) != nil {
		t.Fatal("module must be inactive without TOKI_REPLICA_URL")
	}
	if ok, reason := walreplica.Healthy(app); !ok || reason != "" {
		t.Fatalf("inactive must be healthy, got %v %q", ok, reason)
	}
	if app.Store().Has(kernel.StoreKeyDisableCheckpoint) {
		t.Fatal("checkpoint must stay enabled when inactive")
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv(walreplica.EnvURL, "")
	cfg, err := walreplica.FromEnv()
	if err != nil || cfg.Enabled() || cfg.SyncInterval != time.Second || cfg.Retention != 24*time.Hour || cfg.SnapshotInterval != time.Hour {
		t.Fatalf("defaults: %+v %v", cfg, err)
	}

	t.Setenv(walreplica.EnvURL, "s3://b/p")
	t.Setenv(walreplica.EnvSyncInterval, "250ms")
	t.Setenv(walreplica.EnvRetention, "48h")
	t.Setenv(walreplica.EnvSnapshotInterval, "30m")
	cfg, err = walreplica.FromEnv()
	if err != nil || !cfg.Enabled() || cfg.SyncInterval != 250*time.Millisecond || cfg.Retention != 48*time.Hour || cfg.SnapshotInterval != 30*time.Minute {
		t.Fatalf("custom: %+v %v", cfg, err)
	}

	t.Setenv(walreplica.EnvSyncInterval, "soon")
	if _, err := walreplica.FromEnv(); err == nil || !strings.Contains(err.Error(), walreplica.EnvSyncInterval) {
		t.Fatalf("expected invalid duration error, got %v", err)
	}
}

func TestReplicateAndRestore(t *testing.T) {
	dataDir := t.TempDir()
	replicaDir := t.TempDir()

	app := newApp(t, dataDir, &walreplica.Config{URL: fileURL(replicaDir), SyncInterval: 100 * time.Millisecond})
	if !walreplica.Active(app) {
		t.Fatal("expected active replication")
	}
	if !app.Store().Has(kernel.StoreKeyDisableCheckpoint) {
		t.Fatal("app checkpoint must be disabled while replicating")
	}

	col := core.NewBaseCollection("notes")
	col.Fields.Add(&core.TextField{Name: "title"})
	if err := app.Save(col); err != nil {
		t.Fatal(err)
	}
	const n = 25
	for i := 0; i < n; i++ {
		r := core.NewRecord(col)
		r.Set("title", fmt.Sprintf("note %d", i))
		if err := app.Save(r); err != nil {
			t.Fatal(err)
		}
	}

	// wait until the replica caught up with everything written
	waitFor(t, 15*time.Second, "replica to catch up", func() bool {
		st := walreplica.Status(app)
		if len(st) != 2 {
			return false
		}
		for _, s := range st {
			if s.LocalTXID == 0 || s.ReplicaTXID < s.LocalTXID || s.LastSync == nil {
				return false
			}
		}
		return true
	})

	for _, s := range walreplica.Status(app) {
		if s.LagSeconds != 0 || s.LastError != "" || !strings.HasPrefix(s.ReplicaURL, "file://") {
			t.Fatalf("unexpected status %+v", s)
		}
	}
	if ok, reason := walreplica.Healthy(app); !ok {
		t.Fatalf("expected healthy, got %q", reason)
	}

	// a forced snapshot makes the replica restorable regardless of the snapshot interval
	if _, err := walreplica.Snapshot(context.Background(), app); err != nil {
		t.Fatal(err)
	}

	infos, err := walreplica.Inspect(context.Background(), fileURL(replicaDir))
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 || infos[0].Snapshots < 1 || infos[0].LatestTXID == 0 {
		t.Fatalf("unexpected inspect result %+v", infos)
	}

	// clean stop: final sync, then close after the app released its connections
	stop(t, app)
	if walreplica.Active(app) {
		t.Fatal("replication must be stopped after ClearBootstrap")
	}
	if app.Store().Has(kernel.StoreKeyDisableCheckpoint) {
		t.Fatal("checkpoint disable flag must be cleared on stop")
	}

	restored := t.TempDir()
	if err := walreplica.Restore(context.Background(), fileURL(replicaDir), restored, walreplica.RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	// refuses to clobber
	if err := walreplica.Restore(context.Background(), fileURL(replicaDir), restored, walreplica.RestoreOptions{}); err == nil {
		t.Fatal("expected refusal when data.db exists")
	}
	if err := walreplica.Restore(context.Background(), fileURL(replicaDir), restored, walreplica.RestoreOptions{Overwrite: true}); err != nil {
		t.Fatalf("overwrite restore: %v", err)
	}

	db, err := sqlite.DefaultConnect(filepath.Join(restored, "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.NewQuery("SELECT COUNT(*) FROM notes").Row(&count); err != nil {
		t.Fatal(err)
	}
	if count != n {
		t.Fatalf("restored %d records, want %d", count, n)
	}
	if _, err := os.Stat(filepath.Join(restored, "auxiliary.db")); err != nil {
		t.Fatalf("auxiliary.db not restored: %v", err)
	}

	// the restored directory boots as a normal app with the data intact
	app2 := newApp(t, restored, nil)
	defer stop(t, app2)
	recs, err := app2.FindAllRecords("notes")
	if err != nil || len(recs) != n {
		t.Fatalf("app2 records: %d %v", len(recs), err)
	}
}

func TestLagAndHealth(t *testing.T) {
	// an unreachable replica (a file where a directory is needed) shows up as lag and unhealthy
	dataDir := t.TempDir()
	replicaDir := t.TempDir()

	app := newApp(t, dataDir, &walreplica.Config{URL: fileURL(replicaDir), SyncInterval: 100 * time.Millisecond})
	defer stop(t, app)

	col := core.NewBaseCollection("things")
	col.Fields.Add(&core.TextField{Name: "title"})
	if err := app.Save(col); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 15*time.Second, "initial sync", func() bool {
		s := walreplica.Status(app)
		return len(s) == 2 && s[0].LocalTXID > 0 && s[0].ReplicaTXID >= s[0].LocalTXID
	})

	// break the replica target: remove the dir and put a file in its place
	if err := os.RemoveAll(filepath.Join(replicaDir, "data")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(replicaDir, "data"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		r := core.NewRecord(col)
		r.Set("title", "x")
		if err := app.Save(r); err != nil {
			t.Fatal(err)
		}
	}

	waitFor(t, 15*time.Second, "lag to show", func() bool {
		for _, s := range walreplica.Status(app) {
			if s.Name == "data" && s.LagSeconds > 0 && s.ReplicaTXID < s.LocalTXID {
				return true
			}
		}
		return false
	})
	waitFor(t, 15*time.Second, "error to be recorded", func() bool {
		for _, s := range walreplica.Status(app) {
			if s.Name == "data" && s.LastError != "" {
				return true
			}
		}
		return false
	})
	if ok, reason := walreplica.Healthy(app); ok || !strings.HasPrefix(reason, "data:") {
		t.Fatalf("expected unhealthy for data, got %v %q", ok, reason)
	}

	// fix the target: replication recovers on its own and the error clears
	if err := os.Remove(filepath.Join(replicaDir, "data")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 15*time.Second, "recovery", func() bool {
		ok, _ := walreplica.Healthy(app)
		for _, s := range walreplica.Status(app) {
			if s.Name == "data" && (s.LagSeconds != 0 || s.ReplicaTXID < s.LocalTXID) {
				return false
			}
		}
		return ok
	})
}

// seedReplica replicates a small app into replicaDir, snapshots and stops it.
func seedReplica(t *testing.T, replicaDir string) {
	t.Helper()
	app := newApp(t, t.TempDir(), &walreplica.Config{URL: fileURL(replicaDir), SyncInterval: 100 * time.Millisecond})
	col := core.NewBaseCollection("notes")
	col.Fields.Add(&core.TextField{Name: "title"})
	if err := app.Save(col); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		r := core.NewRecord(col)
		r.Set("title", "n")
		if err := app.Save(r); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := walreplica.Snapshot(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	stop(t, app)
}

func TestPromote(t *testing.T) {
	replicaDir := t.TempDir()
	seedReplica(t, replicaDir)

	var audited *walreplica.PromoteResult
	walreplica.SetAuditSink(func(r walreplica.PromoteResult) { audited = &r })
	defer walreplica.SetAuditSink(nil)

	dir := filepath.Join(t.TempDir(), "standby")
	res, err := walreplica.Promote(context.Background(), fileURL(replicaDir), dir, walreplica.PromoteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Collections == 0 || res.MovedTo != "" || audited == nil {
		t.Fatalf("unexpected result %+v audited=%v", res, audited)
	}
	raw, err := os.ReadFile(filepath.Join(dir, walreplica.PromotedMarker))
	if err != nil {
		t.Fatalf("marker not written: %v", err)
	}
	for _, key := range []string{`"from_url"`, `"restored_txid"`, `"promoted_at"`, `"hostname"`} {
		if !strings.Contains(string(raw), key) {
			t.Fatalf("marker lacks %s: %s", key, raw)
		}
	}

	// non-empty dir is refused and left untouched
	if _, err := walreplica.Promote(context.Background(), fileURL(replicaDir), dir, walreplica.PromoteOptions{}); err == nil {
		t.Fatal("expected refusal for a non-empty dir")
	}
	if _, err := os.Stat(filepath.Join(dir, "data.db")); err != nil {
		t.Fatalf("refused promote must not touch the dir: %v", err)
	}

	// --force moves aside instead of deleting
	res, err = walreplica.Promote(context.Background(), fileURL(replicaDir), dir, walreplica.PromoteOptions{Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.MovedTo == "" || !strings.Contains(res.MovedTo, ".pre-promote-") {
		t.Fatalf("expected moved dir, got %q", res.MovedTo)
	}
	if _, err := os.Stat(filepath.Join(res.MovedTo, "data.db")); err != nil {
		t.Fatalf("old data.db must survive in %s: %v", res.MovedTo, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "data.db")); err != nil {
		t.Fatal(err)
	}

	// empty replica: nothing to promote
	if _, err := walreplica.Promote(context.Background(), fileURL(t.TempDir()), filepath.Join(t.TempDir(), "x"), walreplica.PromoteOptions{}); err == nil {
		t.Fatal("expected error for an empty replica")
	}
}

func TestLeaseBlocksSecondReplicator(t *testing.T) {
	t.Setenv(walreplica.EnvTakeover, "")
	replicaDir := t.TempDir()
	cfg := &walreplica.Config{URL: fileURL(replicaDir), SyncInterval: 100 * time.Millisecond}

	a := newApp(t, t.TempDir(), cfg)
	if !walreplica.Active(a) {
		t.Fatal("first replicator must start")
	}
	li := walreplica.LeaseInfo(a)
	if li == nil || !li.Held {
		t.Fatalf("expected held lease, got %+v", li)
	}
	if _, err := os.Stat(filepath.Join(replicaDir, ".toki-lease.json")); err != nil {
		t.Fatalf("lease object missing: %v", err)
	}

	// second node (different pb_data => different node id) is refused
	b := newApp(t, t.TempDir(), cfg)
	if ok, reason := walreplica.Healthy(b); ok || !strings.HasPrefix(reason, "lease held by ") {
		t.Fatalf("expected unhealthy lease reason, got %v %q", ok, reason)
	}
	if li := walreplica.LeaseInfo(b); li == nil || li.Held || li.NodeID != walreplica.LeaseInfo(a).NodeID {
		t.Fatalf("blocked node must show the foreign lease, got %+v", li)
	}
	if st := walreplica.Status(b); len(st) != 0 {
		t.Fatalf("blocked node must not replicate: %+v", st)
	}
	stop(t, b)

	// takeover override
	t.Setenv(walreplica.EnvTakeover, "1")
	c := newApp(t, t.TempDir(), cfg)
	if ok, reason := walreplica.Healthy(c); !ok {
		t.Fatalf("takeover must start replication: %q", reason)
	}
	stop(t, c)
	stop(t, a)
	t.Setenv(walreplica.EnvTakeover, "")

	// same node (same pb_data) restarting is never blocked
	dir := t.TempDir()
	d := newApp(t, dir, cfg)
	stop(t, d)
	e := newApp(t, dir, cfg)
	if ok, reason := walreplica.Healthy(e); !ok {
		t.Fatalf("same node must restart: %q", reason)
	}
	stop(t, e)
}

func TestLeaseStaleIsIgnored(t *testing.T) {
	t.Setenv(walreplica.EnvTakeover, "")
	replicaDir := t.TempDir()
	old := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	lease := `{"node_id":"dead","hostname":"old","pid":1,"started_at":"` + old + `","heartbeat_at":"` + old + `"}`
	if err := os.WriteFile(filepath.Join(replicaDir, ".toki-lease.json"), []byte(lease), 0o644); err != nil {
		t.Fatal(err)
	}
	app := newApp(t, t.TempDir(), &walreplica.Config{URL: fileURL(replicaDir), SyncInterval: 100 * time.Millisecond})
	defer stop(t, app)
	if ok, reason := walreplica.Healthy(app); !ok || walreplica.LeaseInfo(app) == nil || !walreplica.LeaseInfo(app).Held {
		t.Fatalf("stale lease must not block: %v %q", ok, reason)
	}
}
