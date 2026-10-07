//go:build !no_adminlock

package adminlock

import "github.com/tokibase/tokibase/kernel"

func init() {
	kernel.RegisterModule(kernel.ModuleMarker{Name: "adminlock", Collections: nil, Envs: []string{"TOKI_ADMIN_UI"}, Stubbed: false, OffIsActive: true})
}
