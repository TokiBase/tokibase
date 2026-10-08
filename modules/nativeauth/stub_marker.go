//go:build no_nativeauth

package nativeauth

import "github.com/tokibase/tokibase/kernel"

func init() {
	kernel.RegisterModuleMarker("nativeauth", nil, []string{"TOKI_NATIVEAUTH_GOOGLE_AUDIENCES", "TOKI_NATIVEAUTH_APPLE_AUDIENCES"}, true)
}
