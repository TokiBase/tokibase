//go:build !no_sync

package sync

import (
	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
)

// Sent tracking (docs/SYNC_DESIGN.md §3.5, review P56-3). With pull_view_rule on
// and a restrictive view rule, whether a record is visible to a node can change
// at any time and a deleted record cannot be checked at all. Evict and delete
// rows would therefore reveal id, origin node and HLC of records the node never
// had. The hub remembers what it delivered in `_sync_sent` and sends evict /
// delete rows only for those records.
//
// Nodes that already pulled before the table existed are "legacy": they keep the
// old behaviour (everything is considered sent) until they are re-bootstrapped.

const stateSentInit = "sent_init"

// sentTracked reports whether evict/delete rows of the collection depend on
// the sent set.
func (m *Module) sentTracked(vw *viewer, col *core.Collection, p *policy) bool {
	return p != nil && m.pullRuleOn(vw, p) && col.ViewRule != nil && *col.ViewRule != ""
}

// initSentLegacy marks the nodes that pulled before sent tracking existed. It
// runs once (first pull on the hub).
func (m *Module) initSentLegacy() {
	db := m.app.NonconcurrentDB()
	var n int
	if err := db.NewQuery("SELECT COUNT(*) FROM _sync_state WHERE key={:k}").Bind(dbx.Params{"k": stateSentInit}).Row(&n); err != nil || n > 0 {
		return
	}
	_, err := db.NewQuery("INSERT OR IGNORE INTO _sync_state (key, value) SELECT 'sent_legacy:' || id, '1' FROM " + NodesCollection + " WHERE COALESCE(pulled_seq,0) > 0").Execute()
	if err != nil {
		m.app.Logger().Warn("sync: sent tracking init failed", "error", err)
		return
	}
	_, _ = db.NewQuery("INSERT OR IGNORE INTO _sync_state (key, value) VALUES ({:k}, '1')").Bind(dbx.Params{"k": stateSentInit}).Execute()
}

// wasSent reports whether the node may know the record.
func (m *Module) wasSent(nodeID, colID, recID string) bool {
	db := m.app.DB()
	var n int
	if err := db.NewQuery("SELECT COUNT(*) FROM _sync_state WHERE key={:k}").Bind(dbx.Params{"k": "sent_legacy:" + nodeID}).Row(&n); err != nil || n > 0 {
		return true // legacy node (or unreadable state: fail towards delivering)
	}
	if err := db.NewQuery("SELECT COUNT(*) FROM _sync_sent WHERE node={:n} AND collection={:c} AND record={:r}").
		Bind(dbx.Params{"n": nodeID, "c": colID, "r": recID}).Row(&n); err != nil {
		return true
	}
	return n > 0
}

// markSent records that a visible row of the record was delivered. A row that
// is lost in transit is harmless: the node pulls it again and a stale entry
// only allows an evict/delete row for a record the node could have had.
func (m *Module) markSent(nodeID, colID, recID string) {
	if _, err := m.app.NonconcurrentDB().NewQuery("INSERT OR IGNORE INTO _sync_sent (node, collection, record) VALUES ({:n},{:c},{:r})").
		Bind(dbx.Params{"n": nodeID, "c": colID, "r": recID}).Execute(); err != nil {
		m.app.Logger().Warn("sync: sent tracking write failed", "node", nodeID, "error", err)
	}
}

// viewScoped reports whether a restrictive view rule can make a node hold a
// subset of the collection (the digest of such a node can not equal the hub's).
// pull_view_rule is on by default, so it only counts together with a rule.
func (m *Module) viewScoped(col *core.Collection, p *policy) bool {
	on := p.PullViewRule || (envFlag(EnvPullViewRule) && !p.SkipViewRule)
	return on && col.ViewRule != nil && *col.ViewRule != ""
}
