//go:build !no_ruleguard

package ruleguard

import "github.com/tokibase/tokibase/kernel"

func init() {
	kernel.RegisterModule(kernel.ModuleMarker{Name: "ruleguard", Files: []string{"ruleguard.json"}, Stubbed: false})
}
