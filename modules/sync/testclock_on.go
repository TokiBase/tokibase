//go:build !no_sync && synctest

package sync

import (
	"os"
	"strings"
	stdsync "sync"
	"time"
)

// The test clock is compiled only with the build tag `synctest`: a release
// binary has no way to shift its HLC wall time, whatever its environment says.

// Env of the test clock: with TOKI_SYNC_TEST=1 the wall clock of this process is
// shifted by TOKI_SYNC_TEST_CLOCK_OFFSET (a Go duration, may be negative or use
// the "d" suffix). The e2e tests use it to age nodes without waiting.
const (
	EnvTest            = "TOKI_SYNC_TEST"
	EnvTestClockOffset = "TOKI_SYNC_TEST_CLOCK_OFFSET"
	// EnvTestClockFile names a file holding the offset (same syntax); it is
	// re-read while the process runs, so a test driver can advance the clock of
	// several processes in lockstep (tests/e2e/parking).
	EnvTestClockFile = "TOKI_SYNC_TEST_CLOCK_FILE"
)

// fileClock returns a clock shifted by the duration in the file (re-read at
// most every 50 ms; an unreadable file means no shift).
func fileClock(path string) func() time.Time {
	var (
		mu   stdsync.Mutex
		last time.Time
		off  time.Duration
	)
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		if now := time.Now(); now.Sub(last) > 50*time.Millisecond {
			last = now
			if b, err := os.ReadFile(path); err == nil {
				s := strings.TrimSpace(string(b))
				if d, err := time.ParseDuration(s); err == nil {
					off = d
				} else if d, ok := parseDuration(s); ok {
					off = d
				}
			}
		}
		return time.Now().Add(off)
	}
}

func testNow() func() time.Time {
	if !envFlag(EnvTest) {
		return time.Now
	}
	if f := strings.TrimSpace(os.Getenv(EnvTestClockFile)); f != "" {
		return fileClock(f)
	}
	s := strings.TrimSpace(os.Getenv(EnvTestClockOffset))
	d, err := time.ParseDuration(s)
	if err != nil {
		var ok bool
		if d, ok = parseDuration(s); !ok {
			return time.Now
		}
	}
	return func() time.Time { return time.Now().Add(d) }
}
