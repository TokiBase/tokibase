//go:build no_push

package push

import "github.com/tokibase/tokibase/kernel"

func init() {
	kernel.RegisterModuleMarker("push", []string{"_push_devices", "_push_topics", "_push_subscriptions"}, []string{"TOKI_PUSH"}, true)
}
