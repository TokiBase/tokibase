//go:build !no_sync

package sync

import (
	"compress/gzip"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/pocketbase/dbx"
	validation "github.com/pocketbase/ozzo-validation/v4"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/sync/hlc"
	"github.com/tokibase/tokibase/modules/sync/proto"
	"github.com/tokibase/tokibase/tools/router"
)

// The hub apply pipeline of PR3 (docs/SYNC_DESIGN.md §4.1).
//
// IMPORTANT (temporary): pushed changes are applied as SUPERUSER. They go
// straight through SaveWithContext/DeleteWithContext with
// kernel.WithSyncOrigin(Mode: Push), so collection rules, fieldperm,
// batchguard and the actor grants of §1.6 are NOT evaluated yet. PR4 replaces
// this with the rule-checked replay (apis.ReplayRecordRequests) and the actor
// checks. Until then only enrolled, active nodes can reach this code, but an
// enrolled node can write any collection whose policy direction is both|push.

// rejection is returned by the apply functions when a change is refused. It is
// not an internal error: the change is recorded as rejected and the node gets
// a revert row.
type rejection struct {
	code string
	msg  string
}

func (r *rejection) Error() string { return r.code + ": " + r.msg }

func reject(code, msg string) *rejection { return &rejection{code: code, msg: msg} }

// hubChange is a decoded pushed change.
type hubChange struct {
	proto.PushChange
	oseq  int64
	hlc   hlc.HLC
	base  hlc.HLC
	patch map[string]any
	hash  []byte
}

// outcome is the result of applying one change inside the transaction.
type outcome struct {
	status string
	code   string
	seq    int64
	hash   []byte
}

func parseChange(nodeID string, c proto.PushChange) (*hubChange, error) {
	i := strings.LastIndexByte(c.ID, ':')
	if i <= 0 || c.ID[:i] != nodeID {
		return nil, errors.New("change id must be \"<node>:<origin_seq>\" of the pushing node")
	}
	oseq, err := strconv.ParseInt(c.ID[i+1:], 10, 64)
	if err != nil || oseq <= 0 {
		return nil, errors.New("invalid origin_seq in the change id")
	}
	h, err := hlc.Parse(c.HLC)
	if err != nil {
		return nil, err
	}
	var base hlc.HLC
	if c.Base != "" {
		if base, err = hlc.Parse(c.Base); err != nil {
			return nil, err
		}
	}
	switch c.Op {
	case OpCreate, OpUpdate, OpDelete, OpPurge:
	default:
		return nil, errors.New("invalid op")
	}
	if c.Collection == "" || c.Record == "" {
		return nil, errors.New("collection and record are required")
	}
	hc := &hubChange{PushChange: c, oseq: oseq, hlc: h, base: base, patch: map[string]any{}}
	if len(c.Patch) > 0 && string(c.Patch) != "null" {
		if err := json.Unmarshal(c.Patch, &hc.patch); err != nil {
			return nil, errors.New("invalid patch")
		}
	}
	if c.Hash != "" {
		if hc.hash, err = hex.DecodeString(c.Hash); err != nil {
			return nil, errors.New("invalid hash")
		}
	}
	return hc, nil
}

// readBody reads the request body honoring Content-Encoding: gzip and the
// size limit (413 sync_batch_too_large).
func readPushBody(e *core.RequestEvent) ([]byte, error) {
	var rd io.Reader = http.MaxBytesReader(e.Response, e.Request.Body, proto.MaxPushBytes)
	if strings.EqualFold(e.Request.Header.Get("Content-Encoding"), "gzip") {
		gr, err := gzip.NewReader(rd)
		if err != nil {
			return nil, err
		}
		defer gr.Close()
		rd = io.LimitReader(gr, proto.MaxPushBytes+1)
	}
	b, err := io.ReadAll(rd)
	if err != nil {
		return nil, err
	}
	if len(b) > proto.MaxPushBytes {
		return nil, &http.MaxBytesError{Limit: proto.MaxPushBytes}
	}
	return b, nil
}

// pushHandler is POST /api/sync/push (docs/SYNC_DESIGN.md §3.4).
func (m *Module) pushHandler(e *core.RequestEvent) error {
	nodeID := NodeFrom(e)
	body, err := readPushBody(e)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return syncErr(e, http.StatusRequestEntityTooLarge, proto.CodeBatchTooLarge, "The push body exceeds 8 MiB.", nil)
		}
		return syncErr(e, http.StatusBadRequest, proto.CodeBadRequest, "invalid request body", nil)
	}
	var req proto.PushRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return syncErr(e, http.StatusBadRequest, proto.CodeBadRequest, "invalid request body", nil)
	}
	if len(req.Changes) > proto.MaxPushChanges {
		return syncErr(e, http.StatusRequestEntityTooLarge, proto.CodeBatchTooLarge, "A push carries at most 500 changes.", nil)
	}
	chs := make([]*hubChange, 0, len(req.Changes))
	for _, c := range req.Changes {
		hc, err := parseChange(nodeID, c)
		if err != nil {
			return syncErr(e, http.StatusBadRequest, proto.CodeBadRequest, err.Error(), nil)
		}
		chs = append(chs, hc)
	}

	m.applyMu.Lock()
	defer m.applyMu.Unlock()

	pushed, err := m.pushedSeq(e.App, nodeID)
	if err != nil {
		return err
	}
	for i, c := range chs {
		if (i == 0 && c.oseq > pushed+1) || (i > 0 && c.oseq != chs[i-1].oseq+1) {
			return syncErr(e, http.StatusConflict, proto.CodePushGap, "The push does not continue the processed sequence.", map[string]any{"push_from": pushed + 1})
		}
	}

	results := make([]proto.PushResult, 0, len(chs))
	for i := 0; i < len(chs); {
		j := i + 1
		if chs[i].Tx != "" {
			for j < len(chs) && chs[j].Tx == chs[i].Tx {
				j++
			}
		}
		res, err := m.processGroup(e.App, nodeID, chs[i:j])
		if err != nil {
			e.App.Logger().Error("sync: push failed", "node", nodeID, "error", err)
			return syncErr(e, http.StatusInternalServerError, "sync_internal", "push failed", nil)
		}
		results = append(results, res...)
		i = j
	}
	m.touchNode(e.App, nodeID)
	m.notifyHead()

	acked, err := m.pushedSeq(e.App, nodeID)
	if err != nil {
		return err
	}
	return e.JSON(http.StatusOK, proto.PushResponse{
		ServerTime: m.now().UTC().Format(proto.TimeLayout), AckedThrough: acked, Results: results,
	})
}

func (m *Module) pushedSeq(app kernel.App, nodeID string) (int64, error) {
	var v float64
	err := app.DB().NewQuery("SELECT COALESCE(pushed_origin_seq,0) FROM " + NodesCollection + " WHERE id={:id}").
		Bind(dbx.Params{"id": nodeID}).Row(&v)
	return int64(v), err
}

func (m *Module) touchNode(app kernel.App, nodeID string) {
	_, _ = app.NonconcurrentDB().NewQuery("UPDATE " + NodesCollection + " SET last_seen={:t} WHERE id={:id}").
		Bind(dbx.Params{"t": m.created(), "id": nodeID}).Execute()
}

// storedResult answers a change that was processed before (idempotency).
func (m *Module) storedResult(app kernel.App, nodeID string, c *hubChange) (proto.PushResult, bool) {
	var row struct {
		Seq    int64  `db:"seq"`
		Status string `db:"status"`
		Code   string `db:"code"`
		Hash   []byte `db:"hash"`
	}
	err := app.DB().NewQuery("SELECT seq, status, code, hash FROM _changes WHERE node={:n} AND origin_seq={:o}").
		Bind(dbx.Params{"n": nodeID, "o": c.oseq}).One(&row)
	if err != nil {
		return proto.PushResult{}, false
	}
	res := proto.PushResult{ID: c.ID, Status: proto.ResDuplicate, HubSeq: row.Seq, Code: row.Code, Was: row.Status}
	if row.Status == StatusRejected && row.Code == proto.CodeSuperseded {
		res.Was = proto.ResSuperseded
	}
	if len(row.Hash) > 0 {
		res.Hash = hex.EncodeToString(row.Hash)
	}
	if row.Status == StatusRejected {
		// the revert row directly follows the rejected row
		var rv int64
		if app.DB().NewQuery("SELECT seq FROM _changes WHERE seq={:s} AND status='revert' AND target={:n}").
			Bind(dbx.Params{"s": row.Seq + 1, "n": nodeID}).Row(&rv) == nil {
			res.HubSeq = rv
		}
	}
	return res, true
}

// Hub change statuses (the spoke ones are in capture.go).
const (
	StatusApplied  = "applied"
	StatusRejected = "rejected"
	StatusRevert   = "revert"
)

// processGroup applies a tx group (or one change). Every change is applied in
// one transaction; if one is rejected the whole group is rolled back and
// rejected with the first failure's code.
func (m *Module) processGroup(app kernel.App, nodeID string, group []*hubChange) ([]proto.PushResult, error) {
	out := make([]proto.PushResult, len(group))
	var todo []*hubChange
	var todoIdx []int
	for k, c := range group {
		if r, ok := m.storedResult(app, nodeID, c); ok {
			out[k] = r
			continue
		}
		todo = append(todo, c)
		todoIdx = append(todoIdx, k)
	}
	if len(todo) == 0 {
		return out, nil
	}

	var outs []*outcome
	var applied bool
	err := app.RunInTransaction(func(tx kernel.App) error {
		outs = outs[:0]
		for _, c := range todo {
			o, err := m.applyOne(tx, nodeID, c)
			if err != nil {
				return err
			}
			outs = append(outs, o)
		}
		applied = true
		return m.advancePushed(tx, nodeID, todo[len(todo)-1].oseq)
	})
	if applied && err == nil {
		for k, o := range outs {
			out[todoIdx[k]] = proto.PushResult{ID: todo[k].ID, Status: o.status, Code: o.code, HubSeq: o.seq, Hash: hex.EncodeToString(o.hash)}
		}
		return out, nil
	}
	var rj *rejection
	if !errors.As(err, &rj) {
		return nil, err
	}

	// rejected: the transaction rolled back; record the verdict of every change
	// plus a revert row per record, in a transaction of its own
	var rejSeqs []int64
	err = app.RunInTransaction(func(tx kernel.App) error {
		rejSeqs = rejSeqs[:0]
		for _, c := range todo {
			seq, err := m.recordRejected(tx, nodeID, c, rj.code, true)
			if err != nil {
				return err
			}
			rejSeqs = append(rejSeqs, seq)
		}
		return m.advancePushed(tx, nodeID, todo[len(todo)-1].oseq)
	})
	if err != nil {
		return nil, err
	}
	for k, c := range todo {
		out[todoIdx[k]] = proto.PushResult{ID: c.ID, Status: proto.ResRejected, Code: rj.code, HubSeq: rejSeqs[k]}
	}
	return out, nil
}

func (m *Module) advancePushed(tx kernel.App, nodeID string, oseq int64) error {
	_, err := tx.NonconcurrentDB().NewQuery("UPDATE " + NodesCollection + " SET pushed_origin_seq={:s} WHERE id={:id} AND COALESCE(pushed_origin_seq,0)<{:s}").
		Bind(dbx.Params{"s": oseq, "id": nodeID}).Execute()
	return err
}

// recordRejected stores the rejected row of c and, when revert is set, the
// revert row (target = the pushing node) right after it. It returns the seq a
// node should see for the result: the revert row when there is one.
func (m *Module) recordRejected(tx kernel.App, nodeID string, c *hubChange, code string, revert bool) (int64, error) {
	db := tx.NonconcurrentDB()
	patch := "{}"
	if len(c.Patch) > 0 && string(c.Patch) != "null" {
		patch = string(c.Patch)
	}
	seq, err := m.insertHubRow(db, &hubRow{
		node: nodeID, oseq: c.oseq, hlc: int64(c.hlc), base: int64(c.base), col: c.Collection, rec: c.Record,
		op: c.Op, patch: patch, hash: c.hash, actor: c.Actor, tx: c.Tx, status: StatusRejected, code: code,
	})
	if err != nil {
		return 0, err
	}
	if !revert || (c.Op == OpDelete && code == proto.CodeSuperseded) {
		return seq, nil
	}
	rseq, err := m.insertRevert(tx, nodeID, c.Collection, c.Record)
	if err != nil {
		return 0, err
	}
	if rseq == 0 {
		return seq, nil
	}
	return rseq, nil
}

// insertRevert writes the revert row of a record: the full current hub state,
// or op d when the record does not exist. It returns 0 when the collection is
// unknown or not replicated (no row).
func (m *Module) insertRevert(tx kernel.App, nodeID, colRef, recID string) (int64, error) {
	col, err := tx.FindCachedCollectionByNameOrId(colRef)
	if err != nil {
		return 0, nil
	}
	p := m.pol.For(col)
	if p == nil {
		return 0, nil
	}
	db := tx.NonconcurrentDB()
	r := &hubRow{node: m.hub.id, col: col.Id, rec: recID, target: nodeID, status: StatusRevert, hlc: int64(m.Clock().Now())}
	rec, _ := tx.FindRecordById(col.Id, recID)
	if rec == nil {
		r.op, r.patch = OpDelete, "{}"
	} else {
		vals, err := fieldValues(rec, syncedFields(col, p))
		if err != nil {
			return 0, err
		}
		enc, err := encodePatch(vals)
		if err != nil {
			return 0, err
		}
		r.op, r.patch, r.hash = OpUpdate, enc, canonicalHash(col.Id, recID, vals)
		if h, _, ok := readMeta(db, col.Id, recID); ok {
			r.hlc = h
		}
	}
	return m.insertHubRow(db, r)
}

type hubRow struct {
	node   string
	oseq   int64 // 0 = the new hub seq
	hlc    int64
	base   int64
	col    string
	rec    string
	op     string
	patch  string
	hash   []byte
	actor  string
	tx     string
	target string
	status string
	code   string
}

// insertHubRow inserts a hub `_changes` row and returns its hub seq.
func (m *Module) insertHubRow(db dbx.Builder, r *hubRow) (int64, error) {
	var sv int64
	if v, ok, err := (dbState{db: db}).Get(keySchemaVersion); err == nil && ok {
		sv, _ = strconv.ParseInt(v, 10, 64)
	}
	var hashArg any
	if r.hash != nil {
		hashArg = r.hash
	}
	oseq := any(r.oseq)
	if r.oseq == 0 {
		oseq = nil
	}
	q := `INSERT INTO _changes
  (node, origin_seq, hlc, base_hlc, collection, record, op, patch, hash, schema_version, actor, tx, target, status, code, created)
  VALUES ({:node}, COALESCE({:oseq}, (SELECT COALESCE((SELECT seq FROM sqlite_sequence WHERE name='_changes'),0)+1)),
  {:hlc}, {:base}, {:col}, {:rec}, {:op}, {:patch}, {:hash}, {:sv}, {:actor}, {:tx}, {:target}, {:status}, {:code}, {:created})`
	res, err := db.NewQuery(q).Bind(dbx.Params{
		"node": r.node, "oseq": oseq, "hlc": r.hlc, "base": r.base, "col": r.col, "rec": r.rec, "op": r.op,
		"patch": r.patch, "hash": hashArg, "sv": sv, "actor": r.actor, "tx": r.tx, "target": r.target,
		"status": r.status, "code": r.code, "created": m.created(),
	}).Execute()
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func readMeta(db dbx.Builder, colId, id string) (h int64, node string, ok bool) {
	var row struct {
		HLC  int64  `db:"hlc"`
		Node string `db:"node"`
	}
	if err := db.NewQuery("SELECT hlc, node FROM _sync_meta WHERE collection={:c} AND record={:r}").
		Bind(dbx.Params{"c": colId, "r": id}).One(&row); err != nil {
		return 0, "", false
	}
	return row.HLC, row.Node, true
}

func tombstoneKind(db dbx.Builder, colId, id string) string {
	var k string
	if err := db.NewQuery("SELECT kind FROM _sync_tombstones WHERE collection={:c} AND record={:r}").
		Bind(dbx.Params{"c": colId, "r": id}).Row(&k); err != nil {
		return ""
	}
	return k
}

// isTyped reports whether v is a counter/set operation object.
func typedOp(v any) (map[string]any, bool) {
	mp, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	_, inc := mp["$inc"]
	_, add := mp["$add"]
	_, rm := mp["$rm"]
	return mp, inc || add || rm
}

// applyOne applies one change inside tx. A refusal comes back as *rejection.
func (m *Module) applyOne(tx kernel.App, nodeID string, c *hubChange) (*outcome, error) {
	col, err := tx.FindCachedCollectionByNameOrId(c.Collection)
	if err != nil {
		return nil, reject(proto.CodePolicyDirection, "unknown collection")
	}
	p := m.pol.For(col)
	if p == nil || (p.Direction != DirBoth && p.Direction != DirPush) {
		return nil, reject(proto.CodePolicyDirection, "the collection does not accept pushes")
	}
	if c.hlc.Physical().After(m.now().Add(maxDrift())) {
		return nil, reject(proto.CodeFutureHLC, "the change hlc is too far in the future")
	}
	m.Clock().Observe(c.hlc)

	db := tx.NonconcurrentDB()
	origin := &kernel.SyncOrigin{Mode: kernel.SyncModePush, Node: nodeID, HLC: uint64(c.hlc), ChangeID: c.ID, Actor: c.Actor}
	ctx := kernel.WithSyncOrigin(context.Background(), origin)
	tomb := tombstoneKind(db, col.Id, c.Record)
	metaH, metaNode, hasMeta := readMeta(db, col.Id, c.Record)
	rec, _ := tx.FindRecordById(col.Id, c.Record)

	switch c.Op {
	case OpPurge:
		return nil, reject(proto.CodeLegalTombstone, "purge is not accepted from nodes")
	case OpDelete:
		if rec == nil {
			return m.superseded(tx, nodeID, c, false)
		}
		// deletes are final (§4.2): a delete older than a concurrent update still deletes
		if err := tx.DeleteWithContext(ctx, rec); err != nil {
			return nil, classify(err)
		}
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

	// create / update
	if tomb == "legal" {
		return nil, reject(proto.CodeLegalTombstone, "the record was purged")
	}
	if tomb != "" {
		return nil, reject(proto.CodeTombstoned, "the record was deleted")
	}
	if rec == nil && c.Op == OpUpdate {
		return nil, reject(proto.CodeOrphaned, "the record does not exist on the hub")
	}

	fields := syncedFields(col, p)
	allowed := make(map[string]core.Field, len(fields))
	for _, f := range fields {
		allowed[f.GetName()] = f
	}
	patch := c.patch
	merged := false
	if rec != nil && int64(c.base) != metaH {
		// concurrent: the writer did not see the latest hub version (lww, record level)
		if hlc.Less(hlc.HLC(metaH), metaNode, c.hlc, nodeID) {
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
				return m.superseded(tx, nodeID, c, true)
			}
			patch, merged = typedOnly, true
		}
	}

	isNew := rec == nil
	var pre map[string]any
	if isNew {
		rec = core.NewRecord(col)
		rec.Set("id", c.Record)
	} else {
		if pre, err = fieldValues(rec, fields); err != nil {
			return nil, err
		}
	}
	// autodate columns: the interceptor regenerates them on every save, so the
	// origin value (or, without one in the patch, the current value) is put back
	// after the save
	wantAuto := map[string]string{}
	if !isNew {
		for _, f := range fields {
			if f.Type() == kernel.FieldTypeAutodate {
				wantAuto[f.GetName()] = rec.GetString(f.GetName())
			}
		}
	}
	for name, v := range patch {
		f, ok := allowed[name]
		if !ok {
			continue // unknown, excluded or never-synced field (file, password, tokenKey, derived)
		}
		if op, typed := typedOp(v); typed {
			applyTyped(rec, name, op)
			continue
		}
		if f.Type() == kernel.FieldTypeAutodate {
			if s, ok := v.(string); ok {
				wantAuto[name] = s
			}
		}
		rec.Set(name, v)
	}
	if err := tx.SaveWithContext(ctx, rec); err != nil {
		return nil, classify(err)
	}
	if err := fixAutodates(tx, col, rec.Id, wantAuto); err != nil {
		return nil, err
	}
	fresh, err := tx.FindRecordById(col.Id, c.Record)
	if err != nil {
		return nil, err
	}
	post, err := fieldValues(fresh, fields)
	if err != nil {
		return nil, err
	}
	hash := canonicalHash(col.Id, c.Record, post)

	eff := map[string]any{}
	if isNew {
		for k, v := range post {
			eff[k] = v
		}
	} else {
		eff, _ = diffPatch(fields, p, pre, post)
	}
	encEff, err := encodePatch(eff)
	if err != nil {
		return nil, err
	}
	seq, err := m.insertHubRow(db, &hubRow{
		node: nodeID, oseq: c.oseq, hlc: int64(c.hlc), base: int64(c.base), col: col.Id, rec: c.Record, op: c.Op,
		patch: encEff, hash: hash, actor: c.Actor, tx: c.Tx, status: StatusApplied,
	})
	if err != nil {
		return nil, err
	}

	// record clock: max(meta.hlc, change.hlc); a lost-lww merge keeps the winner's clock
	nh, nn := int64(c.hlc), nodeID
	if hasMeta && (merged || !hlc.Less(hlc.HLC(metaH), metaNode, c.hlc, nodeID)) {
		nh, nn = metaH, metaNode
	}
	if err := upsertMeta(db, col.Id, c.Record, nh, nn, hash); err != nil {
		return nil, err
	}
	st := proto.ResApplied
	if merged {
		st = proto.ResMerged
		// the node keeps its lost plain fields and has not seen the hub's autodate
		// values: send it the hub state so that it converges
		if _, err := m.insertRevert(tx, nodeID, col.Id, c.Record); err != nil {
			return nil, err
		}
	}
	return &outcome{status: st, seq: seq, hash: hash}, nil
}

// superseded records a lost lww change: nothing is written to the record, the
// row is stored as rejected/superseded for idempotency, and (withRevert) the
// node gets the hub state so that it converges even if the winning change does
// not touch the fields it edited.
func (m *Module) superseded(tx kernel.App, nodeID string, c *hubChange, withRevert bool) (*outcome, error) {
	seq, err := m.recordRejected(tx, nodeID, c, proto.CodeSuperseded, withRevert)
	if err != nil {
		return nil, err
	}
	return &outcome{status: proto.ResSuperseded, code: proto.CodeSuperseded, seq: seq}, nil
}

// applyTyped replays a counter/set operation through PocketBase's modifiers.
func applyTyped(rec *core.Record, name string, op map[string]any) {
	if d, ok := op["$inc"]; ok {
		rec.Set(name+"+", d)
	}
	if add, ok := op["$add"]; ok {
		rec.Set(name+"+", add)
	}
	if rm, ok := op["$rm"]; ok {
		rec.Set(name+"-", rm)
	}
}

// fixAutodates restores the origin value of autodate fields when the autodate
// interceptor regenerated it (it does so when the value equals the loaded one).
func fixAutodates(tx kernel.App, col *core.Collection, id string, want map[string]string) error {
	if len(want) == 0 {
		return nil
	}
	for name, v := range want {
		var cur string
		if err := tx.NonconcurrentDB().NewQuery("SELECT {{" + name + "}} FROM {{" + col.Name + "}} WHERE id={:id}").
			Bind(dbx.Params{"id": id}).Row(&cur); err != nil {
			return err
		}
		if cur == v {
			continue
		}
		if _, err := tx.NonconcurrentDB().NewQuery("UPDATE {{" + col.Name + "}} SET {{" + name + "}}={:v} WHERE id={:id}").
			Bind(dbx.Params{"v": v, "id": id}).Execute(); err != nil {
			return err
		}
	}
	return nil
}

// classify maps a save error to a rejection when it is a data problem, and
// leaves it as an internal error otherwise.
func classify(err error) error {
	var ve validation.Errors
	if strings.Contains(err.Error(), "UNIQUE constraint") {
		return reject(proto.CodeUniqueViolation, err.Error())
	}
	if errors.As(err, &ve) {
		return reject(proto.CodeValidationFailed, err.Error())
	}
	var ae *router.ApiError
	if errors.As(err, &ae) {
		if ae.Status == http.StatusForbidden || ae.Status == http.StatusUnauthorized {
			return reject(proto.CodeRuleDenied, err.Error())
		}
		if ae.Status >= 400 && ae.Status < 500 {
			return reject(proto.CodeValidationFailed, err.Error())
		}
	}
	return err
}
