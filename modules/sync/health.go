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
	// SchemaVersion is the schema version of this node (hub: latest bundle).
	SchemaVersion int64 `json:"schema_version"`
	// MaxSkewMs and NodeSkew are hub only: the largest clock skew seen from any
	// node and from each node (docs/SYNC_DESIGN.md §7.4).
	MaxSkewMs int64      `json:"max_skew_ms,omitempty"`
	NodeSkew  []NodeSkew `json:"node_skew,omitempty"`
}

// NodeSkew is the clock metric of one node.
type NodeSkew struct {
	Node          string `json:"node"`
	Name          string `json:"name"`
	ClockOffsetMs int64  `json:"clock_offset_ms"`
	MaxSkewMs     int64  `json:"max_skew_ms"`
}

// Health computes the health block.
func (m *Module) Health() *HealthBlock {
	if !m.ready.Load() {
		return nil
	}
	db := m.app.DB()
	b := &HealthBlock{Role: string(m.role), LowWater: m.lowWater(), Head: m.headSeq(), SchemaVersion: m.schemaVersion()}
	if m.role == RoleHub {
		b.NodeSkew = m.nodeSkew()
		for _, n := range b.NodeSkew {
			b.MaxSkewMs = max(b.MaxSkewMs, n.MaxSkewMs)
		}
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

// nodeSkew lists the clock metrics of the nodes that ever showed a skew (at most 50).
func (m *Module) nodeSkew() []NodeSkew {
	var rows []struct {
		ID   string  `db:"id"`
		Name string  `db:"name"`
		Off  float64 `db:"offs"`
		Max  float64 `db:"mx"`
	}
	if err := m.app.DB().NewQuery("SELECT id, name, COALESCE(clock_offset_ms,0) AS offs, COALESCE(max_skew_ms,0) AS mx FROM " + NodesCollection +
		" WHERE status!='revoked' AND status!='pending' ORDER BY mx DESC LIMIT 50").All(&rows); err != nil {
		return nil
	}
	out := make([]NodeSkew, 0, len(rows))
	for _, r := range rows {
		out = append(out, NodeSkew{Node: r.ID, Name: r.Name, ClockOffsetMs: int64(r.Off), MaxSkewMs: int64(r.Max)})
	}
	return out
}
