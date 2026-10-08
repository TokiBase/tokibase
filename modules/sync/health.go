//go:build !no_sync

package sync

import (
	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
)

// moduleStoreKey keeps the module in app.Store() so that the CLI and health
// code can find it from the app alone.
const moduleStoreKey = "__tokiSyncModule__"

// moduleOf returns the registered module of app (nil when the role is off).
func moduleOf(app core.App) *Module {
	m, _ := app.Store().Get(moduleStoreKey).(*Module)
	return m
}

// HealthBlock is the `sync` block of GET /api/health (superusers only).
type HealthBlock struct {
	Role string `json:"role"`
	// Pending is the number of local changes not yet acknowledged by the hub
	// (spoke), or the number of hub changes the slowest active node has not pulled (hub).
	Pending    int64 `json:"pending"`
	LowWater   int64 `json:"low_water"`
	Head       int64 `json:"head"`
	StaleNodes int64 `json:"stale_nodes"`
	// ActiveNodes and OpenConflicts are hub only.
	ActiveNodes   int64 `json:"active_nodes,omitempty"`
	OpenConflicts int64 `json:"open_conflicts,omitempty"`
}

// Health computes the health block.
func (m *Module) Health() *HealthBlock {
	if !m.ready.Load() {
		return nil
	}
	db := m.app.DB()
	b := &HealthBlock{Role: string(m.role), LowWater: m.lowWater(), Head: m.headSeq()}
	if m.role == RoleHub {
		_ = db.NewQuery("SELECT COUNT(*) FROM " + NodesCollection + " WHERE status='stale'").Row(&b.StaleNodes)
		_ = db.NewQuery("SELECT COUNT(*) FROM " + NodesCollection + " WHERE status='active'").Row(&b.ActiveNodes)
		if b.ActiveNodes > 0 {
			var minPulled int64
			if err := db.NewQuery("SELECT COALESCE(MIN(COALESCE(pulled_seq,0)),0) FROM " + NodesCollection + " WHERE status='active'").Row(&minPulled); err == nil {
				b.Pending = max(b.Head-minPulled, 0)
			}
		}
		if m.app.HasTable(ConflictsCollection) {
			_ = db.NewQuery("SELECT COUNT(*) FROM " + ConflictsCollection + " WHERE status='open'").Row(&b.OpenConflicts)
		}
		return b
	}
	_ = db.NewQuery("SELECT COUNT(*) FROM _changes WHERE status IN ('local','pushed')").Row(&b.Pending)
	return b
}

func (m *Module) bindHealth() {
	apis.SetHealthExtra(m.app, "sync", func(core.App) any {
		if b := m.Health(); b != nil {
			return b
		}
		return nil
	})
}
