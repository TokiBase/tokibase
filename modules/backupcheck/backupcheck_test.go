package backupcheck_test

import (
	"archive/zip"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/backupcheck"
	"github.com/tokibase/tokibase/tests"
)

func newApp(t *testing.T) *tests.TestApp {
	t.Helper()
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)
	return app
}

func backupPath(app kernel.App, name string) string {
	return filepath.Join(app.DataDir(), kernel.LocalBackupsDirName, name)
}

func TestVerifyOK(t *testing.T) {
	app := newApp(t)
	ctx := context.Background()

	if err := app.CreateBackup(ctx, "ok.zip"); err != nil {
		t.Fatal(err)
	}

	r, err := backupcheck.Verify(ctx, app, "ok.zip")
	if err != nil {
		t.Fatal(err, r)
	}
	if !r.IntegrityOK || !r.QuickCheckOK {
		t.Fatalf("expected integrity ok: %+v", r)
	}
	if r.Collections == 0 || r.Collections != r.LiveCollections || !r.IsLatest {
		t.Fatalf("collections mismatch: %+v", r)
	}
	if r.Records == 0 || r.SizeBytes == 0 || r.SampledRecords == 0 {
		t.Fatalf("unexpected counts: %+v", r)
	}
	if !r.OK() {
		t.Fatalf("expected OK: %+v", r)
	}
}

// rebuildCorrupted copies src zip to dst, overwriting the middle of data.db with garbage.
func rebuildCorrupted(t *testing.T, src, dst string) {
	t.Helper()
	zr, err := zip.OpenReader(src)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	out, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	zw := zip.NewWriter(out)
	for _, f := range zr.File {
		w, err := zw.Create(f.Name)
		if err != nil {
			t.Fatal(err)
		}
		if f.FileInfo().IsDir() {
			continue
		}
		rc, _ := f.Open()
		raw, _ := io.ReadAll(rc)
		rc.Close()
		if f.Name == "data.db" {
			for i := len(raw) / 4; i < len(raw)*3/4; i++ {
				raw[i] = byte(i*31 + 7)
			}
		}
		if _, err := w.Write(raw); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyCorrupted(t *testing.T) {
	app := newApp(t)
	ctx := context.Background()

	if err := app.CreateBackup(ctx, "good.zip"); err != nil {
		t.Fatal(err)
	}
	rebuildCorrupted(t, backupPath(app, "good.zip"), backupPath(app, "bad.zip"))

	r, _ := backupcheck.Verify(ctx, app, "bad.zip")
	if r.IntegrityOK {
		t.Fatalf("expected IntegrityOK=false: %+v", r)
	}
	if r.Error == "" || r.OK() {
		t.Fatalf("expected Error set and not OK: %+v", r)
	}
}

func TestVerifyMissingBackup(t *testing.T) {
	app := newApp(t)
	r, err := backupcheck.Verify(context.Background(), app, "nope.zip")
	if err == nil || r.Error == "" {
		t.Fatalf("expected error, got %+v", r)
	}
}

func TestLatest(t *testing.T) {
	app := newApp(t)
	ctx := context.Background()

	for _, n := range []string{"a.zip", "b.zip", "c.zip"} {
		if err := app.CreateBackup(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	base := time.Now().Add(-time.Hour)
	// make b the newest
	for n, d := range map[string]time.Duration{"a.zip": 0, "c.zip": time.Minute, "b.zip": 2 * time.Minute} {
		if err := os.Chtimes(backupPath(app, n), base.Add(d), base.Add(d)); err != nil {
			t.Fatal(err)
		}
	}

	got, err := backupcheck.Latest(ctx, app)
	if err != nil || got != "b.zip" {
		t.Fatalf("latest = %q, %v", got, err)
	}
}

func TestHook(t *testing.T) {
	t.Run("on", func(t *testing.T) {
		t.Setenv(backupcheck.EnvVar, "")
		app := newApp(t)
		calls := 0
		backupcheck.OnResult = func(_ kernel.App, r backupcheck.Report) {
			calls++
			if !r.OK() {
				t.Errorf("hook report not ok: %+v", r)
			}
		}
		defer func() { backupcheck.OnResult = nil }()

		backupcheck.Register(app)
		if err := app.CreateBackup(context.Background(), "hook.zip"); err != nil {
			t.Fatal(err)
		}
		backupcheck.Wait()
		if calls != 1 {
			t.Fatalf("expected 1 verification, got %d", calls)
		}
	})

	t.Run("off", func(t *testing.T) {
		t.Setenv(backupcheck.EnvVar, "off")
		app := newApp(t)
		calls := 0
		backupcheck.OnResult = func(kernel.App, backupcheck.Report) { calls++ }
		defer func() { backupcheck.OnResult = nil }()

		backupcheck.Register(app)
		if err := app.CreateBackup(context.Background(), "hook.zip"); err != nil {
			t.Fatal(err)
		}
		backupcheck.Wait()
		if calls != 0 {
			t.Fatalf("expected no verification, got %d", calls)
		}
	})
}
