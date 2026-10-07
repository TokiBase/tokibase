//go:build no_totp

package totp

import "github.com/tokibase/tokibase/kernel"

func init() {
	kernel.RegisterModuleMarker("totp", []string{"_totp"}, []string{"TOKI_TOTP_KEY", "TOKI_TOTP_REQUIRED_ROLES", "TOKI_TOTP_REQUIRE_SUPERUSERS"}, true)
}
