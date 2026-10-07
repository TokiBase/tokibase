//go:build no_timelint

package timelint

import "github.com/tokibase/tokibase/kernel"

func init() {
	kernel.RegisterModuleMarker("timelint", nil, []string{"TOKI_TIMELINT"}, true)
}
