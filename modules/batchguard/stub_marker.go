//go:build no_batchguard

package batchguard

import "github.com/tokibase/tokibase/kernel"

func init() {
	kernel.RegisterModuleMarker("batchguard", []string{"_batch_rules"}, nil, true)
}
