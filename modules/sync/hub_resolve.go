//go:build !no_sync

package sync

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/sync/hlc"
	"github.com/tokibase/tokibase/modules/sync/proto"
)

// The glue between the hub apply pipeline and the pure resolver (resolve.go):
// it gathers the inputs of a concurrent change, runs the `hook` strategy
// through kernel.OnSyncConflictFor, and writes the `_sync_conflicts` rows.

var errNoHookHandler = errors.New("no handler answered the sync conflict (is the wasm module loaded and subscribed?)")

// maxConflictJSON bounds a stored patch / record snapshot; bigger ones are
// replaced by a summary (the full pushed patch stays in `_changes`).
const maxConflictJSON = 256 << 10

// redactValues replaces the value of every sensitive field with
// kernel.SensitiveMarker. It returns a copy.
func redactValues(colId string, m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	sens := map[string]bool{}
	for _, f := range kernel.SensitiveFieldsOf(colId) {
		sens[f] = true
	}
	for k, v := range m {
		base := strings.TrimRight(strings.TrimPrefix(k, "+"), "+-")
		if sens[base] {
			out[k] = kernel.SensitiveMarker
		} else {
			out[k] = v
		}
	}
	return out
}

func capJSON(m map[string]any) any {
	if m == nil {
		return map[string]any{}
	}
	b, err := json.Marshal(m)
	if err != nil || len(b) > maxConflictJSON {
		return map[string]any{"_truncated": true, "fields": sortedKeys(m)}
	}
	return m
}

// actorInfo resolves a change actor (a grant id, or "node") to the
// kind/id/collection a guest sees.
func actorInfo(app kernel.App, actor string) (kind, id, collection string) {
	if actor == "" || actor == ActorNode {
		return "system", "", ""
	}
	var g struct {
		Col string `db:"collection"`
		Rec string `db:"record"`
	}
	if err := app.DB().NewQuery("SELECT collection, record FROM _sync_actor_grants WHERE aid={:a}").
		Bind(dbx.Params{"a": actor}).One(&g); err != nil {
		return "auth", "", ""
	}
	kind, id, collection = "auth", g.Rec, g.Col
	if col, err := app.FindCachedCollectionByNameOrId(g.Col); err == nil {
		collection = col.Name
		if col.Name == core.CollectionNameSuperusers {
			kind = "superuser"
		}
	}
	return
}

// decide builds the resolver input of a concurrent change and runs it.
func (m *Module) decide(tx kernel.App, nodeID string, c *hubChange, col *core.Collection, p *policy,
	rec *core.Record, fields []core.Field, metaH int64, metaNode string) (Resolution, error) {
	cur, err := fieldValues(rec, fields, p.Types)
	if err != nil {
		return Resolution{}, err
	}
	in := ResolveInput{
		Strategy: p.Strategy, Review: p.Review, Concurrent: true,
		MetaHLC: hlc.HLC(metaH), MetaNode: metaNode, Base: c.base, HLC: c.hlc, Node: nodeID,
		Patch: c.patch, Types: p.Types, Current: cur,
	}
	if p.Strategy == StratFieldMerge || p.Strategy == StratHook {
		if in.Clocks, err = readFieldClocks(tx.NonconcurrentDB(), col.Id, c.Record); err != nil {
			return Resolution{}, err
		}
	}
	if p.Strategy == StratHook {
		in.Hook = func() (d HookDecision) {
			defer func() {
				if r := recover(); r != nil {
					d = HookDecision{Err: fmt.Errorf("hook panic: %v", r)}
				}
			}()
			hooks := kernel.OnSyncConflictFor(m.app)
			if hooks.Length() == 0 {
				return HookDecision{Err: errNoHookHandler}
			}
			kind, aid, acol := actorInfo(tx, c.Actor)
			clocks := make(map[string]uint64, len(in.Clocks))
			for f, h := range in.Clocks {
				clocks[f] = uint64(h)
			}
			ev := &kernel.SyncConflictEvent{
				App: tx, Collection: col, RecordID: c.Record, Hook: p.Hook,
				Current: redactValues(col.Id, cur), CurrentHLC: uint64(metaH), CurrentNode: metaNode,
				Incoming: kernel.SyncIncoming{Op: c.Op, Node: nodeID, HLC: uint64(c.hlc), BaseHLC: uint64(c.base),
					Patch: redactValues(col.Id, c.patch), ActorKind: kind, ActorID: aid, ActorCollection: acol},
				FieldClocks: clocks,
			}
			if err := hooks.Trigger(ev); err != nil {
				return HookDecision{Err: err}
			}
			if ev.Resolution == "" {
				return HookDecision{Err: errNoHookHandler}
			}
			patch := map[string]any{}
			for k, v := range ev.Patch {
				if s, ok := v.(string); ok && s == kernel.SensitiveMarker {
					continue // a redacted value echoed back is not a value
				}
				patch[k] = v
			}
			return HookDecision{Resolution: ev.Resolution, Patch: patch, Message: ev.Message}
		}
	}
	return Resolve(in), nil
}

// settle turns the verdicts of a concurrent change that refuse it into a
// *rejection (reject: reverted to the node; park: kept for an admin, nothing
// applied, no revert). For the other verdicts the conflict row, if any, is
// written now. A rejection carries its conflict row: the apply transaction
// rolls back, so the row is written with the rejection record (processGroup).
func (m *Module) settle(tx kernel.App, nodeID string, c *hubChange, col *core.Collection, d Resolution) error {
	switch d.Verdict {
	case VerdictReject:
		return &rejection{code: d.Code, msg: "rejected by the " + conflictStrategy(d) + " strategy", conflict: d.Conflict, change: c}
	case VerdictPark:
		return &rejection{code: d.Code, msg: "parked by the " + conflictStrategy(d) + " strategy", park: true, conflict: d.Conflict, change: c}
	}
	return m.writeConflict(tx, nodeID, c, col, d.Conflict)
}

func conflictStrategy(d Resolution) string {
	if d.Conflict != nil {
		return d.Conflict.Strategy
	}
	return "conflict"
}

func rawPatch(c *hubChange) string {
	if len(c.Patch) > 0 && string(c.Patch) != "null" {
		return string(c.Patch)
	}
	return "{}"
}

// writeConflict stores a `_sync_conflicts` row (nil ci = nothing to write).
// Values are redacted per kernel.RegisterSensitiveField and capped in size.
func (m *Module) writeConflict(tx kernel.App, nodeID string, c *hubChange, col *core.Collection, ci *ConflictInfo) error {
	if ci == nil {
		return nil
	}
	pc, err := tx.FindCachedCollectionByNameOrId(ConflictsCollection)
	if err != nil {
		return err
	}
	r := core.NewRecord(pc)
	r.Set("collection", col.Id)
	r.Set("record", c.Record)
	r.Set("change", c.ID)
	r.Set("node", nodeID)
	r.Set("actor", c.Actor)
	r.Set("kind", ci.Kind)
	r.Set("strategy", ci.Strategy)
	r.Set("incoming", capJSON(redactValues(col.Id, ci.Incoming)))
	r.Set("current", capJSON(redactValues(col.Id, ci.Current)))
	r.Set("resolution", ci.Resolution)
	r.Set("status", ci.Status)
	if ci.Status == ConflictResolved {
		r.Set("resolved_by", "auto")
		r.Set("resolved_at", m.created())
	}
	note := ci.Note
	if len(note) > 2000 {
		note = note[:2000]
	}
	r.Set("note", note)
	return tx.SaveNoValidate(r)
}

// writeRejectionConflict stores the conflict row of a rejected change; it runs
// in the transaction that records the rejection (the apply one rolled back).
func (m *Module) writeRejectionConflict(tx kernel.App, nodeID string, rj *rejection) error {
	if rj.conflict == nil || rj.change == nil {
		return nil
	}
	col, err := tx.FindCachedCollectionByNameOrId(rj.change.Collection)
	if err != nil {
		return nil
	}
	return m.writeConflict(tx, nodeID, rj.change, col, rj.conflict)
}
