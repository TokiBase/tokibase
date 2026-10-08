//go:build !no_sync

package sync

import "github.com/tokibase/tokibase/core"

// syncSystemAllow lists the system collections (names starting with "_") that
// a `_sync_policies` row may sync, because another module needs their rows on
// every node. It is an explicit allowlist, never a pattern. Each entry has
// rules null (superusers only) and is pulled WITHOUT the view-rule check of
// the service actor (there is no rule to evaluate); the pull stays limited by
// the policy direction.
//
//   - `_device_certs`: the certificate deny list of modules/devicecert. Add on
//     the hub: `toki sync policies set _device_certs --direction pull`.
var syncSystemAllow = map[string]bool{
	"_device_certs": true,
}

// systemAllowed reports whether col is an allowlisted system collection.
func systemAllowed(col *core.Collection) bool {
	return col != nil && col.System && syncSystemAllow[col.Name]
}
