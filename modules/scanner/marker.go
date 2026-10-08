//go:build !no_scanner

package scanner

import "github.com/tokibase/tokibase/kernel"

func init() {
	kernel.RegisterModuleMarker("scanner", []string{"_scanners", "_scan_events"}, []string{"TOKI_SCANNER", "TOKI_SCAN_RETENTION_HOURS", "TOKI_SCAN_TOPIC_AUTH"}, false)
}
