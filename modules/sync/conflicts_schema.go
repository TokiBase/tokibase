//go:build !no_sync

package sync

import "github.com/tokibase/tokibase/core"

// ConflictsCollection is the hub's system collection of changes that need a
// decision (docs/SYNC_DESIGN.md §2.7). PR4 wrote `actor_revoked`
// (parked) and `apply_error` rows; PR5 adds the conflict strategies and the
// admin commands and extends this schema.
const ConflictsCollection = "_sync_conflicts"

// Conflict kinds.
var conflictKinds = []string{
	"concurrent_field", "rule_denied", "validation_failed", "unique_violation", "tombstoned", "actor_revoked",
	"hook_failed", "hub_wins", "schema_dropped_field", "reservation_out_of_range", "orphaned", "apply_error",
}

// Conflict resolutions and statuses.
var (
	conflictResolutions = []string{"auto_lww", "auto_merge", "reverted", "parked", "accepted", "rejected"}
	conflictStatuses    = []string{"open", "resolved"}
)

// EnsureConflictsCollection creates `_sync_conflicts` (superusers only). It is idempotent.
func EnsureConflictsCollection(app core.App) error {
	if c, _ := app.FindCollectionByNameOrId(ConflictsCollection); c != nil {
		return nil
	}
	c := core.NewBaseCollection(ConflictsCollection)
	c.System = true
	c.Fields.Add(
		&core.TextField{Name: "collection", Max: 255},
		&core.TextField{Name: "record", Max: 255},
		&core.TextField{Name: "change", Max: 255},
		&core.TextField{Name: "node", Max: 255},
		&core.TextField{Name: "actor", Max: 255},
		&core.SelectField{Name: "kind", MaxSelect: 1, Values: conflictKinds},
		&core.TextField{Name: "strategy", Max: 64},
		&core.JSONField{Name: "incoming", MaxSize: 4 << 20},
		&core.JSONField{Name: "current", MaxSize: 4 << 20},
		&core.SelectField{Name: "resolution", MaxSelect: 1, Values: conflictResolutions},
		&core.SelectField{Name: "status", MaxSelect: 1, Values: conflictStatuses},
		&core.TextField{Name: "resolved_by", Max: 255},
		&core.DateField{Name: "resolved_at"},
		&core.TextField{Name: "note", Max: 2000},
		&core.AutodateField{Name: "created", OnCreate: true},
	)
	c.AddIndex("idx_sync_conflicts_status", false, "[[status]], [[created]]", "")
	c.AddIndex("idx_sync_conflicts_record", false, "[[collection]], [[record]]", "")
	return app.Save(c)
}
