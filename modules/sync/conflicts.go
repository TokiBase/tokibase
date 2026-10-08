//go:build !no_sync

package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/pocketbase/dbx"
	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/sync/proto"
)

const storeKey = "__tokiSyncModule__"

// ModuleOf returns the module registered on app, or nil.
func ModuleOf(app core.App) *Module {
	m, _ := app.Store().Get(storeKey).(*Module)
	return m
}

// ConflictRow is a `_sync_conflicts` row as `toki sync conflicts --json` prints it.
type ConflictRow struct {
	ID         string          `json:"id"`
	Collection string          `json:"collection"` // name
	Record     string          `json:"record"`
	Change     string          `json:"change"`
	Node       string          `json:"node"`
	Actor      string          `json:"actor"`
	Kind       string          `json:"kind"`
	Strategy   string          `json:"strategy"`
	Resolution string          `json:"resolution"`
	Status     string          `json:"status"`
	Incoming   json.RawMessage `json:"incoming"`
	Current    json.RawMessage `json:"current"`
	ResolvedBy string          `json:"resolved_by,omitempty"`
	ResolvedAt string          `json:"resolved_at,omitempty"`
	Note       string          `json:"note,omitempty"`
	Created    string          `json:"created"`
}

func rawOf(r *core.Record, f string) json.RawMessage {
	b, err := json.Marshal(r.Get(f))
	if err != nil || len(b) == 0 || string(b) == "null" {
		return json.RawMessage(`{}`)
	}
	return b
}

func toConflictRow(app core.App, r *core.Record) ConflictRow {
	name := r.GetString("collection")
	if c, err := app.FindCachedCollectionByNameOrId(name); err == nil {
		name = c.Name
	}
	return ConflictRow{
		ID: r.Id, Collection: name, Record: r.GetString("record"), Change: r.GetString("change"), Node: r.GetString("node"),
		Actor: r.GetString("actor"), Kind: r.GetString("kind"), Strategy: r.GetString("strategy"),
		Resolution: r.GetString("resolution"), Status: r.GetString("status"),
		Incoming: rawOf(r, "incoming"), Current: rawOf(r, "current"),
		ResolvedBy: r.GetString("resolved_by"), ResolvedAt: r.GetDateTime("resolved_at").String(),
		Note: r.GetString("note"), Created: r.GetDateTime("created").String(),
	}
}

// ListConflicts returns the conflict rows, newest first. collection is a name
// or an id ("" = all); openOnly keeps the rows waiting for an admin.
func ListConflicts(app core.App, openOnly bool, collection string) ([]ConflictRow, error) {
	if !app.HasTable(ConflictsCollection) {
		return nil, nil
	}
	exprs := []dbx.Expression{}
	if openOnly {
		exprs = append(exprs, dbx.HashExp{"status": ConflictOpen})
	}
	if collection != "" {
		id := collection
		if c, err := app.FindCollectionByNameOrId(collection); err == nil {
			id = c.Id
		}
		exprs = append(exprs, dbx.HashExp{"collection": id})
	}
	var recs []*core.Record
	q := app.RecordQuery(ConflictsCollection).OrderBy("created DESC", "id DESC").Limit(1000)
	if len(exprs) > 0 {
		q = q.AndWhere(dbx.And(exprs...))
	}
	if err := q.All(&recs); err != nil {
		return nil, err
	}
	out := make([]ConflictRow, 0, len(recs))
	for _, r := range recs {
		out = append(out, toConflictRow(app, r))
	}
	return out, nil
}

// Take values of ResolveConflict.
const (
	TakeHub      = "hub"
	TakeIncoming = "incoming"
	TakePatch    = "patch"
)

// ResolveOptions selects how an open conflict is resolved.
type ResolveOptions struct {
	ID   string
	Take string         // hub | incoming | patch
	Data map[string]any // TakePatch: the patch to apply
	Note string
	By   string // recorded in resolved_by ("cli" by default)
}

// ResolveConflict resolves an open conflict on the hub.
//
//   - hub: nothing is written; the node receives the hub state (revert) so its
//     copy converges.
//   - incoming: the pushed patch is applied to the record (a parked change, or
//     the fields a field-merge review dropped), then the node gets the hub state.
//   - patch: Data is applied instead.
//
// The write is replayed as the original actor of the conflict (see adminWrite),
// captured like any other hub change and delivered to every node by pull.
func ResolveConflict(app core.App, o ResolveOptions) error {
	m := ModuleOf(app)
	if m == nil || m.role != RoleHub || !m.ready.Load() {
		return errors.New("resolving conflicts needs TOKI_SYNC_ROLE=hub")
	}
	return m.resolveConflict(o)
}

// resolveMember is one conflict of a resolution (a tx group has several).
type resolveMember struct {
	cr    *core.Record
	col   *core.Collection
	op    string // c, u or d: what the parked change did
	patch map[string]any
}

// parkedOp returns the op and tx group of the parked change a conflict row
// refers to ("u" and no group for every other conflict).
func (m *Module) parkedOp(cr *core.Record) (op, txID, node string, oseq int64) {
	op = OpUpdate
	ch := cr.GetString("change")
	i := strings.LastIndexByte(ch, ':')
	if i <= 0 || cr.GetString("resolution") != ResolutionParked {
		return
	}
	var row struct {
		Op string `db:"op"`
		Tx string `db:"tx"`
	}
	n, err := strconv.ParseInt(ch[i+1:], 10, 64)
	if err != nil {
		return
	}
	if m.app.DB().NewQuery("SELECT op, tx FROM _changes WHERE node={:n} AND origin_seq={:o} AND status='parked'").
		Bind(dbx.Params{"n": ch[:i], "o": n}).One(&row) != nil {
		return
	}
	return row.Op, row.Tx, ch[:i], n
}

func (m *Module) resolveConflict(o ResolveOptions) error {
	m.applyMu.Lock()
	defer m.applyMu.Unlock()
	cr, err := m.app.FindRecordById(ConflictsCollection, o.ID)
	if err != nil {
		return fmt.Errorf("conflict %q not found", o.ID)
	}
	if cr.GetString("status") != ConflictOpen {
		return fmt.Errorf("conflict %s is already %s", o.ID, cr.GetString("status"))
	}
	resolution := ResolutionRejected
	switch o.Take {
	case TakeHub:
	case TakeIncoming:
		resolution = ResolutionAccepted
	case TakePatch:
		if o.Data == nil {
			return errors.New("no patch given")
		}
		resolution = ResolutionAccepted
	default:
		return fmt.Errorf("unknown --take %q (want hub, incoming or a patch.json file)", o.Take)
	}

	// the conflict plus, for a parked tx group, every other parked member: a
	// transaction is accepted or refused as a whole
	crs := []*core.Record{cr}
	if op, txID, node, oseq := m.parkedOp(cr); txID != "" {
		if o.Take == TakePatch {
			return errors.New("a parked transaction group is resolved with --take hub or --take incoming (all members together)")
		}
		_ = op
		var seqs []int64
		if err := m.app.DB().NewQuery("SELECT origin_seq FROM _changes WHERE node={:n} AND tx={:t} AND status='parked' AND origin_seq!={:s} ORDER BY origin_seq").
			Bind(dbx.Params{"n": node, "t": txID, "s": oseq}).Column(&seqs); err != nil {
			return err
		}
		for _, sq := range seqs {
			other, err := m.app.FindFirstRecordByFilter(ConflictsCollection, "change={:c} && status='open'", dbx.Params{"c": node + ":" + strconv.FormatInt(sq, 10)})
			if err == nil && other != nil {
				crs = append(crs, other)
			}
		}
		sort.SliceStable(crs, func(i, j int) bool { return changeSeq(crs[i]) < changeSeq(crs[j]) })
	}

	members := make([]resolveMember, 0, len(crs))
	for _, c := range crs {
		col, err := m.app.FindCachedCollectionByNameOrId(c.GetString("collection"))
		if err != nil {
			return fmt.Errorf("collection %q no longer exists", c.GetString("collection"))
		}
		mb := resolveMember{cr: c, col: col}
		mb.op, _, _, _ = m.parkedOp(c)
		switch o.Take {
		case TakeIncoming:
			if mb.patch, err = m.storedIncoming(c); err != nil {
				return err
			}
		case TakePatch:
			mb.patch = o.Data
		}
		for k, v := range mb.patch {
			if s, ok := v.(string); ok && s == kernel.SensitiveMarker {
				return fmt.Errorf("field %q is redacted in the conflict row; resolve with --take <patch.json> that holds the real value", k)
			}
		}
		members = append(members, mb)
	}
	by := o.By
	if by == "" {
		by = "cli"
	}
	return m.app.RunInTransaction(func(tx kernel.App) error {
		// a parked change leaves `parked` here (P4-2): rejected with a revert when
		// the operator keeps the hub state, closed as accepted when its patch (or
		// Data) was applied as a normal hub write below
		parkedSeq := m.parkedSeqOf(tx, cr.GetString("change"))
		revertDone := false
		if parkedSeq > 0 && resolution == ResolutionRejected {
			if err := m.RejectParked(tx, parkedSeq, proto.CodeParkResolved, by); err != nil {
				return err
			}
			revertDone = true
		}
		if len(patch) > 0 {
			isCreate := false
			if parkedSeq > 0 {
				var op string
				_ = tx.NonconcurrentDB().NewQuery("SELECT op FROM _changes WHERE seq={:s}").Bind(dbx.Params{"s": parkedSeq}).Row(&op)
				isCreate = op == OpCreate
			}
			if err := m.adminWrite(tx, col, recID, node, cr.GetString("actor"), patch, isCreate); err != nil {
				return err
			}
		}
		if parkedSeq > 0 && !revertDone {
			if _, err := tx.NonconcurrentDB().NewQuery("UPDATE _changes SET status='rejected', code={:c} WHERE seq={:s} AND status='parked'").
				Bind(dbx.Params{"c": proto.CodeParkAccepted, "s": parkedSeq}).Execute(); err != nil {
				return err
			}
		}
		if node != "" && node != m.hub.id && !revertDone {
			if _, err := m.insertRevert(tx, node, col.Id, recID, nil); err != nil {
				return err
			}
		}
		cr2, err := tx.FindRecordById(ConflictsCollection, o.ID)
		if err != nil {
			return err
		}
		cr2.Set("status", ConflictResolved)
		cr2.Set("resolution", resolution)
		cr2.Set("resolved_by", by)
		cr2.Set("resolved_at", m.created())
		if o.Note != "" {
			cr2.Set("note", strings.TrimSpace(cr2.GetString("note")+" | "+o.Note))
		}
		return tx.SaveNoValidate(cr2)
	err = m.app.RunInTransaction(func(tx kernel.App) error {
		reverted := map[string]bool{}
		for _, mb := range members {
			c := mb.cr
			recID, node := c.GetString("record"), c.GetString("node")
			if o.Take != TakeHub {
				if err := m.adminWrite(tx, mb.col, mb.op, recID, node, c.GetString("actor"), mb.patch); err != nil {
					return err
				}
			}
			if k := node + "/" + mb.col.Id + "/" + recID; node != "" && node != m.hub.id && !reverted[k] {
				reverted[k] = true
				if _, err := m.insertRevert(tx, node, mb.col.Id, recID, nil); err != nil {
					return err
				}
			}
			cr2, err := tx.FindRecordById(ConflictsCollection, c.Id)
			if err != nil {
				return err
			}
			cr2.Set("status", ConflictResolved)
			cr2.Set("resolution", resolution)
			cr2.Set("resolved_by", by)
			cr2.Set("resolved_at", m.created())
			if o.Note != "" {
				cr2.Set("note", strings.TrimSpace(cr2.GetString("note")+" | "+o.Note))
			}
			if err := tx.SaveNoValidate(cr2); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, mb := range members {
		emit(AuditConflictResolve, mb.col.Id, mb.cr.GetString("record"), map[string]any{
			"conflict": mb.cr.Id, "take": o.Take, "resolution": resolution, "op": mb.op, "by": by, "node": mb.cr.GetString("node"),
		})
	}
	return nil
}

// changeSeq is the origin_seq of the change a conflict row refers to.
func changeSeq(cr *core.Record) int64 {
	ch := cr.GetString("change")
	n, _ := strconv.ParseInt(ch[strings.LastIndexByte(ch, ':')+1:], 10, 64)
	return n
}

// storedIncoming returns the real pushed patch of a conflict: the one kept in
// `_changes` (parked and rejected changes), else the conflict row's own
// `incoming` (the fields a field-merge dropped).
func (m *Module) storedIncoming(cr *core.Record) (map[string]any, error) {
	if ch := cr.GetString("change"); ch != "" && cr.GetString("resolution") == ResolutionParked {
		if i := strings.LastIndexByte(ch, ':'); i > 0 {
			var raw string
			err := m.app.DB().NewQuery("SELECT patch FROM _changes WHERE node={:n} AND origin_seq={:o}").
				Bind(dbx.Params{"n": ch[:i], "o": ch[i+1:]}).Row(&raw)
			if err == nil {
				var p map[string]any
				if json.Unmarshal([]byte(raw), &p) == nil {
					return p, nil
				}
			}
		}
	}
	var p map[string]any
	if err := json.Unmarshal(rawOf(cr, "incoming"), &p); err != nil {
		return nil, err
	}
	return p, nil
}

// adminWrite replays patch (plain values and counter/set operations) on an
// existing hub record AS THE ORIGINAL ACTOR of the conflict (docs/SYNC_DESIGN.md
// §5), through the same rule-checked path as a pushed change: collection rules,
// fieldperm and batchguard apply, and the write is an ordinary hub change that
// every node receives by pull. A grant that is no longer valid falls back to the
// service actor of the node; without one the resolution is refused (use
// --take hub, or fix the actor first).
func (m *Module) adminWrite(tx kernel.App, col *core.Collection, recID, nodeID, actor string, patch map[string]any, create bool) error {
	existing, _ := tx.FindRecordById(col.Id, recID)
	if existing == nil && !create {
func (m *Module) adminWrite(tx kernel.App, col *core.Collection, op, recID, nodeID, actor string, patch map[string]any) error {
	if op == OpUpdate && len(patch) == 0 {
		return nil
	}
	_, ferr := tx.FindRecordById(col.Id, recID)
	exists := ferr == nil
	switch {
	case op == OpCreate && exists:
		return fmt.Errorf("record %s already exists in %s", recID, col.Name)
	case op == OpCreate:
		if k := tombstoneKind(tx.NonconcurrentDB(), col.Id, recID); k != "" {
			return fmt.Errorf("record %s was deleted or purged (%s tombstone): it cannot be created again", recID, k)
		}
	case op == OpDelete && !exists:
		return nil // already gone: nothing to apply
	case !exists:
		return fmt.Errorf("record %s no longer exists in %s", recID, col.Name)
	}
	p, err := m.pol.For(col)
	if err != nil {
		return err
	}
	if p == nil {
		return fmt.Errorf("collection %s is not synced", col.Name)
	}
	allowed := map[string]core.Field{}
	for _, f := range syncedFields(col, p) {
		allowed[f.GetName()] = f
	}
	if existing != nil {
		if rj := validateTyped(p.Types, allowed, patch, false); rj != nil {
	if op != OpDelete {
		if rj := validateTyped(p.Types, allowed, patch, op == OpCreate); rj != nil {
			return rj
		}
	}
	body := map[string]any{}
	for _, name := range sortedKeys(patch) {
		f, ok := allowed[name]
		if !ok {
			return fmt.Errorf("field %q is not a synced field of %s", name, col.Name)
		}
		if f.Type() == kernel.FieldTypeAutodate {
			continue // regenerated by the write
		}
		if existing == nil {
			// a parked create holds the whole state: plain values, no operations
			if !isZeroValue(patch[name]) {
				body[name] = patch[name]
			}
			continue
		}
		if op, typed := opOf(p.Types, name, patch[name]); typed {
			for mk, mv := range typedModifiers(name, op) {
				body[mk] = mv
			}
		} else {
			body[name] = patch[name]
		}
	}
	if len(body) == 0 && op == OpUpdate {
		return nil
	}
	aid := actor
	if aid == ActorNode {
		aid = ""
	}
	var arec *core.Record
	if ac, rj := m.resolveActor(tx, nodeID, aid, nil); rj == nil {
		arec = ac.rec
	} else if sa := serviceActor(tx, nodeID); sa != nil {
		arec = sa
	} else {
		return fmt.Errorf("the original actor can no longer be resolved (%s) and node %s has no service actor", rj.msg, nodeID)
	}
	req := &core.InternalRequest{Method: http.MethodPatch, URL: "/api/collections/" + col.Id + "/records/" + recID, Body: body}
	if existing == nil {
		// accepting a parked create: the record is created with the id of the node
		body["id"] = recID
		req = &core.InternalRequest{Method: http.MethodPost, URL: "/api/collections/" + col.Id + "/records", Body: body}
	base := "/api/collections/" + col.Id + "/records"
	req := &core.InternalRequest{Method: http.MethodPatch, URL: base + "/" + recID, Body: body}
	switch op {
	case OpCreate:
		body["id"] = recID
		req = &core.InternalRequest{Method: http.MethodPost, URL: base, Body: body}
	case OpDelete:
		req = &core.InternalRequest{Method: http.MethodDelete, URL: base + "/" + recID}
	}
	_, err = apis.ReplayRecordRequestsFrom(context.Background(), core.AsApp(tx), arec, "",
		map[string]string{proto.HeaderSyncNode: nodeID}, []*core.InternalRequest{req})
	return classify(err)
}

// conflictsCommand is `toki sync conflicts`.
func conflictsCommand(app core.App) *cobra.Command {
	var openOnly, asJSON bool
	var coll, resolve, take, note string
	c := &cobra.Command{
		Use:   "conflicts [--open] [--collection c] [--json] | --resolve <id> --take hub|incoming|<patch.json> [--note ...]",
		Short: "List sync conflicts, or resolve an open one (hub)",
		Long: "Lists the rows of _sync_conflicts (on a spoke: what the hub answered to this node's changes).\n" +
			"--resolve (hub only) settles an open conflict: --take hub keeps the hub state, --take incoming applies the\n" +
			"pushed patch, --take <file.json> applies the patch in the file.",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			if resolve != "" {
				o := ResolveOptions{ID: resolve, Note: note}
				switch take {
				case "":
					return errors.New("--resolve needs --take hub|incoming|<patch.json>")
				case TakeHub, TakeIncoming:
					o.Take = take
				default:
					b, err := os.ReadFile(take)
					if err != nil {
						return fmt.Errorf("--take %q: not hub, incoming or a readable patch file: %w", take, err)
					}
					if err := json.Unmarshal(b, &o.Data); err != nil {
						return fmt.Errorf("%s: %w", take, err)
					}
					o.Take = TakePatch
				}
				if err := ResolveConflict(app, o); err != nil {
					return err
				}
				fmt.Fprintf(out, "conflict %s resolved (%s)\n", resolve, take)
				return nil
			}
			rows, err := ListConflicts(app, openOnly, coll)
			if err != nil {
				return err
			}
			if asJSON {
				b, _ := json.Marshal(rows)
				fmt.Fprintln(out, string(b))
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tSTATUS\tCOLLECTION\tRECORD\tKIND\tSTRATEGY\tRESOLUTION\tNODE\tCREATED")
			for _, r := range rows {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.ID, r.Status, r.Collection, r.Record, r.Kind,
					dash(r.Strategy), r.Resolution, dash(r.Node), r.Created)
			}
			return tw.Flush()
		},
	}
	c.Flags().BoolVar(&openOnly, "open", false, "only conflicts waiting for an admin")
	c.Flags().StringVar(&coll, "collection", "", "only this collection (name or id)")
	c.Flags().BoolVar(&asJSON, "json", false, "output JSON")
	c.Flags().StringVar(&resolve, "resolve", "", "resolve the open conflict with this id (hub)")
	c.Flags().StringVar(&take, "take", "", "hub | incoming | path of a patch.json")
	c.Flags().StringVar(&note, "note", "", "note stored with the resolution")
	return c
}

// parkedSeqOf returns the hub seq of the parked change a conflict refers to
// (change id "<node>:<origin_seq>"), 0 when it is not parked (any more).
func (m *Module) parkedSeqOf(tx kernel.App, change string) int64 {
	i := strings.LastIndexByte(change, ':')
	if i <= 0 {
		return 0
	}
	var seq int64
	if err := tx.NonconcurrentDB().NewQuery("SELECT seq FROM _changes WHERE node={:n} AND origin_seq={:o} AND status='parked'").
		Bind(dbx.Params{"n": change[:i], "o": change[i+1:]}).Row(&seq); err != nil {
		return 0
	}
	return seq
}
