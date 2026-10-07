//go:build no_audit

package audit

import "github.com/tokibase/tokibase/kernel"

func init() {
	kernel.RegisterModuleMarker("audit", []string{"_audit"}, []string{"TOKI_AUDIT"}, true)
}
