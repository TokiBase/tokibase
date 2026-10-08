//go:build !no_nativeauth

package nativeauth

import "github.com/tokibase/tokibase/kernel"

func init() {
	kernel.RegisterModuleMarker("nativeauth", nil, []string{EnvSwitch, EnvGoogleAudiences, EnvAppleAudiences}, false)
}
