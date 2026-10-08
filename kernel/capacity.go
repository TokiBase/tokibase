package kernel

import (
	"os"
	"runtime"
	"strconv"
	"strings"
)

const (
	// EnvDBMaxConns overrides the size of the data.db read pool.
	EnvDBMaxConns = "TOKI_DB_MAX_CONNS"

	// minAutoDataMaxOpenConns is the floor of the automatic pool size. It is
	// above the number of connections a single request can hold at once so
	// that nested queries cannot exhaust the pool by themselves.
	minAutoDataMaxOpenConns = 32
)

// envPositiveInt returns the value of the env var or 0 if unset or invalid.
func envPositiveInt(name string) int {
	n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	if err != nil || n <= 0 {
		return 0
	}

	return n
}

// defaultDataMaxOpenConns resolves the size of the data.db read pool when the
// app config does not set one: TOKI_DB_MAX_CONNS, else 4 connections per
// CPU between 32 and [DefaultDataMaxOpenConns].
//
// Every connection owns a page cache and its sort buffers, and the queries are
// CPU bound, so a pool of 120 on a 12 core host only adds memory.
func defaultDataMaxOpenConns() int {
	if n := envPositiveInt(EnvDBMaxConns); n > 0 {
		return n
	}

	n := runtime.NumCPU() * 4
	if n < minAutoDataMaxOpenConns {
		n = minAutoDataMaxOpenConns
	}
	if n > DefaultDataMaxOpenConns {
		n = DefaultDataMaxOpenConns
	}

	return n
}

// envNonNegativeInt is like [envPositiveInt] but accepts 0 and reports whether a valid value was set.
func envNonNegativeInt(name string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	if err != nil || n < 0 {
		return 0, false
	}

	return n, true
}
