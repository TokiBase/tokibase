//go:build !no_sync

package client

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/sync/proto"
)

// The spoke keeps a LOCAL, informational copy of what happened to its changes
// in `_sync_conflicts` (the same system collection the hub uses): the hub's
// answer to a push that was not a plain "applied", local edits that a revert
// or a delete discarded, and pulled changes that could not be applied. The
// hub's own table stays the source of truth; nothing here is synced, and
// `toki sync conflicts` on a spoke lists these rows. A parked change shows as
// open until the next pull of the record (the hub's resolution is not mirrored).

const (
	conflictsCollection = "_sync_conflicts"
	maxLocalConflicts   = 500 // resolved rows kept
	maxConflictJSON     = 256 << 10
)

// conflictRow is a local conflict row to write.
type conflictRow struct {
	Collection, Record, Change, Node, Actor string
	Kind, Strategy, Resolution, Status      string
	Incoming                                any
	Note                                    string
}

// writeConflictRow inserts the row unless an identical (change, kind, status)
// one exists, then prunes old resolved rows.
func writeConflictRow(tx kernel.App, r conflictRow) {
	col, err := tx.FindCachedCollectionByNameOrId(conflictsCollection)
	if err != nil || col == nil {
		return // the collection exists once the module initialized
	}
	db := tx.NonconcurrentDB()
	if r.Change != "" {
		var n int
		if db.NewQuery("SELECT COUNT(*) FROM "+conflictsCollection+" WHERE change={:c} AND kind={:k} AND status={:s}").
			Bind(dbx.Params{"c": r.Change, "k": r.Kind, "s": r.Status}).Row(&n) == nil && n > 0 {
			return
		}
	}
	rec := core.NewRecord(col)
	rec.Set("collection", r.Collection)
	rec.Set("record", r.Record)
	rec.Set("change", r.Change)
	rec.Set("node", r.Node)
	rec.Set("actor", r.Actor)
	rec.Set("kind", r.Kind)
	if r.Strategy != "" {
		rec.Set("strategy", r.Strategy)
	}
	rec.Set("resolution", r.Resolution)
	rec.Set("status", r.Status)
	inc := r.Incoming
	if b, err := json.Marshal(inc); err != nil || len(b) > maxConflictJSON {
		inc = map[string]any{"_truncated": true}
	}
	rec.Set("incoming", inc)
	if len(r.Note) > 2000 {
		r.Note = r.Note[:2000]
	}
	rec.Set("note", r.Note)
	if r.Status == "resolved" {
		rec.Set("resolved_by", "hub")
		rec.Set("resolved_at", time.Now().UTC().Format(dateLayout))
	}
	if err := tx.SaveNoValidate(rec); err != nil {
		return
	}
	_, _ = db.NewQuery(`DELETE FROM ` + conflictsCollection + ` WHERE status='resolved' AND id NOT IN
  (SELECT id FROM ` + conflictsCollection + ` WHERE status='resolved' ORDER BY created DESC, id DESC LIMIT {:n})`).
		Bind(dbx.Params{"n": maxLocalConflicts}).Execute()
}

// conflictKindOf maps a hub result code to a `kind` of §2.7.
func conflictKindOf(code string) string {
	switch {
	case code == "", code == proto.CodeSuperseded, code == proto.CodeHookRejected, code == "hook_parked":
		return "concurrent_field"
	case code == proto.CodeLegalTombstone:
		return "tombstoned"
	case strings.HasPrefix(code, "actor_"):
		return "actor_revoked"
	}
	for _, k := range []string{"rule_denied", "validation_failed", "unique_violation", "tombstoned", "hook_failed",
		"hub_wins", "orphaned", "schema_dropped_field", "reservation_out_of_range"} {
		if code == k {
			return k
		}
	}
	return "concurrent_field"
}

// recordOutcome keeps the hub's answer to a pushed change that was not a plain
// "applied" (merged, superseded, rejected, parked).
func (c *Client) recordOutcome(row outRow, r proto.PushResult) {
	if c.o.App == nil {
		return
	}
	status := r.Status
	if status == proto.ResDuplicate {
		status = r.Was
	}
	cr := conflictRow{Collection: row.Coll, Record: row.Record, Change: r.ID, Node: c.nodeID, Actor: row.Actor,
		Kind: conflictKindOf(r.Code), Status: "resolved", Note: "hub answer: " + status}
	if r.Code != "" {
		cr.Note += " (" + r.Code + ")"
	}
	var patch map[string]any
	_ = json.Unmarshal([]byte(row.Patch), &patch)
	cr.Incoming = patch
	switch status {
	case proto.ResSuperseded:
		cr.Resolution = "auto_lww"
	case proto.ResMerged:
		cr.Resolution = "auto_merge"
	case proto.ResParked:
		cr.Resolution, cr.Status = "parked", "open"
	case proto.ResRejected:
		cr.Resolution = "rejected"
		if r.Code == proto.CodeHubWins {
			cr.Resolution = "reverted"
		}
	default:
		return
	}
	_ = c.o.App.RunInTransaction(func(tx kernel.App) error {
		writeConflictRow(tx, cr)
		return nil
	})
}

// applyFailureRow records a pulled change that could not be applied.
func applyFailureRow(tx kernel.App, ch *proto.PullChange, err error) {
	writeConflictRow(tx, conflictRow{Collection: ch.Collection, Record: ch.Record, Change: ch.ID, Node: ch.Node,
		Kind: "apply_error", Resolution: "parked", Status: "open", Note: "pull apply failed: " + err.Error()})
}
