//go:build !no_sync

package sync

import "github.com/tokibase/tokibase/kernel"

func init() {
	kernel.RegisterModule(markerOf(false))
}
