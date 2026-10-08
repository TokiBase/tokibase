//go:build !no_sync

package sync

import (
	stdsync "sync"

	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/sync/hlc"
	"github.com/tokibase/tokibase/modules/sync/proto"
)

// Audit actions emitted by the module.
const (
	AuditNodeEnroll      = "sync.node.enroll"
	AuditNodeRevoke      = "sync.node.revoke"
	AuditHandshakeFailed = "sync.handshake.failed"
	AuditApply           = "sync.apply"
	AuditReject          = "sync.reject"
	AuditActorGrant      = "sync.actor.grant"
	AuditActorRevoke     = "sync.actor.revoke"
	// AuditConflictResolve is a resolved conflict (--take hub|incoming|patch);
	// AuditCompact is one compaction run that deleted something or marked nodes stale.
	AuditConflictResolve = "sync.conflict.resolve"
	AuditCompact         = "sync.compact"
)

var (
	auditMu   stdsync.RWMutex
	auditSink func(action, collection, record string, details map[string]any)
)

// SetAuditSink connects sync events to an external audit log. Modules must not
// import each other, so the wiring happens in tokibase.go (same pattern as
// mcp.SetAuditSink). Details set `cli: true` for actions of the command line.
func SetAuditSink(fn func(action, collection, record string, details map[string]any)) {
	auditMu.Lock()
	auditSink = fn
	auditMu.Unlock()
}

func emit(action, collection, record string, details map[string]any) {
	auditMu.RLock()
	fn := auditSink
	auditMu.RUnlock()
	if fn != nil {
		fn(action, collection, record, details)
	}
}

// auditDetails builds the details of a hub apply audit entry: actor_* is the
// ORIGINAL actor and request is {path, ip, node, change, hlc}.
func (m *Module) auditDetails(nodeID, ip string, c *hubChange, act *actorCtx, extra map[string]any) map[string]any {
	d := map[string]any{
		"op": c.Op, "collection": c.Collection,
		"request": map[string]any{"path": "/api/sync/push", "ip": ip, "node": nodeID, "change": c.ID, "hlc": c.hlc.String()},
	}
	if act != nil && act.rec != nil {
		d["actor_kind"] = act.kind()
		d["actor_id"] = act.rec.Id
		d["actor_collection"] = act.rec.Collection().Name
		d["service_actor"] = act.service
	} else if c.Actor != "" {
		d["actor_grant"] = c.Actor
		// a rejection has no resolved actor: name the user of the grant anyway
		if g, err := loadGrant(m.app.DB(), c.Actor); err == nil && g != nil {
			d["actor_id"] = g.Record
			if col, err := m.app.FindCachedCollectionByNameOrId(g.Collection); err == nil && col != nil {
				d["actor_collection"] = col.Name
			}
		}
	}
	for k, v := range extra {
		d[k] = v
	}
	return d
}

// auditApplied emits sync.apply for changes of superuser and service actors
// (or of everyone with TOKI_SYNC_AUDIT_ALL=1: it is a high volume entry).
func (m *Module) auditApplied(nodeID, ip string, c *hubChange, o *outcome) {
	if o.status != proto.ResApplied && o.status != proto.ResMerged {
		return
	}
	if o.actor == nil || !(o.actor.service || o.actor.kind() == kernel.AuthKindSuperuser || envFlag(EnvAuditAll)) {
		return
	}
	emit(AuditApply, c.Collection, c.Record, m.auditDetails(nodeID, ip, c, o.actor, map[string]any{"status": o.status}))
}

var _ = hlc.HLC(0)
