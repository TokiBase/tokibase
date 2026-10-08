//go:build !no_sync

package client

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/sync/hlc"
)

// parkedRow is an unpushed local change that a bootstrap parked.
type parkedRow struct {
	Seq     int64  `db:"origin_seq"`
	HLC     int64  `db:"hlc"`
	Coll    string `db:"collection"`
	Record  string `db:"record"`
	Op      string `db:"op"`
	Patch   string `db:"patch"`
	Actor   string `db:"actor"`
	Created string `db:"created"`
}

// rebaseParked replays the parked local changes on the data the snapshot and
// the log delivered, as fresh local changes (new HLC, base = the new record
// clock), in their original order. A change older than the retention, or one
// whose record is gone, becomes an `orphaned` conflict instead (§3.9). So does
// a change whose fields the hub edited LATER than the change was made (the
// record clock, or the field clock of a field-merge collection, is newer than
// original HLC of the parked change): the replay must never silently override
// a newer edit, which is what last-write-wins on the original times says too.
// A replay that goes through keeps the ORIGINAL HLC of its change (change row,
// record clock and field clocks are set back to it), so it also loses against
// edits other nodes made after it. Counter and set fields are commutative and
// always apply. Each
// parked row turns into a filler (`code = rebased`) in the same transaction, so
// the origin_seq sequence the hub tracks stays contiguous and a crash neither
// loses nor repeats a change.
func (c *Client) rebaseParked(ctx context.Context) (rebased, orphaned int, err error) {
	var rows []parkedRow
	if err := c.o.App.DB().NewQuery(`SELECT origin_seq, hlc, collection, record, op, patch, actor, created FROM _changes
  WHERE node={:n} AND status={:s} ORDER BY origin_seq`).Bind(dbx.Params{"n": c.nodeID, "s": StatusRebase}).All(&rows); err != nil {
		return 0, 0, err
	}
	cut := c.wallNow().Add(-c.retention())
	base := map[string]recClock{}
	for _, r := range rows {
		if err := ctx.Err(); err != nil {
			return rebased, orphaned, err
		}
		var note string
		err := c.o.App.RunInTransaction(func(tx kernel.App) error {
			var rerr error
			if note, rerr = c.replayLocal(tx, r, cut, base); rerr != nil {
				return rerr
			}
			if note != "" {
				writeConflictRow(tx, conflictRow{Collection: r.Coll, Record: r.Record, Change: c.nodeID + ":" + itoa(r.Seq), Node: c.nodeID,
					Actor: r.Actor, Kind: "orphaned", Resolution: "orphaned", Status: "open", Incoming: patchOf(r.Patch), Note: note})
			}
			_, rerr = tx.NonconcurrentDB().NewQuery("UPDATE _changes SET status='local', op='u', patch='{}', hash=NULL, base_hlc=0, code={:c}, part_old='', part_new='' WHERE node={:n} AND origin_seq={:s} AND status={:r}").
				Bind(dbx.Params{"c": CodeRebased, "n": c.nodeID, "s": r.Seq, "r": StatusRebase}).Execute()
			return rerr
		})
		if err != nil {
			return rebased, orphaned, fmt.Errorf("sync: rebase of local change %s:%d failed: %w", c.nodeID, r.Seq, err)
		}
		if note != "" {
			orphaned++
		} else {
			rebased++
		}
	}
	if len(rows) > 0 && c.o.Logger != nil {
		c.o.Logger.Info("sync: local changes rebased after the snapshot", "rebased", rebased, "orphaned", orphaned)
	}
	return rebased, orphaned, nil
}

// recClock is the clock state of a record as the hub data delivered it.
type recClock struct {
	hlc    int64
	clocks map[string]hlc.HLC
}

// parseClocks decodes `_sync_meta.fields` ({"field": "<hlc>"}).
func parseClocks(raw string) map[string]hlc.HLC {
	out := map[string]hlc.HLC{}
	var m map[string]string
	if raw == "" || json.Unmarshal([]byte(raw), &m) != nil {
		return out
	}
	for f, s := range m {
		if h, err := hlc.Parse(s); err == nil {
			out[f] = h
		}
	}
	return out
}

func patchOf(raw string) map[string]any {
	var p map[string]any
	_ = json.Unmarshal([]byte(raw), &p)
	return p
}

// replayLocal applies one parked change to the current data as a new local
// change. It returns a note when the change cannot be kept (the orphaned reason).
func (c *Client) replayLocal(tx kernel.App, r parkedRow, cut time.Time, base map[string]recClock) (string, error) {
	col, err := tx.FindCachedCollectionByNameOrId(r.Coll)
	if err != nil {
		return "the collection no longer exists on this node", nil
	}
	pv := c.o.Backend.Policy(col)
	if pv == nil {
		return "the collection is no longer replicated on this node", nil
	}
	if t, perr := time.Parse(dateLayout, r.Created); perr == nil && t.Before(cut) {
		return "the change is older than the retention (" + c.retention().String() + ")", nil
	}
	db := tx.NonconcurrentDB()
	var tomb int
	_ = db.NewQuery("SELECT COUNT(*) FROM _sync_tombstones WHERE collection={:c} AND record={:r}").
		Bind(dbx.Params{"c": col.Id, "r": r.Record}).Row(&tomb)
	rec, _ := tx.FindRecordById(col.Id, r.Record)

	var prevMax int64
	_ = db.NewQuery("SELECT COALESCE(MAX(origin_seq),0) FROM _changes WHERE node={:n}").Bind(dbx.Params{"n": c.nodeID}).Row(&prevMax)
	// the replay keeps the HLC of the change it replays (never newer): a fresh one
	// would let an old offline edit beat edits other nodes made after it
	restamp := func(plain []string, floor int64, floors map[string]hlc.HLC) {
		if r.HLC <= 0 {
			return
		}
		_, _ = db.NewQuery("UPDATE _changes SET hlc={:o} WHERE node={:n} AND origin_seq>{:m} AND hlc>{:o}").
			Bind(dbx.Params{"o": r.HLC, "n": c.nodeID, "m": prevMax}).Execute()
		// the record clock never goes below what the hub data had (a counter-only
		// replay of an old change must not move it back)
		_, _ = db.NewQuery("UPDATE _sync_meta SET hlc={:o} WHERE collection={:c} AND record={:r} AND hlc>{:o}").
			Bind(dbx.Params{"o": max(r.HLC, floor), "c": col.Id, "r": r.Record}).Execute()
		_, _ = db.NewQuery("UPDATE _sync_tombstones SET hlc={:o} WHERE collection={:c} AND record={:r} AND hlc>{:o}").
			Bind(dbx.Params{"o": r.HLC, "c": col.Id, "r": r.Record}).Execute()
		if len(plain) == 0 {
			return
		}
		var fraw string
		if db.NewQuery("SELECT COALESCE(fields,'') FROM _sync_meta WHERE collection={:c} AND record={:r}").
			Bind(dbx.Params{"c": col.Id, "r": r.Record}).Row(&fraw) != nil || fraw == "" || fraw == "{}" {
			return
		}
		cl := parseClocks(fraw)
		moved := false
		for _, f := range plain {
			if h, ok := cl[f]; ok && int64(h) > max(r.HLC, int64(floors[f])) {
				cl[f], moved = hlc.HLC(max(r.HLC, int64(floors[f]))), true
			}
		}
		if moved {
			out := make(map[string]string, len(cl))
			for f, h := range cl {
				out[f] = h.String()
			}
			b, _ := json.Marshal(out)
			_, _ = db.NewQuery("UPDATE _sync_meta SET fields={:f} WHERE collection={:c} AND record={:r}").
				Bind(dbx.Params{"f": string(b), "c": col.Id, "r": r.Record}).Execute()
		}
	}
	keepActor := func() {
		if r.Actor != "" && r.Actor != "node" {
			_, _ = db.NewQuery("UPDATE _changes SET actor={:a} WHERE node={:n} AND origin_seq>{:m} AND actor='node'").
				Bind(dbx.Params{"a": r.Actor, "n": c.nodeID, "m": prevMax}).Execute()
		}
	}

	// the clocks the hub data had BEFORE the first replay of this record: a replay
	// raises them, and a second parked change of the same record must not
	// conflict with the first one
	var recHLC int64
	var clocks map[string]hlc.HLC
	if rec != nil {
		key := col.Id + "/" + r.Record
		bc, seen := base[key]
		if !seen {
			var fraw string
			if err := db.NewQuery("SELECT hlc, COALESCE(fields,'') FROM _sync_meta WHERE collection={:c} AND record={:r}").
				Bind(dbx.Params{"c": col.Id, "r": r.Record}).Row(&bc.hlc, &fraw); err == nil {
				bc.clocks = parseClocks(fraw)
			}
			base[key] = bc
		}
		recHLC, clocks = bc.hlc, bc.clocks
	}
	switch r.Op {
	case "d":
		if rec == nil {
			return "", nil // already gone: the hub deleted it too
		}
		if recHLC > r.HLC {
			return "the record was edited on the hub after this delete was made", nil
		}
		if err := tx.Delete(rec); err != nil {
			return "the delete failed: " + err.Error(), nil
		}
		keepActor()
		restamp(nil, recHLC, nil)
		return "", nil
	case "c", "u":
	default:
		return "", nil // purge requests come from the hub only
	}

	if tomb > 0 {
		return "the record was deleted on the hub", nil
	}
	isNew := rec == nil
	if isNew {
		if r.Op == "u" {
			return "the record no longer exists on the hub", nil
		}
		rec = core.NewRecord(col)
		rec.Set("id", r.Record)
	}
	patch := patchOf(r.Patch)
	changed := isNew
	var newer []string // plain fields the hub edited after this change was made
	for name, raw := range patch {
		f, ok := isAllowedField(col, pv, name)
		if !ok || f.Type() == kernel.FieldTypeAutodate {
			continue // autodate fields are regenerated by the new write
		}
		if !isNew && pv.Types[name] != "counter" && pv.Types[name] != "set" {
			fc := recHLC
			if h, has := clocks[name]; has {
				fc = int64(h)
			}
			if fc > r.HLC {
				newer = append(newer, name)
				delete(patch, name)
				continue
			}
		}
		var val any
		switch pv.Types[name] {
		case "counter":
			switch x := raw.(type) {
			case map[string]any:
				d, _ := x["$inc"].(float64)
				val = rec.GetFloat(name) + d
			case float64:
				val = x // a create carries the absolute value
			default:
				continue
			}
		case "set":
			x, ok := raw.(map[string]any)
			if !ok {
				if list, isList := raw.([]any); isList {
					val = list
					break
				}
				continue
			}
			add, _ := x["$add"].([]any)
			rm, _ := x["$rm"].([]any)
			var cur []any
			_ = json.Unmarshal([]byte(export(rec, f)), &cur)
			p := &pending{sets: map[string][]setOp{name: {{add: add, rm: rm}}}}
			val = p.applySet(name, cur)
		default:
			val = raw
		}
		if !isNew && export(rec, f) == canon(val) {
			continue
		}
		rec.Set(name, val)
		changed = true
	}
	if len(newer) > 0 {
		sort.Strings(newer)
		note := "the hub edited " + strings.Join(newer, ", ") + " after this change was made; the hub value was kept"
		if !changed {
			return note, nil
		}
		writeConflictRow(tx, conflictRow{Collection: r.Coll, Record: r.Record, Change: c.nodeID + ":" + itoa(r.Seq), Node: c.nodeID,
			Actor: r.Actor, Kind: "orphaned", Resolution: "orphaned", Status: "open", Incoming: patchOf(r.Patch), Note: note})
	}
	if !changed {
		return "", nil // the snapshot already holds this state
	}
	if err := tx.SaveNoValidate(rec); err != nil {
		return "the change cannot be applied to the new data: " + err.Error(), nil
	}
	keepActor()
	var plain []string
	for name := range patch {
		if pv.Types[name] != "counter" && pv.Types[name] != "set" {
			plain = append(plain, name)
		}
	}
	restamp(plain, recHLC, clocks)
	return "", nil
}
