//go:build no_geo

package geo

import "github.com/tokibase/tokibase/kernel"

func init() {
	// geo owns only derived _geo_* R*Tree tables (rebuildable, no guard to lose), so there is nothing to check.
	kernel.RegisterModuleMarker("geo", nil, nil, true)
}
