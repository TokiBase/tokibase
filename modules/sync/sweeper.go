//go:build !no_sync

package sync

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/sync/hlc"
	"github.com/tokibase/tokibase/modules/sync/proto"
)

// ctPrefix is the prefix of a stored ciphertext (modules/crypto, not imported:
// a seam by value). A spoke finds the key versions of its queued changes by it.
const ctPrefix = "tkc1:"

var _ kernel.SyncSweeper = (*Module)(nil)

// SyncRole implements kernel.SyncSweeper.
func (m *Module) SyncRole() string { return string(m.role) }

// IsSynced implements kernel.SyncSweeper. A policy that cannot be loaded counts
// as synced (the caller then refuses, fail closed).
func (m *Module) IsSynced(collectionId string) bool {
	col := m.collectionOf(collectionId)
	if col == nil {
		return false
	}
	p, err := m.pol.For(col)
	return err != nil || p != nil
}

// RecordSweep implements kernel.SyncSweeper (hub). A bulk rewrite of encrypted
// values (`toki crypto enable|disable|rotate`) writes the table with raw SQL, so
// it would leave no trace in the change log and the nodes would keep the old
// values (plaintext after an enable, stale ciphertext after a rotation) for
// ever. This writes the change rows the model save would have written: one `u`
// per record with the stored values of the rewritten fields, a new HLC, and
// the record hash of the stored row. A record without a `_sync_meta` row (never
// captured) is sent whole as a create. The rows are not grouped into an atomic
// group: they are independent.
func (m *Module) RecordSweep(tx kernel.App, collectionId string, changed map[string][]string) error {
	if m.role != RoleHub {
		return fmt.Errorf("sync: only the hub records a key sweep (this node is a %s)", m.role)
	}
	if len(changed) == 0 {
		return nil
	}
	col, err := tx.FindCachedCollectionByNameOrId(collectionId)
	if err != nil || col == nil {
		return fmt.Errorf("sync: collection %q not found", collectionId)
	}
	p, err := m.pol.For(col)
	if err != nil {
		return err
	}
	if p == nil {
		return nil
	}
	fields := syncedFields(col, p)
	inSet := make(map[string]struct{}, len(fields))
	for _, f := range fields {
		inSet[f.GetName()] = struct{}{}
	}
	ids := make([]string, 0, len(changed))
	for id := range changed {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	db := tx.NonconcurrentDB()
	node := m.NodeID()
	for _, id := range ids {
		rec, err := tx.FindRecordById(col.Id, id)
		if err != nil || rec == nil {
			continue // deleted meanwhile: the delete is its own change
		}
		vals, err := fieldValues(rec, fields, p.Types)
		if err != nil {
			return err
		}
		hash := canonicalHash(col.Id, id, vals)
		patch := map[string]any{}
		for _, f := range changed[id] {
			if _, ok := inSet[f]; !ok {
				continue // withheld (strip), excluded or not synced
			}
			if t := p.Types[f]; t == TypeCounter || t == TypeSet {
				continue
			}
			patch[f] = vals[f]
		}
		if len(patch) == 0 {
			if err := refreshMetaHash(db, col.Id, id, hash); err != nil {
				return err
			}
			continue
		}
		op := OpUpdate
		baseHLC, _, hasMeta := readMeta(db, col.Id, id)
		if !hasMeta {
			op, patch, baseHLC = OpCreate, vals, 0
		}
		enc, err := encodePatch(patch)
		if err != nil {
			return err
		}
		h := int64(m.Clock().Now())
		part := partValue(rec, p)
		if err := m.insertChange(tx, &change{
			node: node, hlc: h, baseHLC: baseHLC, collection: col.Id, record: id, op: op,
			patch: enc, hash: hash, actor: ActorNode, partOld: part, partNew: part, ungrouped: true,
		}); err != nil {
			return err
		}
		if err := upsertMeta(db, col.Id, id, h, node, hash); err != nil {
			return err
		}
		if p.Strategy == StratFieldMerge {
			if err := bumpFieldClocks(db, col.Id, id, plainFields(p.Types, patch), hlc.HLC(h)); err != nil {
				return err
			}
		}
		if err := setMetaPart(db, col.Id, id, part); err != nil {
			return err
		}
	}
	return nil
}

// resendFields sends fields of every record of the collection again (a policy
// stopped withholding them).
func (m *Module) resendFields(collectionID string, fields []string) error {
	col := m.collectionOf(collectionID)
	if col == nil || len(fields) == 0 {
		return nil
	}
	for offset := 0; ; offset += 500 {
		recs, err := m.app.FindRecordsByFilter(col, "", "id", 500, offset)
		if err != nil {
			return err
		}
		if len(recs) == 0 {
			return nil
		}
		changed := make(map[string][]string, len(recs))
		for _, r := range recs {
			changed[r.Id] = fields
		}
		m.pol.invalidate()
		if err := m.app.RunInTransaction(func(tx kernel.App) error {
			return m.RecordSweep(tx, col.Id, changed)
		}); err != nil {
			return err
		}
		if len(recs) < 500 {
			return nil
		}
	}
}

// ----- key report (retire guard) -----

// keyReport is what the hub knows about the data keys of one node, per
// collection id: the newest version it handed to the node, and the versions
// the unsent changes of the node hold ciphertext of (reported at the handshake).
type keyReportEntry struct {
	Have    int    `json:"have"`
	Pending []int  `json:"pending,omitempty"`
	At      string `json:"at"`
}

// noteKeyReport stores the key report of a node (a targeted UPDATE, like touchNode).
func (m *Module) noteKeyReport(nodeID string, have map[string]int, pending map[string][]int) {
	rep := map[string]keyReportEntry{}
	at := m.now().UTC().Format(proto.TimeLayout)
	for col, v := range have {
		pend := append([]int(nil), pending[col]...)
		sort.Ints(pend)
		rep[col] = keyReportEntry{Have: v, Pending: pend, At: at}
	}
	b, err := json.Marshal(rep)
	if err != nil {
		return
	}
	if _, err := m.app.NonconcurrentDB().NewQuery("UPDATE " + NodesCollection + " SET key_report={:r} WHERE id={:id}").
		Bind(dbx.Params{"r": string(b), "id": nodeID}).Execute(); err != nil {
		m.app.Logger().Warn("sync: cannot store the key report of a node", "node", nodeID, "error", err)
	}
}

// RetireBlockers implements kernel.SyncSweeper: the live nodes that may still
// hold or write ciphertext of one of versions, as printable lines.
func (m *Module) RetireBlockers(collectionId string, versions []int) ([]string, error) {
	if m.role != RoleHub || len(versions) == 0 {
		return nil, nil
	}
	col := m.collectionOf(collectionId)
	if col == nil {
		return nil, nil
	}
	kp := kernel.SyncKeyProviderOf(m.app)
	nodes, err := m.app.FindAllRecords(NodesCollection)
	if err != nil {
		return nil, err
	}
	top := 0
	for _, v := range versions {
		top = max(top, v)
	}
	var out []string
	for _, n := range nodes {
		switch n.GetString("status") {
		case NodeRevoked, NodePending:
			continue
		}
		if kp != nil {
			ids, err := m.keyCollections(kp, n)
			if err != nil {
				return nil, err
			}
			needs := false
			for _, id := range ids {
				needs = needs || id == col.Id
			}
			if !needs {
				continue
			}
		}
		label := fmt.Sprintf("%s (%s)", n.Id, n.GetString("name"))
		var rep map[string]keyReportEntry
		if raw := rawJSON(n, "key_report"); raw != nil {
			_ = json.Unmarshal(raw, &rep)
		}
		e, ok := rep[col.Id]
		switch {
		case !ok:
			out = append(out, label+": has not reported its keys since the hub learned to track them (it must complete a handshake)")
		case e.Have <= top:
			out = append(out, fmt.Sprintf("%s: last handshake %s gave it key v%d only (it may still write v%d); it must handshake again", label, e.At, e.Have, e.Have))
		default:
			for _, v := range e.Pending {
				if slices.Contains(versions, v) {
					out = append(out, fmt.Sprintf("%s: still has unsent changes encrypted with key v%d (reported %s)", label, v, e.At))
					break
				}
			}
		}
	}
	return out, nil
}
