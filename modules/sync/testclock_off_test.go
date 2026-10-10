//go:build !no_sync && !synctest

package sync

import (
	"testing"
	"time"
)

// A release build has no clock override: the variables are refused, not ignored.
func TestReleaseBuildRefusesTheTestClock(t *testing.T) {
	if testNow()().Sub(time.Now()) > time.Second {
		t.Fatal("the clock moved without a variable")
	}
	for _, k := range []string{EnvTest, EnvTestClockFile, EnvTestClockOffset} {
		t.Run(k, func(t *testing.T) {
			t.Setenv(k, "1")
			defer func() {
				if recover() == nil {
					t.Fatalf("%s set in a release build must refuse to start", k)
				}
			}()
			testNow()
		})
	}
}
