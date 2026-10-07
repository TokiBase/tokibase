//go:build !no_tlscheck

package tlscheck

import "github.com/tokibase/tokibase/kernel"

func init() {
	kernel.RegisterModuleMarker("tlscheck", nil, []string{"TOKI_TLS_CHECK"}, false)
}
