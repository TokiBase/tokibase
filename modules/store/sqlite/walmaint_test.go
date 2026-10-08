package sqlite_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/store/sqlite"
)

// Overlapping readers keep the WAL from restarting: only the truncating
// checkpoint of MaintainWAL brings it back while they are still running.
func TestMaintainWALWithOverlappingReaders(t *testing.T) {
	t.Parallel()

	c := openTestConn(t, false)
	wm, ok := c.(kernel.WALMaintainer)
	if !ok {
		t.Fatal("the sqlite conn must implement kernel.WALMaintainer")
	}

	if _, err := c.Nonconcurrent().NewQuery("CREATE TABLE t (id INTEGER PRIMARY KEY, v BLOB)").Execute(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Nonconcurrent().NewQuery("INSERT INTO t (v) VALUES (zeroblob(2000))").Execute(); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(offset int) {
			defer wg.Done()
			time.Sleep(time.Duration(offset) * 15 * time.Millisecond)
			for {
				select {
				case <-stop:
					return
				default:
				}
				tx, err := c.Concurrent().Begin()
				if err != nil {
					return
				}
				var n int
				_ = tx.NewQuery("SELECT count(*) FROM t").Row(&n)
				time.Sleep(45 * time.Millisecond)
				_ = tx.Commit()
			}
		}(i)
	}
	defer func() { close(stop); wg.Wait() }()

	for i := 0; i < 3000; i++ {
		if _, err := c.Nonconcurrent().NewQuery("INSERT INTO t (v) VALUES (zeroblob(4000))").Execute(); err != nil {
			t.Fatal(err)
		}
	}

	before := wm.WALStatus().SizeBytes
	if before < 4*1024*1024 {
		t.Skipf("the WAL did not grow (%d bytes), nothing to prove", before)
	}

	if err := wm.MaintainWAL(context.Background(), 1024*1024); err != nil {
		t.Fatal(err)
	}

	st := wm.WALStatus()
	if st.Escalations != 1 || st.LastMode != "TRUNCATE" {
		t.Fatalf("expected one TRUNCATE escalation, got %+v", st)
	}
	if st.SizeBytes > 1024*1024 {
		t.Fatalf("expected the WAL under 1 MiB after the truncate, got %d (before %d)", st.SizeBytes, before)
	}

	// under the limit: only the PASSIVE round runs
	if err := wm.MaintainWAL(context.Background(), 1<<40); err != nil {
		t.Fatal(err)
	}
	if got := wm.WALStatus(); got.Escalations != 1 || got.Checkpoints < 2 {
		t.Fatalf("unexpected status %+v", got)
	}
}

func TestTuningFromEnv(t *testing.T) {
	t.Setenv("TOKI_DB_CACHE_KB", "4096")
	t.Setenv("TOKI_DB_TEMP_STORE", "memory")
	t.Setenv("TOKI_DB_HEAP_MB", "128")
	t.Setenv("TOKI_DB_MMAP_MB", "")

	tn := sqliteTuning()
	q := tn.Query()
	for _, want := range []string{"cache_size(-4096)", "temp_store(MEMORY)", "soft_heap_limit(134217728)"} {
		if !contains(q, want) {
			t.Fatalf("expected %q in %s", want, q)
		}
	}
	if contains(q, "mmap_size") {
		t.Fatalf("unexpected mmap_size in %s", q)
	}

	t.Setenv("TOKI_DB_CACHE_KB", "bad")
	t.Setenv("TOKI_DB_TEMP_STORE", "")
	t.Setenv("TOKI_DB_HEAP_MB", "")
	q = sqliteTuning().Query()
	if !contains(q, "cache_size(-8192)") || !contains(q, "temp_store(FILE)") || contains(q, "soft_heap_limit") {
		t.Fatalf("unexpected defaults %s", q)
	}
}

func sqliteTuning() sqlite.Tuning { return sqlite.TuningFromEnv() }

func contains(s, sub string) bool { return strings.Contains(s, sub) }
