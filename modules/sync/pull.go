//go:build !no_sync

package sync

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/sync/hlc"
	"github.com/tokibase/tokibase/modules/sync/proto"
)

// pullByteBudget is the size after which a pull page stops taking rows.
// A single row can still be large; the client reads up to 8 MiB.
const pullByteBudget = 4 << 20

const (
	defaultPage = 500
	maxPage     = 1000
	keyLowWater = "low_water"
)

func (m *Module) lowWater() int64 {
	if v, ok, err := (dbState{db: m.app.DB()}).Get(keyLowWater); err == nil && ok {
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	}
	return 0
}

// hubNodeStatus returns the status of a node ("" when it does not exist).
func hubNodeStatus(app kernel.App, nodeID string) string {
	var st string
	_ = app.DB().NewQuery("SELECT status FROM " + NodesCollection + " WHERE id={:id}").Bind(dbx.Params{"id": nodeID}).Row(&st)
	return st
}

func (m *Module) schemaVersion() int64 {
	if v, ok, err := (dbState{db: m.app.DB()}).Get(keySchemaVersion); err == nil && ok {
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	}
	return 0
}

// ackPulled records that the node has everything up to through (clamped to the
// hub head) and returns the clamped value.
func (m *Module) ackPulled(nodeID string, through int64) int64 {
	if head := m.headSeq(); through > head {
		through = head
	}
	if through <= 0 {
		return 0
	}
	_, _ = m.app.NonconcurrentDB().NewQuery("UPDATE " + NodesCollection + " SET pulled_seq={:s}, last_seen={:t} WHERE id={:id} AND COALESCE(pulled_seq,0)<{:s}").
		Bind(dbx.Params{"s": through, "id": nodeID, "t": m.created()}).Execute()
	return through
}

// pullHandler is GET /api/sync/pull?after=<seq>&limit=500[&wait=25]
// (docs/SYNC_DESIGN.md §3.5): rows are limited to the partition of the node,
// records that left it (or the view rule) become evictions (op "x").
func (m *Module) pullHandler(e *core.RequestEvent) error {
	nodeID := NodeFrom(e)
	q := e.Request.URL.Query()
	num := func(name string, def int64) (int64, bool) {
		s := q.Get(name)
		if s == "" {
			return def, true
		}
		n, err := strconv.ParseInt(s, 10, 64)
		return n, err == nil && n >= 0
	}
	after, ok1 := num("after", 0)
	limit, ok2 := num("limit", defaultPage)
	wait, ok3 := num("wait", 0)
	if !ok1 || !ok2 || !ok3 {
		return syncErr(e, http.StatusBadRequest, proto.CodeBadRequest, "after, limit and wait must be non-negative integers", nil)
	}
	if limit == 0 {
		limit = defaultPage
	}
	limit = min(limit, maxPage)
	wait = min(wait, proto.MaxWait)
	if head := m.headSeq(); after > head {
		// a cursor beyond the head (hub restored to an older state, corrupt cursor)
		// would skip the future changes with seq <= after (P3-7)
		return syncErr(e, http.StatusGone, proto.CodeRebootstrap, "The cursor is ahead of the hub; re-bootstrap.", map[string]any{"head": head})
	}
	if low := m.lowWater(); after < low {
		return syncErr(e, http.StatusGone, proto.CodeRebootstrap, "The cursor is older than the retained changes; re-bootstrap.", map[string]any{"low_water": low})
	}
	if st := hubNodeStatus(e.App, nodeID); st == NodeStale || st == NodeRebootstrap {
		// compaction no longer keeps changes for a stale node (§3.6)
		return syncErr(e, http.StatusGone, proto.CodeRebootstrap, "This node was offline longer than the retention; re-bootstrap.",
			map[string]any{"low_water": m.lowWater(), "node_status": st})
	}
	m.ackPulled(nodeID, after) // pull implicitly acks `after`

	deadline := time.NewTimer(time.Duration(wait) * time.Second)
	defer deadline.Stop()
	for {
		ch := m.notify.wait() // before the query, so a commit in between is not missed
		head := m.headSeq()
		resp, err := m.buildPull(e.App, nodeID, after, head, int(limit))
		if err != nil {
			e.App.Logger().Error("sync: pull failed", "node", nodeID, "error", err)
			return syncErr(e, http.StatusInternalServerError, "sync_internal", "pull failed", nil)
		}
		if wait == 0 || len(resp.Changes) > 0 || resp.More || resp.Next > after {
			return e.JSON(http.StatusOK, resp)
		}
		select {
		case <-ch:
		case <-deadline.C:
			return e.JSON(http.StatusOK, resp)
		case <-e.Request.Context().Done():
			return nil
		}
	}
}

type pullRow struct {
	Seq        int64  `db:"seq"`
	Node       string `db:"node"`
	OriginSeq  int64  `db:"origin_seq"`
	HLC        int64  `db:"hlc"`
	Collection string `db:"collection"`
	Record     string `db:"record"`
	Op         string `db:"op"`
	Patch      string `db:"patch"`
	Hash       []byte `db:"hash"`
	Status     string `db:"status"`
	Code       string `db:"code"`
	PartOld    string `db:"part_old"`
	PartNew    string `db:"part_new"`
}

// buildPull reads one page of deliverable changes with seq in (after, head].
func (m *Module) buildPull(app kernel.App, nodeID string, after, head int64, limit int) (*proto.PullResponse, error) {
	resp := &proto.PullResponse{
		ServerTime: m.now().UTC().Format(proto.TimeLayout), SchemaVersion: m.schemaVersion(),
		LowWater: m.lowWater(), Changes: []proto.PullChange{}, Next: after,
	}
	if head <= after {
		return resp, nil
	}
	vw := newViewer(app, nodeID, serviceActor(app, nodeID))
	// partitions (§3.5): rows of other partitions never leave the database
	pex, pparams, err := m.partitionExclusion(vw)
	if err != nil {
		return nil, err
	}
	params := dbx.Params{"a": after, "h": head, "n": nodeID, "hub": m.hub.id, "lim": limit + 1}
	for k, v := range pparams {
		params[k] = v
	}
	var rows []pullRow
	err = app.DB().NewQuery(`SELECT seq, node, origin_seq, hlc, collection, record, op, patch, hash, status, part_old, part_new, code FROM _changes
  WHERE seq > {:a} AND seq <= {:h}
    AND ((status='applied') OR (status='revert' AND target={:n}) OR (status='parked' AND node={:n}) OR (status='local' AND node={:hub}))` + pex + `
  ORDER BY seq LIMIT {:lim}`).
		Bind(params).All(&rows)
	if err != nil {
		return nil, err
	}
	if len(rows) > limit {
		rows = rows[:limit]
		resp.More = true
		resp.Next = rows[len(rows)-1].Seq
	} else {
		resp.Next = head
	}
	size := 0
	for i := range rows {
		if size >= pullByteBudget && i > 0 {
			// a page must stay below what the client reads (8 MiB): stop here, the
			// rest comes on the next page (a page always carries at least one row)
			resp.More, resp.Next = true, rows[i-1].Seq
			break
		}
		pc, ok, err := m.pullChange(app, vw, &rows[i])
		if err != nil {
			return nil, err
		}
		if ok {
			resp.Changes = append(resp.Changes, pc)
			size += len(pc.Patch) + 320
		}
	}
	return resp, nil
}

// pullChange converts a row to its wire form. Counter and set fields are sent
// as ABSOLUTE values read from the current record; revert rows carry the
// current full record and the current record clock.
func (m *Module) pullChange(app kernel.App, vw *viewer, r *pullRow) (proto.PullChange, bool, error) {
	col, err := app.FindCachedCollectionByNameOrId(r.Collection)
	if err != nil {
		return proto.PullChange{}, false, nil // collection deleted since
	}
	p, perr := m.pol.For(col)
	if perr != nil {
		return proto.PullChange{}, false, perr
	}
	parkedNotice := r.Status == StatusParked
	// reverts target one node and are delivered whatever the direction: a
	// push-only collection still has to learn that its change was refused
	if p == nil || (!parkedNotice && p.Direction != DirBoth && p.Direction != DirPull && r.Status != StatusRevert) {
		return proto.PullChange{}, false, nil
	}
	pc := proto.PullChange{
		Seq: r.Seq, ID: r.Node + ":" + strconv.FormatInt(r.OriginSeq, 10), Node: r.Node,
		HLC: hlc.HLC(r.HLC).String(), Collection: col.Id, Record: r.Record, Op: r.Op,
	}
	if len(r.Hash) > 0 {
		pc.Hash = hex.EncodeToString(r.Hash)
	}
	if parkedNotice {
		// informational: the node learns that its change waits for review and
		// keeps serving its local value (no data travels)
		pc.Notice, pc.Code, pc.Patch = proto.NoticeParked, r.Code, json.RawMessage(`{}`)
		pc.Hash = ""
		return pc, true, nil
	}
	fields := syncedFields(col, p)
	db := app.DB()

	if r.Status == StatusRevert {
		pc.Revert = true
		mh, mn, hasMeta := readMeta(db, col.Id, r.Record) // meta first: a newer record only costs a spurious conflict
		rec, _ := app.FindRecordById(col.Id, r.Record)
		if rec != nil && r.Code == revertInvisible {
			// the verdict was taken when the revert was stored, for the actor of
			// the rejected change: no data, and the node keeps (or evicts) its copy
			return m.invisibleNotice(pc, p), true, nil
		}
		var hidden map[string]struct{}
		var allowed map[string]struct{}
		if rec != nil && !vw.inPartition(rec, p) {
			// the record the node touched lives in another partition: the node
			// drops its copy (no tombstone) and learns nothing about it
			return evictChange(pc), true, nil
		}
		if rec != nil && r.Code == revertInvisible {
			// the verdict was taken when the revert was stored, for the actor of
			// the rejected change: no data, and the node keeps (or evicts) its copy
			return m.invisibleNotice(pc, p), true, nil
		}
		if rec != nil && r.Op == OpUpdate {
			// the hidden set is the one stored with the row (fields missing from
			// its patch), not a new evaluation for a different actor
			var stored map[string]any
			if json.Unmarshal([]byte(r.Patch), &stored) == nil {
				allowed = make(map[string]struct{}, len(stored))
				for k := range stored {
					allowed[k] = struct{}{}
				}
			}
		} else if rec != nil {
			vr, err := vw.view(rec, p, true)
			if err != nil {
				return pc, false, err
			}
			if !vr.visible {
				return m.invisibleNotice(pc, p), true, nil
			}
			hidden = vr.hidden
		}
		if rec == nil {
			pc.Op, pc.Patch, pc.Hash = OpDelete, json.RawMessage(`{}`), ""
			return pc, true, nil
		}
		vals, err := fieldValues(rec, fields, p.Types)
		if err != nil {
			return pc, false, err
		}
		for name := range hidden {
			delete(vals, name)
		}
		if allowed != nil {
			for name := range vals {
				if _, ok := allowed[name]; !ok {
					delete(vals, name)
				}
			}
		}
		enc, err := encodePatch(vals)
		if err != nil {
			return pc, false, err
		}
		pc.Op, pc.Patch = OpUpdate, json.RawMessage(enc)
		pc.Hash = hex.EncodeToString(canonicalHash(col.Id, r.Record, vals))
		if hasMeta {
			pc.HLC, pc.Node = hlc.HLC(mh).String(), mn
		}
		pc.Fields = m.fieldClockWire(db, col, p, r.Record, nil)
		return pc, true, nil
	}

	if r.Op == OpPurge {
		// an erasure reaches every node that pulls the collection, whatever its
		// partition: the node may hold the record from earlier
		pc.Patch = json.RawMessage(`{}`)
		return pc, true, nil
	}
	if m.pullRuleOn(vw, p) && col.ViewRule == nil && !p.Trusted {
		return pc, false, nil // view rule null: superusers only, never pulled unless trusted (§7.7)
	}
	enter := false
	if m.pullRuleOn(vw, p) && col.ViewRule != nil && *col.ViewRule != "" && r.Op == OpUpdate {
		// a restrictive view rule can make a record visible by an update: the node may
		// not have it, so the update travels as the whole record (an upsert)
		enter = true
	}
	if p.PartField != "" {
		pv, ok := vw.nodePartition(p)
		if !ok {
			return pc, false, nil // the node has no such parameter: it gets nothing
		}
		newIn, oldIn := r.PartNew == pv, r.PartOld == pv
		switch {
		case !newIn && !oldIn:
			return pc, false, nil
		case oldIn && !newIn && r.Op != OpDelete:
			return evictChange(pc), true, nil // the record left the partition
		case newIn && !oldIn && r.Op == OpUpdate:
			enter = true // the record entered the partition: send it whole
		}
	}
	if r.Op == OpDelete {
		pc.Patch = json.RawMessage(`{}`)
		return pc, true, nil
	}
	patch := map[string]any{}
	if err := json.Unmarshal([]byte(r.Patch), &patch); err != nil {
		return pc, false, err
	}
	if r.Op == OpUpdate && len(patch) == 0 {
		return pc, false, nil // a push that changed nothing
	}
	// only synced, readable fields travel (P3-10): fields that left the sync set
	// since the row was written and fields fieldperm hides from the node's actor
	// are dropped. A record that is gone cannot be checked: its row is skipped,
	// the delete row that follows it is enough.
	rec, _ := app.FindRecordById(col.Id, r.Record)
	if rec == nil {
		return pc, false, nil
	}
	vr, err := m.visibleForNode(vw, p, rec)
	if err != nil {
		return pc, false, err
	}
	if !vr.visible {
		if r.Op == OpUpdate {
			return evictChange(pc), true, nil // it may have been sent before: drop it
		}
		return pc, false, nil
	}
	if enter {
		vals, err := fieldValues(rec, fields, p.Types)
		if err != nil {
			return pc, false, err
		}
		for name := range vr.hidden {
			delete(vals, name)
		}
		enc, err := encodePatch(vals)
		if err != nil {
			return pc, false, err
		}
		pc.Op, pc.Patch = OpCreate, json.RawMessage(enc)
		pc.Hash = hex.EncodeToString(canonicalHash(col.Id, r.Record, vals))
		if mh, mn, ok := readMeta(db, col.Id, r.Record); ok {
			pc.HLC, pc.Node = hlc.HLC(mh).String(), mn
		}
		return pc, true, nil
	}
	allowed := make(map[string]struct{}, len(fields))
	for _, f := range fields {
		allowed[f.GetName()] = struct{}{}
	}
	for name := range patch {
		_, ok := allowed[name]
		_, hid := vr.hidden[name]
		if !ok || hid {
			delete(patch, name)
		}
	}
	if r.Op == OpUpdate && len(patch) == 0 {
		return pc, false, nil
	}
	cur, err := fieldValues(rec, fields, p.Types)
	if err != nil {
		return pc, false, err
	}
	for name, v := range patch {
		if _, typed := opOf(p.Types, name, v); !typed {
			continue
		}
		if abs, ok := cur[name]; ok {
			patch[name] = abs
		}
	}
	enc, err := encodePatch(patch)
	if err != nil {
		return pc, false, err
	}
	pc.Patch = json.RawMessage(enc)
	pc.Fields = m.fieldClockWire(db, col, p, r.Record, patch)
	return pc, true, nil
}

// revertInvisible is the code of a revert row whose record the actor of the
// rejected change may not view.
const revertInvisible = "invisible"

// EnvPullViewRule makes the view rule check of pulled rows the default for
// every collection whose policy does not set `pull_view_rule` (the column is a
// plain bool, so an explicit false cannot be told from unset: PR6 keeps it a
// per-policy choice and this variable is the global default).
const EnvPullViewRule = "TOKI_SYNC_PULL_VIEW_RULE"

// pullRuleOn tells whether the collection view rule is evaluated for the
// service actor of the pulling node: the policy's `pull_view_rule`, or the
// TOKI_SYNC_PULL_VIEW_RULE default for nodes that have a service actor to
// evaluate it for (P4-7), unless the policy opts out with SkipViewRule.
func (m *Module) pullRuleOn(vw *viewer, p *policy) bool {
	return p.PullViewRule || (envFlag(EnvPullViewRule) && !p.SkipViewRule && vw.actor != nil)
}

// visibleForNode is the single view check of the rows a node pulls: what the
// service actor of the node may see of rec (view rule when pullRuleOn, and the
// fieldperm hidden set).
func (m *Module) visibleForNode(vw *viewer, p *policy, rec *core.Record) (*viewResult, error) {
	return vw.view(rec, p, m.pullRuleOn(vw, p))
}

// invisibleNotice turns pc into the answer for a record the node's actor may
// not view: an eviction when the policy says so, else a notice without data.
// It never deletes the local copy on its own (P4-6).
func (m *Module) invisibleNotice(pc proto.PullChange, p *policy) proto.PullChange {
	pc.Patch, pc.Hash = json.RawMessage(`{}`), ""
	pc.Code = revertInvisible
	if p.EvictInvisible || envFlag(EnvEvictInvisible) {
		return evictChange(pc) // the PR6 eviction: a local delete without tombstone
	}
	pc.Notice = proto.NoticeInvisible
	return pc
}

// fieldClockWire returns the field clocks of record for the wire (field-merge
// collections only): those of the patch fields, or all of them when patch is nil.
func (m *Module) fieldClockWire(db dbx.Builder, col *core.Collection, p *policy, id string, patch map[string]any) map[string]string {
	if p.Strategy != StratFieldMerge {
		return nil
	}
	clocks, err := readFieldClocks(db, col.Id, id)
	if err != nil || len(clocks) == 0 {
		return nil
	}
	out := map[string]string{}
	for f, h := range clocks {
		if _, ok := patch[f]; patch == nil || ok {
			out[f] = h.String()
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
