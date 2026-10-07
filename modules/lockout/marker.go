//go:build !no_lockout

package lockout

import "github.com/tokibase/tokibase/kernel"

func init() {
	kernel.RegisterModuleMarker("lockout", []string{"_lockout"}, []string{"TOKI_LOCKOUT", "TOKI_LOCKOUT_THRESHOLD"}, false)
}
