//go:build !no_sync

package sync

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/sync/hlc"
	"github.com/tokibase/tokibase/modules/sync/proto"
	"github.com/tokibase/tokibase/tools/types"
)

// Snapshot bootstrap, hub side (docs/SYNC_DESIGN.md §3.9).
//
// A snapshot is "fuzzy": POST /api/sync/snapshot only records the hub head
// (start_seq) and hands out an id; GET /api/sync/snapshot then reads the
// records page by page with plain reads (no long transaction). The spoke
// pulls from start_seq afterwards, and a change that is already part of a page
// is a no-op when it is applied again.

const (
	// SnapshotTTL is how long a snapshot id is accepted.
	SnapshotTTL = 24 * time.Hour

	// snapshotByteBudget stops a page that grows beyond what the client reads (8 MiB).
	snapshotByteBudget = 4 << 20

	snapshotInfo = "toki_sync/snapshot/v1"

	// AuditSnapshot is emitted when a node starts a snapshot.
	AuditSnapshot = "sync.snapshot"
	// AuditRebootstrap is emitted when an operator marks a node for re-bootstrap.
	AuditRebootstrap = "sync.node.rebootstrap"
)

// snapClaims is the content of a snapshot id. The id is a signed token (HMAC
// with a key derived from the hub secret), so the hub keeps no state per
// snapshot and the id survives a restart.
type snapClaims struct {
	Node  string `json:"n"`
	Seq   int64  `json:"s"`
	Epoch string `json:"e"`
	Exp   int64  `json:"x"`
	Nonce string `json:"i"`
}

func (m *Module) snapMAC(payload string) string {
	h := hmac.New(sha256.New, m.hub.secret)
	h.Write([]byte(snapshotInfo + "." + payload))
	return hex.EncodeToString(h.Sum(nil))
}

func (m *Module) mintSnapshotID(node string, seq int64, now time.Time) (string, time.Time, error) {
	nonce, err := randHex(6)
	if err != nil {
		return "", time.Time{}, err
	}
	exp := now.Add(SnapshotTTL)
	raw, _ := json.Marshal(snapClaims{Node: node, Seq: seq, Epoch: m.hub.epoch, Exp: exp.Unix(), Nonce: nonce})
	p := base64.RawURLEncoding.EncodeToString(raw)
	return p + "." + m.snapMAC(p), exp, nil
}

// parseSnapshotID verifies id for node: signature, expiry, and the hub epoch it
// was issued under (a restored hub has a new epoch, its old snapshots are void).
func (m *Module) parseSnapshotID(id, node string) (*snapClaims, bool) {
	p, mac, ok := strings.Cut(id, ".")
	if !ok || len(id) > 512 || !constEq(mac, m.snapMAC(p)) {
		return nil, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil {
		return nil, false
	}
	var c snapClaims
	if json.Unmarshal(raw, &c) != nil || c.Node != node || c.Epoch != m.hub.epoch || m.now().Unix() >= c.Exp {
		return nil, false
	}
	return &c, true
}

// pullable returns the policies a node can pull: direction both or pull, with an
// existing collection, each once, ordered by (order, name).
func (c *policyCache) pullable() ([]*policy, error) {
	rows, err := c.load()
	if err != nil {
		return nil, err
	}
	var out []*policy
	for k, p := range rows {
		if p.ColID != "" && k == p.ColID && (p.Direction == DirBoth || p.Direction == DirPull) {
			out = append(out, p)
		}
	}
	names := map[string]string{}
	for k, p := range rows {
		if k != p.ColID && p.ColID != "" {
			if n, ok := names[p.ColID]; !ok || len(k) < len(n) {
				names[p.ColID] = k
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Order != out[j].Order {
			return out[i].Order < out[j].Order
		}
		return names[out[i].ColID] < names[out[j].ColID]
	})
	return out, nil
}

// snapshotScope lists the collections a node receives in a snapshot.
func (m *Module) snapshotScope(app kernel.App) ([]*core.Collection, []*policy, error) {
	ps, err := m.pol.pullable()
	if err != nil {
		return nil, nil, err
	}
	var cols []*core.Collection
	var pols []*policy
	for _, p := range ps {
		col, err := app.FindCachedCollectionByNameOrId(p.ColID)
		if err != nil || !eligible(col) {
			continue
		}
		cols = append(cols, col)
		pols = append(pols, p)
	}
	return cols, pols, nil
}

// snapshotStartHandler is POST /api/sync/snapshot.
func (m *Module) snapshotStartHandler(e *core.RequestEvent) error {
	nodeID := NodeFrom(e)
	start := m.headSeq() // before any page is read
	cols, _, err := m.snapshotScope(e.App)
	if err != nil {
		return err
	}
	now := m.now()
	id, exp, err := m.mintSnapshotID(nodeID, start, now)
	if err != nil {
		return err
	}
	m.sentOnce.Do(m.initSentLegacy)
	// a bootstrap starts the node over: the pages below fill `_sync_sent` with what
	// it receives, so the node is no longer "legacy" (sent.go)
	db := m.app.NonconcurrentDB()
	_, _ = db.NewQuery("DELETE FROM _sync_sent WHERE node={:n}").Bind(dbx.Params{"n": nodeID}).Execute()
	_, _ = db.NewQuery("DELETE FROM _sync_state WHERE key={:k}").Bind(dbx.Params{"k": "sent_legacy:" + nodeID}).Execute()
	resp := proto.SnapshotStart{
		SnapshotID: id, StartSeq: start, ServerTime: now.UTC().Format(proto.TimeLayout),
		Expires: exp.UTC().Format(proto.TimeLayout), HubEpoch: m.hub.epoch,
		Schema: []json.RawMessage{}, Policies: m.handshakePolicies(), Collections: []proto.SnapshotCollection{},
	}
	for i, col := range cols {
		raw, err := json.Marshal(col) // secrets of auth collections are redacted by the marshaler
		if err != nil {
			return err
		}
		resp.Schema = append(resp.Schema, raw)
		resp.Collections = append(resp.Collections, proto.SnapshotCollection{ID: col.Id, Name: col.Name, Order: i})
	}
	emit(AuditSnapshot, NodesCollection, nodeID, map[string]any{"node": nodeID, "start_seq": start, "collections": len(cols), "ip": e.RealIP()})
	return e.JSON(http.StatusOK, resp)
}

// snapshotPageHandler is GET /api/sync/snapshot?id&collection&after&limit.
func (m *Module) snapshotPageHandler(e *core.RequestEvent) error {
	nodeID := NodeFrom(e)
	q := e.Request.URL.Query()
	limit := int64(proto.SnapshotMaxPage)
	if s := q.Get("limit"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n < 0 {
			return syncErr(e, http.StatusBadRequest, proto.CodeBadRequest, "limit must be a non-negative integer", nil)
		}
		if n > 0 {
			limit = min(n, proto.SnapshotMaxPage)
		}
	}
	if _, ok := m.parseSnapshotID(q.Get("id"), nodeID); !ok {
		return syncErr(e, http.StatusGone, proto.CodeSnapshotExpired, "The snapshot id is invalid or expired; start a new snapshot.", nil)
	}
	cols, pols, err := m.snapshotScope(e.App)
	if err != nil {
		return err
	}
	ref := q.Get("collection")
	for i, col := range cols {
		if col.Id != ref {
			continue
		}
		page, err := m.buildSnapshotPage(e.App, nodeID, col, pols[i], q.Get("after"), int(limit))
		if err != nil {
			e.App.Logger().Error("sync: snapshot page failed", "node", nodeID, "collection", col.Name, "error", err)
			return syncErr(e, http.StatusInternalServerError, "sync_internal", "snapshot page failed", nil)
		}
		return e.JSON(http.StatusOK, page)
	}
	return syncErr(e, http.StatusBadRequest, proto.CodeBadRequest, "unknown collection for this snapshot", nil)
}

// recordScope is the single filter that decides what a node may receive of a
// record (partition, view rule, hidden fields). The snapshot uses it; pull
// applies the same rules row by row in pullChange.
func (m *Module) recordScope(v *viewer, rec *core.Record, p *policy) (bool, map[string]struct{}, error) {
	if !v.inPartition(rec, p) {
		return false, nil, nil
	}
	vr, err := m.visibleForNode(v, p, rec)
	if err != nil {
		return false, nil, err
	}
	return vr.visible, vr.hidden, nil
}

// buildSnapshotPage reads one page of col after the record id `after`. Records
// and tombstones are paged along the same id axis: the page covers the ids in
// (after, next], and the tombstones in that interval travel with it.
func (m *Module) buildSnapshotPage(app kernel.App, nodeID string, col *core.Collection, p *policy, after string, limit int) (*proto.SnapshotPage, error) {
	vw := newViewer(app, nodeID, serviceActor(app, nodeID))
	fields := syncedFields(col, p)
	page := &proto.SnapshotPage{Records: []proto.SnapshotRecord{}, Tombstones: []proto.SnapshotTombstone{}, Next: after}

	_, hasPart := vw.nodePartition(p)
	empty := p.PartField != "" && !hasPart // the node has no such parameter: it gets no records
	scanned := after
	recMore := false
	size := 0
	var ids []string
	if !empty {
		for !recMore && len(page.Records) < limit {
			var batch []*core.Record
			q := app.RecordQuery(col).OrderBy("id ASC").Limit(int64(limit))
			if scanned != "" {
				q = q.AndWhere(dbx.NewExp("id > {:a}", dbx.Params{"a": scanned}))
			}
			if err := q.All(&batch); err != nil {
				return nil, err
			}
			for _, rec := range batch {
				scanned = rec.Id
				ok, hidden, err := m.recordScope(vw, rec, p)
				if err != nil {
					return nil, err
				}
				if ok {
					vals, err := fieldValues(rec, fields, p.Types)
					if err != nil {
						return nil, err
					}
					for name := range hidden {
						delete(vals, name)
					}
					sr := proto.SnapshotRecord{ID: rec.Id, Data: vals, Node: m.hub.id, HLC: hlc.HLC(0).String()}
					sr.Hash = hex.EncodeToString(canonicalHash(col.Id, rec.Id, vals))
					page.Records = append(page.Records, sr)
					ids = append(ids, rec.Id)
					if b, err := json.Marshal(vals); err == nil {
						size += len(b) + 200
					}
				}
				if len(page.Records) >= limit || size >= snapshotByteBudget {
					recMore = true
					break
				}
			}
			if len(batch) < limit {
				break
			}
		}
	}
	upper := "" // "" = no upper bound
	if recMore {
		upper = scanned
	}
	// record clocks of the scanned interval
	if len(page.Records) > 0 {
		if err := m.fillSnapshotMeta(app, col, p, after, scanned, page.Records); err != nil {
			return nil, err
		}
	}

	// tombstones in (after, upper]
	tq := "SELECT record, kind, hlc, node, created FROM _sync_tombstones WHERE collection={:c} AND record > {:a}"
	params := dbx.Params{"c": col.Id, "a": after, "lim": limit + 1}
	if upper != "" {
		tq += " AND record <= {:u}"
		params["u"] = upper
	}
	scoped := p.PartField != "" || m.pullRuleOn(vw, p)
	if scoped {
		// a deleted record can not be checked against the partition or the view
		// rule of the node: its id, clock and origin must not leak (review P56-3)
		tq += " AND 0"
	} else {
		tq += " AND (kind='legal' OR created >= {:cut})"
		params["cut"] = cutoff(m.now(), retention())
	}
	tq += " ORDER BY record LIMIT {:lim}"
	var trows []struct {
		Record  string `db:"record"`
		Kind    string `db:"kind"`
		HLC     int64  `db:"hlc"`
		Node    string `db:"node"`
		Created string `db:"created"`
	}
	if err := app.DB().NewQuery(tq).Bind(params).All(&trows); err != nil {
		return nil, err
	}
	tombMore := false
	if len(trows) > limit {
		trows = trows[:limit]
		tombMore = true
		upper = trows[len(trows)-1].Record
	}
	for _, t := range trows {
		page.Tombstones = append(page.Tombstones, proto.SnapshotTombstone{
			Record: t.Record, Kind: t.Kind, HLC: hlc.HLC(t.HLC).String(), Node: t.Node, Created: t.Created,
		})
	}
	if tombMore {
		// cut the records at the tombstone boundary; the rest comes on the next page
		keep := page.Records[:0]
		for _, r := range page.Records {
			if r.ID <= upper {
				keep = append(keep, r)
			}
		}
		page.Records = keep
	}
	if m.sentTracked(vw, col, p) {
		// the node now holds these records: later evict/delete rows may name them
		for _, r := range page.Records {
			m.markSent(nodeID, col.Id, r.ID)
		}
	}
	page.More = recMore || tombMore
	switch {
	case page.More:
		page.Next = upper
	case scanned != "":
		page.Next = scanned
	}
	return page, nil
}

// fillSnapshotMeta adds hlc/node/field clocks from `_sync_meta` to records
// (ids ascending, all in (lo, hi]). A record without a meta row (data from before
// sync was enabled) keeps hlc 0 and the hub as origin.
func (m *Module) fillSnapshotMeta(app kernel.App, col *core.Collection, p *policy, lo, hi string, recs []proto.SnapshotRecord) error {
	var rows []struct {
		Record string `db:"record"`
		HLC    int64  `db:"hlc"`
		Node   string `db:"node"`
		Fields string `db:"fields"`
	}
	if err := app.DB().NewQuery("SELECT record, hlc, node, fields FROM _sync_meta WHERE collection={:c} AND record > {:lo} AND record <= {:hi}").
		Bind(dbx.Params{"c": col.Id, "lo": lo, "hi": hi}).All(&rows); err != nil {
		return err
	}
	by := make(map[string]int, len(rows))
	for i, r := range rows {
		by[r.Record] = i
	}
	for i := range recs {
		j, ok := by[recs[i].ID]
		if !ok {
			continue
		}
		r := rows[j]
		recs[i].HLC, recs[i].Node = hlc.HLC(r.HLC).String(), r.Node
		if p.Strategy == StratFieldMerge {
			if cl := parseFieldClocks(r.Fields); len(cl) > 0 {
				recs[i].Fields = make(map[string]string, len(cl))
				for f, h := range cl {
					recs[i].Fields[f] = h.String()
				}
			}
		}
	}
	return nil
}

// completeSnapshot handles the snapshot_id of an ack: the node finished its
// bootstrap, so it is active again and counts as having pulled start_seq.
func (m *Module) completeSnapshot(nodeID, id string) (bool, error) {
	c, ok := m.parseSnapshotID(id, nodeID)
	if !ok {
		return false, nil
	}
	_, err := m.app.NonconcurrentDB().NewQuery("UPDATE " + NodesCollection + " SET status={:a}, pulled_seq={:s}, updated={:t} WHERE id={:id} AND status IN ({:s1},{:s2},{:a})").
		Bind(dbx.Params{"a": NodeActive, "s": min(c.Seq, m.headSeq()), "t": m.created(), "id": nodeID, "s1": NodeStale, "s2": NodeRebootstrap}).Execute()
	return err == nil, err
}

// MarkRebootstrap flags a node so that its next handshake answers
// `rebootstrap: true` (`toki sync rebootstrap <node>`). Pending and revoked
// nodes cannot be marked.
func MarkRebootstrap(app core.App, ref string) (*core.Record, error) {
	rec, err := FindNode(app, ref)
	if err != nil {
		return nil, fmt.Errorf("node %q not found", ref)
	}
	switch rec.GetString("status") {
	case NodePending, NodeRevoked:
		return nil, fmt.Errorf("node %q is %s", ref, rec.GetString("status"))
	}
	name := rec.GetString("name")
	if _, err := app.NonconcurrentDB().NewQuery("UPDATE " + NodesCollection + " SET status={:r}, updated={:t} WHERE name={:n} AND status IN ({:a},{:s},{:r})").
		Bind(dbx.Params{"r": NodeRebootstrap, "a": NodeActive, "s": NodeStale, "t": types.NowDateTime().String(), "n": name}).Execute(); err != nil {
		return nil, err
	}
	cur, err := FindNode(app, name)
	if err != nil {
		return nil, err
	}
	if cur.GetString("status") != NodeRebootstrap {
		return nil, fmt.Errorf("node %q was not marked (status %s)", ref, cur.GetString("status"))
	}
	emit(AuditRebootstrap, NodesCollection, cur.Id, map[string]any{"name": name, "cli": true})
	return cur, nil
}
