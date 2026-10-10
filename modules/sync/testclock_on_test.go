//go:build !no_sync && synctest

package sync

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTestClockFileShiftsTime(t *testing.T) {
	f := filepath.Join(t.TempDir(), "clock")
	if err := os.WriteFile(f, []byte("48h"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvTest, "1")
	t.Setenv(EnvTestClockFile, f)
	if d := testNow()().Sub(time.Now()); d < 47*time.Hour || d > 49*time.Hour {
		t.Fatalf("shift %v", d)
	}
}
