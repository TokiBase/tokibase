//go:build no_computed

package computed

import "github.com/tokibase/tokibase/kernel"

func init() {
	kernel.RegisterModuleMarker("computed", []string{"_computed_fields"}, []string{"TOKI_COMPUTED_ALLOW_MANUAL", "TOKI_COMPUTED_DRIFT_CRON"}, true)
}
