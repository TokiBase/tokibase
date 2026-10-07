//go:build !no_replica

package walreplica

import "github.com/tokibase/tokibase/kernel"

func init() {
	kernel.RegisterModule(kernel.ModuleMarker{Name: "walreplica", Tag: "no_replica", Envs: []string{"TOKI_REPLICA_URL"}, Stubbed: false})
}
