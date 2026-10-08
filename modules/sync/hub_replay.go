//go:build !no_sync

package sync

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/pocketbase/dbx"
	validation "github.com/pocketbase/ozzo-validation/v4"
	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/sync/hlc"
	"github.com/tokibase/tokibase/modules/sync/proto"
	"github.com/tokibase/tokibase/tools/router"
)

// The hub replay (docs/SYNC_DESIGN.md §4.1, §5): the effective patch of every
// change becomes a record API request that runs as the ORIGINAL ACTOR through
// apis.ReplayRecordRequests, so collection rules, fieldperm, validation and
// batchguard (tx groups) apply exactly as for a client.

type gkey struct{ col, rec string }

type gmeta struct {
	h    int64
	node string
	has  bool
}

// groupState is the view of the hub as the earlier changes of the same tx
// group leave it (they are replayed together, after all were prepared).
type groupState struct {
	exists map[gkey]bool
	meta   map[gkey]gmeta
	tomb   map[gkey]string
}

func newGroupState() *groupState {
	return &groupState{exists: map[gkey]bool{}, meta: map[gkey]gmeta{}, tomb: map[gkey]string{}}
}

// prepared is one change ready to be finished after the replay.
type prepared struct {
	c      *hubChange
	col    *core.Collection
	pol    *policy
	fields []core.Field
	key    gkey

	skip       bool // lost lww: nothing replayed
	skipRevert bool
	req        *core.InternalRequest
	applied    map[string]any // patch after resolution (typed ops kept)
	wantAuto   map[string]string
	isNew      bool
	merged     bool
	nh         int64
	nn         string
}

// applyFault is a test seam: a non-nil error aborts the apply of a group like an
// infrastructure failure would. It is nil in production.
var applyFault func(c *hubChange) error

// applyGroup applies a tx group (or one change) inside tx. A refusal comes back
// as *rejection.
func (m *Module) applyGroup(tx kernel.App, nodeID, ip string, group []*hubChange) ([]*outcome, error) {
	if f := applyFault; f != nil {
		for _, c := range group {
			if err := f(c); err != nil {
				return nil, err
			}
		}
	}
	// ---- the actor of the group (a hook-written change of a user request has
	// actor "node" but belongs to the user's request: the group runs as the user)
	groupAID := ""
	for _, c := range group {
		a := c.Actor
		if a == "" || a == ActorNode {
			continue
		}
		if groupAID != "" && groupAID != a {
			return nil, reject(proto.CodeActorUnknown, "a tx group must have a single actor")
		}
		groupAID = a
	}
	hs := make([]hlc.HLC, len(group))
	for i, c := range group {
		hs[i] = c.hlc
	}
	var grantHLCs []hlc.HLC
	if groupAID != "" {
		for i, c := range group {
			if c.Actor == groupAID {
				grantHLCs = append(grantHLCs, hs[i])
			}
		}
	}
	actor, rj := m.resolveActor(tx, nodeID, groupAID, grantHLCs)
	if rj != nil {
		if rj.internal {
			return nil, errors.New(rj.msg)
		}
		return nil, rj
	}

	gs := newGroupState()
	preps := make([]*prepared, 0, len(group))
	for _, c := range group {
		p, err := m.prepare(tx, nodeID, c, gs)
		if err != nil {
			return nil, err
		}
		preps = append(preps, p)
	}

	// state before the group, per record, and the last change per record
	db := tx.NonconcurrentDB()
	pre := map[gkey]map[string]any{}
	existedBefore := map[gkey]bool{}
	last := map[gkey]int{}
	var reqs []*core.InternalRequest
	fixAuto := map[gkey]map[string]string{}
	origin := &kernel.SyncOrigin{Mode: kernel.SyncModePush, Node: nodeID, HLC: uint64(group[len(group)-1].hlc), ChangeID: group[0].ID, Actor: groupAID, Fields: map[string]any{}}
	for i, p := range preps {
		last[p.key] = i
		if p.req == nil {
			continue
		}
		reqs = append(reqs, p.req)
		if _, seen := existedBefore[p.key]; !seen {
			rec, _ := tx.FindRecordById(p.col.Id, p.key.rec)
			existedBefore[p.key] = rec != nil
			if rec != nil {
				v, err := fieldValues(rec, p.fields, p.pol.Types)
				if err != nil {
					return nil, err
				}
				pre[p.key] = v
			}
		}
		for name, v := range p.wantAuto {
			origin.Fields[OriginFieldKey(p.col.Id, p.key.rec, name)] = v
			if fixAuto[p.key] == nil {
				fixAuto[p.key] = map[string]string{}
			}
			fixAuto[p.key][name] = v
		}
	}

	if len(reqs) > 0 {
		ctx := kernel.WithSyncOrigin(context.Background(), origin)
		_, err := apis.ReplayRecordRequestsFrom(ctx, core.AsApp(tx), actor.rec, ip, map[string]string{proto.HeaderSyncNode: nodeID}, reqs)
		if err != nil {
			return nil, classify(err)
		}
	}
	for k, want := range fixAuto {
		col, _ := tx.FindCachedCollectionByNameOrId(k.col)
		if col == nil {
			continue
		}
		if rec, _ := tx.FindRecordById(col.Id, k.rec); rec == nil {
			continue
		}
		if err := fixAutodates(tx, col, k.rec, want); err != nil {
			return nil, err
		}
	}

	// ---- finish: hub rows, record clocks, tombstones
	outs := make([]*outcome, 0, len(preps))
	for i, p := range preps {
		o, err := m.finish(tx, db, nodeID, p, i == last[p.key], pre[p.key], existedBefore[p.key], actor.rec)
		if err != nil {
			return nil, err
		}
		o.actor = actor
		outs = append(outs, o)
	}
	return outs, nil
}

// prepare runs the envelope checks and the lww decision of one change and
// builds its request.
func (m *Module) prepare(tx kernel.App, nodeID string, c *hubChange, gs *groupState) (*prepared, error) {
	col, err := tx.FindCachedCollectionByNameOrId(c.Collection)
	if err != nil {
		return nil, reject(proto.CodePolicyDirection, "unknown collection")
	}
	p, perr := m.pol.For(col)
	if perr != nil {
		return nil, perr
	}
	if p == nil || (p.Direction != DirBoth && p.Direction != DirPush) {
		return nil, reject(proto.CodePolicyDirection, "the collection does not accept pushes")
	}
	if _, err := m.Clock().ObserveBounded(c.hlc, maxDrift()); err != nil {
		return nil, reject(proto.CodeFutureHLC, "the change hlc is too far in the future")
	}
	db := tx.NonconcurrentDB()
	key := gkey{col.Id, c.Record}
	pr := &prepared{c: c, col: col, pol: p, key: key, fields: syncedFields(col, p), wantAuto: map[string]string{}}

	tomb, ok := gs.tomb[key]
	if !ok {
		tomb = tombstoneKind(db, col.Id, c.Record)
	}
	meta, ok := gs.meta[key]
	if !ok {
		h, n, has := readMeta(db, col.Id, c.Record)
		meta = gmeta{h, n, has}
	}
	exists, ok := gs.exists[key]
	var rec *core.Record
	if !ok {
		rec, _ = tx.FindRecordById(col.Id, c.Record)
		exists = rec != nil
	}

	base := "/api/collections/" + col.Id + "/records"
	switch c.Op {
	case OpPurge:
		return nil, reject(proto.CodeLegalTombstone, "purge is not accepted from nodes")
	case OpDelete:
		if !exists {
			pr.skip = true
			return pr, nil
		}
		// deletes are final (§4.2): a delete older than a concurrent update still deletes
		pr.req = &core.InternalRequest{Method: http.MethodDelete, URL: base + "/" + c.Record}
		gs.exists[key], gs.tomb[key] = false, "delete"
		gs.meta[key] = gmeta{}
		return pr, nil
	}

	if tomb == "legal" {
		return nil, reject(proto.CodeLegalTombstone, "the record was purged")
	}
	if tomb != "" {
		return nil, reject(proto.CodeTombstoned, "the record was deleted")
	}
	if !exists && c.Op == OpUpdate {
		return nil, reject(proto.CodeOrphaned, "the record does not exist on the hub")
	}

	allowed := make(map[string]core.Field, len(pr.fields))
	for _, f := range pr.fields {
		allowed[f.GetName()] = f
	}
	patch := c.patch
	if exists && int64(c.base) != meta.h {
		// concurrent: the writer did not see the latest hub version (lww, record level)
		if hlc.Less(hlc.HLC(meta.h), meta.node, c.hlc, nodeID) {
			// the incoming change wins; fields not in the patch keep the hub values
		} else {
			// lost: nothing but counter/set operations is applied (they never conflict, §4.5)
			typedOnly := map[string]any{}
			for k, v := range patch {
				if _, ok := typedOp(v); ok {
					typedOnly[k] = v
				}
			}
			if len(typedOnly) == 0 {
				pr.skip, pr.skipRevert = true, true
				return pr, nil
			}
			patch, pr.merged = typedOnly, true
		}
	}

	pr.isNew = !exists
	if !pr.isNew && rec != nil {
		for _, f := range pr.fields {
			if f.Type() == kernel.FieldTypeAutodate {
				pr.wantAuto[f.GetName()] = rec.GetString(f.GetName())
			}
		}
	}
	body := map[string]any{}
	pr.applied = map[string]any{}
	for name, v := range patch {
		f, ok := allowed[name]
		if !ok {
			continue // unknown, excluded or never-synced field (file, password, tokenKey, derived)
		}
		pr.applied[name] = v
		if op, typed := typedOp(v); typed {
			if d, ok := op["$inc"]; ok {
				body[name+"+"] = d
			}
			if add, ok := op["$add"]; ok {
				body[name+"+"] = add
			}
			if rm, ok := op["$rm"]; ok {
				body[name+"-"] = rm
			}
			continue
		}
		if f.Type() == kernel.FieldTypeAutodate {
			if s, ok := v.(string); ok {
				pr.wantAuto[name] = s
			}
			continue
		}
		body[name] = v
	}
	if pr.isNew {
		body["id"] = c.Record
		pr.req = &core.InternalRequest{Method: http.MethodPost, URL: base, Body: body}
	} else {
		pr.req = &core.InternalRequest{Method: http.MethodPatch, URL: base + "/" + c.Record, Body: body}
	}
	// record clock: max(meta.hlc, change.hlc); a lost-lww merge keeps the winner's clock
	pr.nh, pr.nn = int64(c.hlc), nodeID
	if meta.has && (pr.merged || !hlc.Less(hlc.HLC(meta.h), meta.node, c.hlc, nodeID)) {
		pr.nh, pr.nn = meta.h, meta.node
	}
	gs.exists[key] = true
	gs.meta[key] = gmeta{pr.nh, pr.nn, true}
	return pr, nil
}

// finish stores the hub row of a replayed change (and the clock / tombstone).
func (m *Module) finish(tx kernel.App, db dbx.Builder, nodeID string, p *prepared, isLast bool, pre map[string]any, existedBefore bool, actor *core.Record) (*outcome, error) {
	c, col := p.c, p.col
	if p.skip {
		return m.superseded(tx, nodeID, c, p.skipRevert, actor)
	}
	if c.Op == OpDelete {
		seq, err := m.insertHubRow(db, &hubRow{
			node: nodeID, oseq: c.oseq, hlc: int64(c.hlc), base: int64(c.base), col: col.Id, rec: c.Record, op: OpDelete,
			patch: "{}", actor: c.Actor, tx: c.Tx, status: StatusApplied,
		})
		if err != nil {
			return nil, err
		}
		if err := putTombstone(db, col.Id, c.Record, "delete", int64(c.hlc), nodeID, c.Actor, "", m.created()); err != nil {
			return nil, err
		}
		_, err = db.NewQuery("DELETE FROM _sync_meta WHERE collection={:c} AND record={:r}").
			Bind(dbx.Params{"c": col.Id, "r": c.Record}).Execute()
		return &outcome{status: proto.ResApplied, seq: seq}, err
	}

	var hash []byte
	enc := ""
	fresh, _ := tx.FindRecordById(col.Id, c.Record)
	if isLast && fresh != nil {
		post, err := fieldValues(fresh, p.fields, p.pol.Types)
		if err != nil {
			return nil, err
		}
		hash = canonicalHash(col.Id, c.Record, post)
		eff := map[string]any{}
		if !existedBefore {
			for k, v := range post {
				eff[k] = v
			}
		} else {
			eff, _ = diffPatch(p.fields, p.pol, pre, post)
		}
		if enc, err = encodePatch(eff); err != nil {
			return nil, err
		}
	} else {
		var err error
		if enc, err = encodePatch(p.applied); err != nil {
			return nil, err
		}
	}
	seq, err := m.insertHubRow(db, &hubRow{
		node: nodeID, oseq: c.oseq, hlc: int64(c.hlc), base: int64(c.base), col: col.Id, rec: c.Record, op: c.Op,
		patch: enc, hash: hash, actor: c.Actor, tx: c.Tx, status: StatusApplied,
	})
	if err != nil {
		return nil, err
	}
	if isLast && fresh != nil {
		if err := upsertMeta(db, col.Id, c.Record, p.nh, p.nn, hash); err != nil {
			return nil, err
		}
	}
	st := proto.ResApplied
	if p.merged {
		st = proto.ResMerged
		// the node keeps its lost plain fields and has not seen the hub's autodate
		// values: send it the hub state so that it converges
		if _, err := m.insertRevert(tx, nodeID, col.Id, c.Record, actor); err != nil {
			return nil, err
		}
	}
	return &outcome{status: st, seq: seq, hash: hash}, nil
}

// classify maps a replay or save error to a rejection when it is a data
// problem, and leaves it as an internal error otherwise.
func classify(err error) error {
	if err == nil {
		return nil
	}
	var rj *rejection
	if errors.As(err, &rj) {
		return err
	}
	var ve validation.Errors
	if strings.Contains(err.Error(), "UNIQUE constraint") {
		return reject(proto.CodeUniqueViolation, err.Error())
	}
	if errors.As(err, &ve) {
		return reject(codeForValidation(ve), err.Error())
	}
	var ae *router.ApiError
	if errors.As(err, &ae) {
		switch {
		case ae.Status == http.StatusForbidden || ae.Status == http.StatusUnauthorized || ae.Status == http.StatusNotFound:
			return reject(proto.CodeRuleDenied, err.Error())
		case ae.Status == http.StatusBadRequest:
			if raw, ok := ae.RawData().(error); ok {
				if strings.Contains(raw.Error(), "rule failure") {
					return reject(proto.CodeRuleDenied, err.Error())
				}
				if strings.Contains(raw.Error(), "UNIQUE constraint") {
					return reject(proto.CodeUniqueViolation, err.Error())
				}
				var rve validation.Errors
				if errors.As(raw, &rve) {
					return reject(codeForValidation(rve), err.Error())
				}
			}
			return reject(proto.CodeValidationFailed, err.Error())
		case ae.Status >= 400 && ae.Status < 500:
			return reject(proto.CodeValidationFailed, err.Error())
		}
	}
	return err
}

func codeForValidation(ve validation.Errors) string {
	for _, e := range ve {
		if c, ok := e.(interface{ Code() string }); ok && c.Code() == "validation_not_unique" {
			return proto.CodeUniqueViolation
		}
		var nested validation.Errors
		if errors.As(e, &nested) && codeForValidation(nested) == proto.CodeUniqueViolation {
			return proto.CodeUniqueViolation
		}
	}
	return proto.CodeValidationFailed
}

// isTransient tells a retriable infrastructure error from a permanent one.
func isTransient(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	s := strings.ToLower(err.Error())
	for _, t := range []string{"database is locked", "sqlite_busy", "busy", "interrupted", "connection", "closed", "timeout"} {
		if strings.Contains(s, t) {
			return true
		}
	}
	return false
}
