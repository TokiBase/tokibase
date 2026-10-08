package sync

import "github.com/tokibase/tokibase/kernel"

// markerCollections are the tables and system collections the module owns
// (docs/SYNC_DESIGN.md §8.5).
var markerCollections = []string{
	"_changes", "_sync_meta", "_sync_tombstones", "_sync_nodes", "_sync_cursors",
	"_sync_policies", "_sync_conflicts", "_sync_reservations", "_sync_sequences",
	"_sync_reserved", "_sync_actor_grants", "_sync_actors", "_sync_schema", "_sync_state",
}

func markerOf(stubbed bool) kernel.ModuleMarker {
	return kernel.ModuleMarker{
		Name:        "sync",
		Collections: markerCollections,
		Envs:        []string{"TOKI_SYNC_ROLE", "TOKI_SYNC_HUB_URL"},
		Files:       []string{"sync_node.key"},
		Stubbed:     stubbed,
	}
}
