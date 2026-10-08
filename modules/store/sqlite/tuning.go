package sqlite

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Environment knobs for the memory use of the SQLite connections.
//
// Every pooled connection owns a page cache and sorts/indexes in its own temp
// space, so the worst case memory is roughly
//
//	max open conns x (cache_size + temp space)
//
// The defaults keep that bounded on a small host; see docs/CAPACITY.md.
const (
	// EnvDBCacheKB is the page cache size of every connection in KiB (default 8192).
	EnvDBCacheKB = "TOKI_DB_CACHE_KB"
	// EnvDBTempStore selects where temporary tables and sort spill files live:
	// "memory" (default) or "file" (bounds the memory of huge sorts, needs a
	// writable temp dir: SQLITE_TMPDIR or TMPDIR, else /var/tmp, /usr/tmp, /tmp).
	// Measured: no difference in RSS or throughput for the FGR list queries.
	EnvDBTempStore = "TOKI_DB_TEMP_STORE"
	// EnvDBHeapMB sets the SQLite soft heap limit for the whole process in MiB
	// (0 = no limit, default 0): above it SQLite frees cache pages before allocating.
	// Measured: it did not lower the RSS of the read load, the pool size is the effective bound.
	EnvDBHeapMB = "TOKI_DB_HEAP_MB"
	// EnvDBMmapMB sets mmap_size of every connection in MiB (default 0 = off).
	EnvDBMmapMB = "TOKI_DB_MMAP_MB"
)

// DefaultCacheKB is the default per connection page cache.
const DefaultCacheKB = 8192

// Tuning holds the resolved memory related pragmas.
type Tuning struct {
	CacheKB   int
	TempStore string // "memory" or "file"
	HeapMB    int
	MmapMB    int
}

// TuningFromEnv resolves the tuning from the environment (invalid values fall back to the defaults).
func TuningFromEnv() Tuning {
	t := Tuning{CacheKB: DefaultCacheKB, TempStore: "memory"}

	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(EnvDBCacheKB))); err == nil && n > 0 {
		t.CacheKB = n
	}
	if strings.EqualFold(strings.TrimSpace(os.Getenv(EnvDBTempStore)), "file") {
		t.TempStore = "file"
	}
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(EnvDBHeapMB))); err == nil && n > 0 {
		t.HeapMB = n
	}
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(EnvDBMmapMB))); err == nil && n > 0 {
		t.MmapMB = n
	}

	return t
}

// Query returns the DSN query string with all connection pragmas.
func (t Tuning) Query() string {
	// Note: the busy_timeout pragma must be first because
	// the connection needs to be set to block on busy before WAL mode
	// is set in case it hasn't been already set by another connection.
	q := "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=journal_size_limit(200000000)" +
		"&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)"

	if t.TempStore == "file" {
		q += "&_pragma=temp_store(FILE)"
	} else {
		q += "&_pragma=temp_store(MEMORY)"
	}

	q += fmt.Sprintf("&_pragma=cache_size(-%d)", t.CacheKB)

	if t.HeapMB > 0 {
		q += fmt.Sprintf("&_pragma=soft_heap_limit(%d)", int64(t.HeapMB)*1024*1024)
	}
	if t.MmapMB > 0 {
		q += fmt.Sprintf("&_pragma=mmap_size(%d)", int64(t.MmapMB)*1024*1024)
	}

	return q + "&_defensive=1"
}
