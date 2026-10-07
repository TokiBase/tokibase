//go:build no_webhooks

package webhooks

import "github.com/tokibase/tokibase/kernel"

func init() {
	kernel.RegisterModuleMarker("webhooks", []string{"_webhooks", "_webhook_deliveries"}, []string{"TOKI_WEBHOOKS"}, true)
}
