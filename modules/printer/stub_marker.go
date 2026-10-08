//go:build no_printer

package printer

import "github.com/tokibase/tokibase/kernel"

func init() {
	kernel.RegisterModuleMarker("printer", []string{"_printers", "_print_templates", "_print_jobs"},
		[]string{"TOKI_PRINTER", "TOKI_PRINT_AUTH", "TOKI_PRINT_ALLOW_CIDRS", "TOKI_PRINT_RETENTION_DAYS", "TOKI_PRINT_MAX_BYTES"}, true)
}
