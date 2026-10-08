//go:build !no_sync

package client

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/sync/hlc"
	"github.com/tokibase/tokibase/modules/sync/proto"
)

const dateLayout = "2006-01-02 15:04:05.000Z"

// applyPage applies one pull page and moves `pull_after` in the same
// transaction (docs/SYNC_DESIGN.md §6.2). A change that cannot be applied is
// reported and counted; it does not stop the page.
func (c *Client) applyPage(hubID string, pr *proto.PullResponse) (applied, failed int, err error) {
	var events []Event
	err = c.o.App.RunInTransaction(func(tx kernel.App) error {
		applied, failed, events = 0, 0, events[:0]
		for i := range pr.Changes {
			ok, aerr := c.applyChange(tx, &pr.Changes[i])
			if aerr != nil {
				failed++
				events = append(events, Event{Type: EventError, ID: pr.Changes[i].ID, Collection: pr.Changes[i].Collection,
					Record: pr.Changes[i].Record, Message: "apply failed: " + aerr.Error()})
				continue
			}
			if ok {
				applied++
			}
		}
		return setPullAfter(tx.NonconcurrentDB(), hubID, pr.Next)
	})
	if err != nil {
		return 0, 0, err
	}
	for _, ev := range events {
		c.emit(ev)
	}
	if applied > 0 {
		c.emit(Event{Type: EventApplied, Message: fmt.Sprintf("%d changes", applied)})
	}
	return applied, failed, nil
}

// applyChange applies one pulled change in tx (docs/SYNC_DESIGN.md §4.7). It
// reports whether anything was written.
func (c *Client) applyChange(tx kernel.App, ch *proto.PullChange) (bool, error) {
	col, err := tx.FindCachedCollectionByNameOrId(ch.Collection)
	if err != nil {
		return false, fmt.Errorf("unknown collection %q", ch.Collection)
	}
	pv := c.o.Backend.Policy(col)
	if pv == nil {
		return false, nil // not replicated on this node
	}
	if ch.Notice != "" {
		// informational row of the hub: no data, the local value stays
		return false, c.markReview(tx, col, ch)
	}
	h, err := hlc.Parse(ch.HLC)
	if err != nil {
		return false, err
	}
	if ch.Evict {
		return c.applyEvict(tx, col, ch)
	}
	if ch.Revert || ch.Op == "d" || ch.Op == "p" {
		// the hub decided about this record: it is no longer pending review
		if err := clearReview(tx.NonconcurrentDB(), col.Id, ch.Record); err != nil {
			return false, err
		}
	}
	if c.o.Clock != nil {
		c.o.Clock.Observe(h)
	}
	// Own changes come back too: the hub log is applied in hub order so that an
	// older foreign row that was pulled after our newer, already acked change is
	// followed by that change again. Equal values are no-ops.
	ctx := kernel.WithSyncOrigin(context.Background(), &kernel.SyncOrigin{
		Mode: kernel.SyncModePull, Node: ch.Node, HLC: uint64(h), ChangeID: ch.ID,
	})
	switch ch.Op {
	case "d", "p":
		return c.applyDelete(tx, ctx, col, ch, h)
	case "c", "u":
		return c.applyUpsert(tx, ctx, col, pv, ch, h)
	}
	return false, fmt.Errorf("unknown op %q", ch.Op)
}

func (c *Client) applyDelete(tx kernel.App, ctx context.Context, col *core.Collection, ch *proto.PullChange, h hlc.HLC) (bool, error) {
	rec, _ := tx.FindRecordById(col.Id, ch.Record)
	if rec != nil {
		return true, tx.DeleteWithContext(ctx, rec)
	}
	// already gone: make sure the id stays tombstoned
	_, err := tx.NonconcurrentDB().NewQuery(`INSERT INTO _sync_tombstones (collection, record, kind, hlc, node, actor, reason, created)
  VALUES ({:c}, {:r}, 'delete', {:h}, {:n}, '', '', {:t}) ON CONFLICT(collection, record) DO NOTHING`).
		Bind(dbx.Params{"c": col.Id, "r": ch.Record, "h": int64(h), "n": ch.Node, "t": time.Now().UTC().Format(dateLayout)}).Execute()
	return false, err
}

func isAllowedField(col *core.Collection, pv *PolicyView, name string) (core.Field, bool) {
	f := col.Fields.GetByName(name)
	if f == nil {
		return nil, false
	}
	switch {
	case name == kernel.FieldNameId, name == kernel.FieldNameTokenKey, name == kernel.FieldNamePassword,
		f.Type() == kernel.FieldTypeFile, f.Type() == kernel.FieldTypePassword, kernel.IsDerived(col.Id, name):
		return nil, false
	}
	if _, ex := pv.Exclude[name]; ex {
		return nil, false
	}
	return f, true
}

// export returns the normalized DB export value of a field (as capture sees it).
func export(rec *core.Record, f core.Field) string {
	var v any
	if dv, ok := f.(core.DriverValuer); ok {
		x, err := dv.DriverValue(rec)
		if err == nil {
			v = x
		}
	} else {
		v = rec.GetRaw(f.GetName())
	}
	b, _ := json.Marshal(v)
	var n any
	_ = json.Unmarshal(b, &n)
	nb, _ := json.Marshal(n)
	return string(nb)
}

func canon(v any) string {
	b, _ := json.Marshal(v)
	var n any
	_ = json.Unmarshal(b, &n)
	nb, _ := json.Marshal(n)
	return string(nb)
}

func (c *Client) applyUpsert(tx kernel.App, ctx context.Context, col *core.Collection, pv *PolicyView, ch *proto.PullChange, h hlc.HLC) (bool, error) {
	db := tx.NonconcurrentDB()
	var tomb int
	_ = db.NewQuery("SELECT COUNT(*) FROM _sync_tombstones WHERE collection={:c} AND record={:r}").
		Bind(dbx.Params{"c": col.Id, "r": ch.Record}).Row(&tomb)
	if tomb > 0 {
		return false, nil // deleted here (or purged); a delete wins
	}
	pend, err := loadPending(db, c.nodeID, col.Id, ch.Record)
	if err != nil {
		return false, err
	}
	if pend.hasDelete {
		return false, nil // our pending delete wins at the hub too
	}
	var patch map[string]any
	if err := json.Unmarshal(ch.Patch, &patch); err != nil {
		return false, err
	}
	rec, _ := tx.FindRecordById(col.Id, ch.Record)
	isNew := rec == nil
	if isNew {
		if ch.Op == "u" && !ch.Revert {
			return false, nil // an update for a record this node never had
		}
		rec = core.NewRecord(col)
		rec.Set("id", ch.Record)
	}

	changed := false
	// the autodate interceptor regenerates these on every save: the hub value
	// (or the current one when the patch has none) is put back after the save
	wantAuto := map[string]string{}
	if !isNew {
		for _, f := range col.Fields {
			if f.Type() == kernel.FieldTypeAutodate {
				wantAuto[f.GetName()] = rec.GetString(f.GetName())
			}
		}
	}
	for name, raw := range patch {
		f, ok := isAllowedField(col, pv, name)
		if !ok {
			continue
		}
		val := raw
		switch pv.Types[name] {
		case "counter":
			n, ok := raw.(float64)
			if !ok {
				continue
			}
			val = n + pend.inc[name] // hub value + pending local increments (§4.5)
		case "set":
			list, ok := raw.([]any)
			if !ok {
				continue
			}
			val = pend.applySet(name, list)
		default:
			if pend.hlc[name] > int64(h) {
				continue // a pending local change on this field has a higher hlc and wins at the hub too
			}
		}
		if !isNew && export(rec, f) == canon(val) {
			continue
		}
		rec.Set(name, val)
		changed = true
		if f.Type() == kernel.FieldTypeAutodate {
			if s, ok := raw.(string); ok {
				wantAuto[name] = s
			}
		}
	}

	if !isNew && !changed {
		// same values: only align the record clock
		res, err := db.NewQuery("UPDATE _sync_meta SET hlc={:h}, node={:n} WHERE collection={:c} AND record={:r} AND (hlc!={:h} OR node!={:n})").
			Bind(dbx.Params{"h": int64(h), "n": ch.Node, "c": col.Id, "r": ch.Record}).Execute()
		if err != nil {
			return false, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			var has int
			_ = db.NewQuery("SELECT COUNT(*) FROM _sync_meta WHERE collection={:c} AND record={:r}").Bind(dbx.Params{"c": col.Id, "r": ch.Record}).Row(&has)
			if has == 0 {
				_, err = db.NewQuery("INSERT INTO _sync_meta (collection, record, hlc, node) VALUES ({:c}, {:r}, {:h}, {:n})").
					Bind(dbx.Params{"c": col.Id, "r": ch.Record, "h": int64(h), "n": ch.Node}).Execute()
				if err != nil {
					return false, err
				}
				return false, c.o.Backend.Rehash(tx, rec)
			}
		}
		return false, nil
	}

	if err := tx.SaveNoValidateWithContext(ctx, rec); err != nil {
		return false, err
	}
	fixed := false
	for name, want := range wantAuto {
		var cur string
		if err := db.NewQuery("SELECT {{" + name + "}} FROM {{" + col.Name + "}} WHERE id={:id}").Bind(dbx.Params{"id": rec.Id}).Row(&cur); err != nil {
			return true, err
		}
		if strings.TrimSpace(cur) == want {
			continue
		}
		if _, err := db.NewQuery("UPDATE {{" + col.Name + "}} SET {{" + name + "}}={:v} WHERE id={:id}").
			Bind(dbx.Params{"v": want, "id": rec.Id}).Execute(); err != nil {
			return true, err
		}
		fixed = true
	}
	if fixed {
		fresh, err := tx.FindRecordById(col.Id, rec.Id)
		if err != nil {
			return true, err
		}
		return true, c.o.Backend.Rehash(tx, fresh)
	}
	return true, nil
}
