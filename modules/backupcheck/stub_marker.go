//go:build no_backupcheck

package backupcheck

import "github.com/tokibase/tokibase/kernel"

func init() {
	kernel.RegisterModuleMarker("backupcheck", nil, []string{"TOKI_BACKUP_VERIFY"}, true)
}
