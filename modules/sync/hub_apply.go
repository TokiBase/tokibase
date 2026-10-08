//go:build !no_sync

package sync

import (
	"compress/gzip"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	stdatomic "sync/atomic"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/sync/hlc"
	"github.com/tokibase/tokibase/modules/sync/proto"
)

// The hub apply pipeline of PR3 (docs/SYNC_DESIGN.md §4.1).
//
// Since PR4 a pushed change is REPLAYED as its original actor through
// apis.ReplayRecordRequests (see hub_replay.go and actor.go): collection rules,
// fieldperm, validation and batchguard apply as for a client.

// rejection is returned by the apply functions when a change is refused. It is
// not an internal error: the change is recorded as rejected and the node gets
// a revert row.
type rejection struct {
	code string
	msg  string
	// park parks the change for review (actor revoked) instead of reverting it.
	park bool
	// internal marks an infrastructure failure during validation (not a verdict).
	internal bool
	// conflict (sync PR5) is the `_sync_conflicts` row to record with the
	// rejection, written in the transaction that stores it; change is the
	// pushed change it belongs to.
	conflict *ConflictInfo
	change   *hubChange
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
	actor  *actorCtx
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
		res, err := m.processGroup(e.App, nodeID, e.RealIP(), pushed, chs[i:j])
		if err != nil {
			e.App.Logger().Error("sync: push failed", "node", nodeID, "error", err)
			return syncErr(e, http.StatusInternalServerError, "sync_internal", "push failed", nil)
		}
		results = append(results, res...)
		i = j
	}
	m.touchSeen(e.App, nodeID)
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

func (m *Module) touchSeen(app kernel.App, nodeID string) {
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
	StatusParked   = "parked"
)

// processGroup applies a tx group (or one change). Every change is applied in
// one transaction; if one is rejected the whole group is rolled back and
// rejected with the first failure's code.
func (m *Module) processGroup(app kernel.App, nodeID, ip string, pushed int64, group []*hubChange) ([]proto.PushResult, error) {
	out := make([]proto.PushResult, len(group))
	var todo []*hubChange
	var todoIdx []int
	for k, c := range group {
		if r, ok := m.storedResult(app, nodeID, c); ok {
			out[k] = r
			continue
		}
		if c.oseq <= pushed {
			// pushed_origin_seq is authoritative: this sequence was processed and its
			// row is gone (compaction, restore). Never apply it a second time.
			out[k] = proto.PushResult{ID: c.ID, Status: proto.ResDuplicate, Was: proto.ResApplied}
			continue
		}
		todo = append(todo, c)
		todoIdx = append(todoIdx, k)
	}
	if len(todo) == 0 {
		return out, nil
	}

	var outs []*outcome
	err := app.RunInTransaction(func(tx kernel.App) error {
		var err error
		if outs, err = m.applyGroup(tx, nodeID, ip, todo); err != nil {
			return err
		}
		return m.advancePushed(tx, nodeID, todo[len(todo)-1].oseq)
	})
	if err == nil {
		for k, o := range outs {
			out[todoIdx[k]] = proto.PushResult{ID: todo[k].ID, Status: o.status, Code: o.code, HubSeq: o.seq, Hash: hex.EncodeToString(o.hash)}
			m.auditApplied(nodeID, ip, todo[k], o)
		}
		return out, nil
	}
	var rj *rejection
	if !errors.As(err, &rj) {
		if isReplayTimeout(err) {
			// a group that keeps timing out (poison pill, deadlocked hook) must not
			// hold the apply mutex of every push for ever: it is permanent after
			// maxReplayTimeouts attempts (P4-9)
			if m.bumpTimeout(nodeID, todo[0].oseq) < maxReplayTimeouts {
				return nil, err
			}
			m.clearTimeout(nodeID, todo[0].oseq)
		} else if isTransient(err) {
			return nil, err // retriable: the node pushes again
		}
		// a permanent failure must not block the queue of the node for ever
		app.Logger().Error("sync: change could not be applied, rejecting it", "node", nodeID, "change", todo[0].ID, "error", err)
		rj = &rejection{code: CodeApplyError, msg: err.Error()}
	}

	// rejected: the transaction rolled back; record the verdict of every change
	// (plus a revert row per record, or a parked row and conflict), in a
	// transaction of its own
	var rejSeqs []int64
	err = app.RunInTransaction(func(tx kernel.App) error {
		rejSeqs = rejSeqs[:0]
		var revertActor *core.Record
		if !rj.park {
			revertActor = m.revertActor(tx, nodeID, todo)
		}
		for _, c := range todo {
			var seq int64
			var err error
			if rj.park {
				seq, err = m.recordParked(tx, nodeID, c, rj)
			} else {
				seq, err = m.recordRejected(tx, nodeID, c, rj.code, true, revertActor)
				if err == nil && rj.code == CodeApplyError {
					err = m.addConflict(tx, nodeID, c, rj, "reverted", "resolved")
				}
			}
			if err != nil {
				return err
			}
			rejSeqs = append(rejSeqs, seq)
			if rj.change == c && !rj.park {
				if err := m.writeRejectionConflict(tx, nodeID, rj); err != nil {
					return err
				}
			}
		}
		return m.advancePushed(tx, nodeID, todo[len(todo)-1].oseq)
	})
	if err != nil {
		return nil, err
	}
	status := proto.ResRejected
	if rj.park {
		status = proto.ResParked
	}
	for k, c := range todo {
		out[todoIdx[k]] = proto.PushResult{ID: c.ID, Status: status, Code: rj.code, HubSeq: rejSeqs[k]}
		emit(AuditReject, c.Collection, c.Record, m.auditDetails(nodeID, ip, c, nil, map[string]any{"code": rj.code, "message": truncate(rj.msg, 300), "parked": rj.park}))
	}
	return out, nil
}

// CodeApplyError is the code of a change that failed for a permanent
// infrastructure reason (neither a rule, validation nor unique problem).
const CodeApplyError = "apply_error"

// revertActor is the record whose view rights decide what a revert row may
// carry: the original actor of the rejected group when it still resolves, else
// the service actor of the node (nil when there is none).
func (m *Module) revertActor(tx kernel.App, nodeID string, group []*hubChange) *core.Record {
	aid := ""
	for _, c := range group {
		if c.Actor != "" && c.Actor != ActorNode {
			aid = c.Actor
			break
		}
	}
	if aid != "" {
		if a, rj := m.resolveActor(tx, nodeID, aid, nil); rj == nil {
			return a.rec
		}
	}
	return serviceActor(tx, nodeID)
}

// recordParked stores a change as parked with an open conflict (no revert row:
// the spoke keeps its state until an admin decides).
func (m *Module) recordParked(tx kernel.App, nodeID string, c *hubChange, rj *rejection) (int64, error) {
	patch := "{}"
	if len(c.Patch) > 0 && string(c.Patch) != "null" && !m.erased(tx, c) {
		patch = string(c.Patch)
	}
	seq, err := m.insertHubRow(tx.NonconcurrentDB(), &hubRow{
		node: nodeID, oseq: c.oseq, hlc: int64(c.hlc), base: int64(c.base), col: c.Collection, rec: c.Record,
		op: c.Op, patch: patch, hash: c.hash, actor: c.Actor, tx: c.Tx, status: StatusParked, code: rj.code,
	})
	if err != nil {
		return 0, err
	}
	if rj.change == c && rj.conflict != nil {
		col, cerr := tx.FindCachedCollectionByNameOrId(c.Collection)
		if cerr != nil {
			return seq, nil
		}
		return seq, m.writeConflict(tx, nodeID, c, col, rj.conflict)
	}
	return seq, m.addConflict(tx, nodeID, c, rj, "parked", "open")
}

// addConflict writes a `_sync_conflicts` row for a change that needs review.
func (m *Module) addConflict(tx kernel.App, nodeID string, c *hubChange, rj *rejection, resolution, status string) error {
	col, err := tx.FindCachedCollectionByNameOrId(ConflictsCollection)
	if err != nil {
		return nil // PR4 minimal schema missing: nothing to write to
	}
	r := core.NewRecord(col)
	r.Set("collection", truncate(c.Collection, 255))
	r.Set("record", truncate(c.Record, 255))
	r.Set("change", c.ID)
	r.Set("node", nodeID)
	r.Set("actor", truncate(c.Actor, 255))
	kind := rj.code
	if kind != CodeApplyError && kind != proto.CodeActorRevoked {
		kind = CodeApplyError
	}
	r.Set("kind", kind)
	r.Set("strategy", "lww")
	if len(c.Patch) > 0 && string(c.Patch) != "null" && !m.erased(tx, c) {
		r.Set("incoming", c.Patch)
	}
	r.Set("resolution", resolution)
	r.Set("status", status)
	r.Set("note", truncate(rj.msg, 2000))
	return tx.Save(r)
}

func (m *Module) advancePushed(tx kernel.App, nodeID string, oseq int64) error {
	_, err := tx.NonconcurrentDB().NewQuery("UPDATE " + NodesCollection + " SET pushed_origin_seq={:s} WHERE id={:id} AND COALESCE(pushed_origin_seq,0)<{:s}").
		Bind(dbx.Params{"s": oseq, "id": nodeID}).Execute()
	return err
}

// recordRejected stores the rejected row of c and, when revert is set, the
// revert row (target = the pushing node) right after it. It returns the seq a
// node should see for the result: the revert row when there is one.
func (m *Module) recordRejected(tx kernel.App, nodeID string, c *hubChange, code string, revert bool, actor *core.Record) (int64, error) {
	db := tx.NonconcurrentDB()
	patch := "{}"
	hash := c.hash
	if len(c.Patch) > 0 && string(c.Patch) != "null" && code != proto.CodeLegalTombstone && !m.erased(tx, c) {
		patch = string(c.Patch)
	} else {
		hash = nil // an erased record leaves no data in the log, not even a refused push
	}
	seq, err := m.insertHubRow(db, &hubRow{
		node: nodeID, oseq: c.oseq, hlc: int64(c.hlc), base: int64(c.base), col: c.Collection, rec: c.Record,
		op: c.Op, patch: patch, hash: hash, actor: c.Actor, tx: c.Tx, status: StatusRejected, code: code,
	})
	if err != nil {
		return 0, err
	}
	if !revert || (c.Op == OpDelete && code == proto.CodeSuperseded) {
		return seq, nil
	}
	rseq, err := m.insertRevert(tx, nodeID, c.Collection, c.Record, actor)
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
func (m *Module) insertRevert(tx kernel.App, nodeID, colRef, recID string, actor *core.Record) (int64, error) {
	col, err := tx.FindCachedCollectionByNameOrId(colRef)
	if err != nil {
		return 0, nil
	}
	p, err := m.pol.For(col)
	if err != nil {
		return 0, err
	}
	if p == nil {
		return 0, nil
	}
	db := tx.NonconcurrentDB()
	r := &hubRow{node: m.hub.id, col: col.Id, rec: recID, target: nodeID, status: StatusRevert, hlc: int64(m.Clock().Now())}
	rec, _ := tx.FindRecordById(col.Id, recID)
	var hidden map[string]struct{}
	if rec != nil {
		// a revert must not carry more than the actor may see (P3-2): a record
		// outside the view rule is reported as gone, without data
		vw := newViewer(tx, nodeID, actor)
		if !vw.inPartition(rec, p) {
			rec = nil // another partition: the pull reports an eviction, nothing is stored
		} else {
			vr, err := vw.view(rec, p, true)
			if err != nil {
				return 0, err
			}
			if !vr.visible {
				// outside the view rule of the actor: never an op d (that would delete
				// the data on the device); the row is a verdict without data
				r.op, r.patch, r.code = OpUpdate, "{}", revertInvisible
				return m.insertHubRow(db, r)
			}
			hidden = vr.hidden
		}
	}
	if rec == nil {
		r.op, r.patch = OpDelete, "{}"
	} else {
		vals, err := fieldValues(rec, syncedFields(col, p), p.Types)
		if err != nil {
			return 0, err
		}
		for name := range hidden {
			delete(vals, name)
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
	// partOld / partNew are the partition key of the record before and after
	// the change ("" without partition or when the record does not exist).
	partOld string
	partNew string
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
  (node, origin_seq, hlc, base_hlc, collection, record, op, patch, hash, schema_version, actor, tx, target, status, code, created, part_old, part_new)
  VALUES ({:node}, COALESCE({:oseq}, (SELECT COALESCE((SELECT seq FROM sqlite_sequence WHERE name='_changes'),0)+1)),
  {:hlc}, {:base}, {:col}, {:rec}, {:op}, {:patch}, {:hash}, {:sv}, {:actor}, {:tx}, {:target}, {:status}, {:code}, {:created}, {:po}, {:pn})`
	res, err := db.NewQuery(q).Bind(dbx.Params{
		"node": r.node, "oseq": oseq, "hlc": r.hlc, "base": r.base, "col": r.col, "rec": r.rec, "op": r.op,
		"patch": r.patch, "hash": hashArg, "sv": sv, "actor": r.actor, "tx": r.tx, "target": r.target,
		"status": r.status, "code": r.code, "created": m.created(), "po": r.partOld, "pn": r.partNew,
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

// superseded records a lost lww change: nothing is written to the record, the
// row is stored as rejected/superseded for idempotency, and (withRevert) the
// node gets the hub state so that it converges even if the winning change does
// not touch the fields it edited.
func (m *Module) superseded(tx kernel.App, nodeID string, c *hubChange, withRevert bool, actor *core.Record) (*outcome, error) {
	seq, err := m.recordRejected(tx, nodeID, c, proto.CodeSuperseded, withRevert, actor)
	if err != nil {
		return nil, err
	}
	return &outcome{status: proto.ResSuperseded, code: proto.CodeSuperseded, seq: seq}, nil
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

// maxReplayTimeouts is how often a group may time out before it is rejected.
const maxReplayTimeouts = 3

func isReplayTimeout(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "batch transaction timeout")
}

func timeoutKey(nodeID string, oseq int64) string { return nodeID + ":" + strconv.FormatInt(oseq, 10) }

func (m *Module) bumpTimeout(nodeID string, oseq int64) int {
	v, _ := m.timeouts.LoadOrStore(timeoutKey(nodeID, oseq), new(stdatomic.Int32))
	return int(v.(*stdatomic.Int32).Add(1))
}

func (m *Module) clearTimeout(nodeID string, oseq int64) { m.timeouts.Delete(timeoutKey(nodeID, oseq)) }

// ExpireParked rejects the parked changes older than TOKI_SYNC_PARK_TTL (default
// 30 d): the row becomes rejected (code park_expired), the node gets a revert
// row with the hub state, the open conflict is closed and the event is audited.
// It returns how many changes it rejected. The hub runs it hourly; operators
// resolve parked changes before that with `toki sync conflicts`.
func (m *Module) ExpireParked() (int, error) {
	if !m.hubReady() {
		return 0, nil
	}
	cutoff := m.now().UTC().Add(-parkTTL()).Format("2006-01-02 15:04:05.000Z")
	var rows []struct {
		Seq    int64  `db:"seq"`
		Node   string `db:"node"`
		OSeq   int64  `db:"origin_seq"`
		Col    string `db:"collection"`
		Record string `db:"record"`
		Actor  string `db:"actor"`
	}
	if err := m.app.DB().NewQuery("SELECT seq, node, origin_seq, collection, record, actor FROM _changes WHERE status='parked' AND created < {:c} ORDER BY seq LIMIT 500").
		Bind(dbx.Params{"c": cutoff}).All(&rows); err != nil {
		return 0, err
	}
	n := 0
	for _, r := range rows {
		m.applyMu.Lock()
		err := m.app.RunInTransaction(func(tx kernel.App) error {
			return m.RejectParked(tx, r.Seq, proto.CodeParkExpired, "park_ttl")
		})
		m.applyMu.Unlock()
		if err != nil {
			return n, err
		}
		n++
		emit(AuditReject, r.Col, r.Record, map[string]any{
			"code": proto.CodeParkExpired, "node": r.Node, "change": r.Node + ":" + strconv.FormatInt(r.OSeq, 10),
			"actor_grant": r.Actor, "stage": "park_ttl", "by": "park_ttl",
		})
	}
	return n, nil
}

// RejectParked turns the parked change with hub seq into a rejected one and
// sends the pushing node a revert row. It is the exit of `parked` for the TTL
// and the hook point of `toki sync conflicts --resolve ... reject`.
func (m *Module) RejectParked(tx kernel.App, seq int64, code, by string) error {
	var row struct {
		Node   string `db:"node"`
		OSeq   int64  `db:"origin_seq"`
		Col    string `db:"collection"`
		Record string `db:"record"`
	}
	if err := tx.NonconcurrentDB().NewQuery("SELECT node, origin_seq, collection, record FROM _changes WHERE seq={:s} AND status='parked'").
		Bind(dbx.Params{"s": seq}).One(&row); err != nil {
		return err
	}
	db := tx.NonconcurrentDB()
	if _, err := db.NewQuery("UPDATE _changes SET status='rejected', code={:c} WHERE seq={:s}").
		Bind(dbx.Params{"c": code, "s": seq}).Execute(); err != nil {
		return err
	}
	if _, err := m.insertRevert(tx, row.Node, row.Col, row.Record, serviceActor(tx, row.Node)); err != nil {
		return err
	}
	_, err := db.NewQuery("UPDATE " + ConflictsCollection + " SET status='resolved', resolution='rejected', resolved_by={:b}, resolved_at={:t} WHERE change={:ch} AND status='open'").
		Bind(dbx.Params{"b": by, "t": m.created(), "ch": row.Node + ":" + strconv.FormatInt(row.OSeq, 10)}).Execute()
	if err != nil && strings.Contains(err.Error(), "no such table") {
		return nil
	}
	return err
}

// erased reports whether the record of c has a legal tombstone (a purge): the
// data of a change to it is never stored.
func (m *Module) erased(tx kernel.App, c *hubChange) bool {
	id := c.Collection
	if col, err := tx.FindCachedCollectionByNameOrId(c.Collection); err == nil {
		id = col.Id
	}
	return tombstoneKind(tx.NonconcurrentDB(), id, c.Record) == "legal"
}
