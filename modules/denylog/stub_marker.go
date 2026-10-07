//go:build no_denylog

package denylog

import "github.com/tokibase/tokibase/kernel"

func init() {
	kernel.RegisterModuleMarker("denylog", nil, []string{"TOKI_DENYLOG"}, true)
}
