//go:build no_kiosk

package kiosk

import "github.com/tokibase/tokibase/kernel"

func init() {
	kernel.RegisterModuleMarker("kiosk", []string{"_kiosk_devices"}, []string{"TOKI_KIOSK", "TOKI_KIOSK_ALLOW_REMOTE", "TOKI_KIOSK_SESSION_HOURS"}, true)
}
