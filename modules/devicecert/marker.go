//go:build !no_devicecert

package devicecert

import "github.com/tokibase/tokibase/kernel"

func init() {
	kernel.RegisterModule(markerOf(false))
}
