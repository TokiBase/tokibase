//go:build !no_totp

package totp

import "github.com/tokibase/tokibase/kernel"

func init() {
	kernel.RegisterModuleMarker("totp", []string{CollectionName}, []string{EnvKey, EnvRequiredRoles, EnvRequireSuper}, false)
}
