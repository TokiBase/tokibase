//go:build no_roles

package roles

import "github.com/tokibase/tokibase/kernel"

func init() {
	kernel.RegisterModuleMarker("roles", []string{"_roles", "_memberships"}, nil, true)
}
