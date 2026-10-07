//go:build !no_fieldperm

package fieldperm

import "github.com/tokibase/tokibase/kernel"

func init() {
	kernel.RegisterModuleMarker("fieldperm", []string{"_field_rules"}, []string{"TOKI_FIELDPERM_SUPERUSER"}, false)
}
