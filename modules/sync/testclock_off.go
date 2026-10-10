//go:build !no_sync && !synctest

package sync

import (
	"os"
	"time"
)

// Env of the test clock (only effective in a binary built with `-tags synctest`).
const (
	EnvTest            = "TOKI_SYNC_TEST"
	EnvTestClockOffset = "TOKI_SYNC_TEST_CLOCK_OFFSET"
	EnvTestClockFile   = "TOKI_SYNC_TEST_CLOCK_FILE"
)

// testNow is the real clock in a release build. A test clock request through
// the environment is refused loudly instead of silently ignored, so nobody
// believes a time-shifted test ran on a binary that cannot shift time.
func testNow() func() time.Time {
	if os.Getenv(EnvTest) != "" || os.Getenv(EnvTestClockFile) != "" || os.Getenv(EnvTestClockOffset) != "" {
		panic("sync: TOKI_SYNC_TEST* is set but this binary was built without -tags synctest; refusing to start")
	}
	return time.Now
}
