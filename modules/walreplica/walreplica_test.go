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
