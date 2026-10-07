//go:build !no_walreplica

package walreplica

import "github.com/tokibase/tokibase/kernel"

func init() {
	kernel.RegisterModuleMarker("walreplica", nil, []string{"TOKI_REPLICA_URL"}, false)
}
