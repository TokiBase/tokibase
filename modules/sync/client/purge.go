//go:build !no_sync

package client

import (
	"context"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/sync/hlc"
	"github.com/tokibase/tokibase/modules/sync/proto"
)

// applyPurge applies op "p" (docs/SYNC_DESIGN.md §7.3): the record is deleted,
// a LEGAL tombstone is written (database triggers keep it permanent) and the
// patches of the local change rows of the record are erased. It reports
// whether the record existed.
func (c *Client) applyPurge(tx kernel.App, ctx context.Context, col *core.Collection, ch *proto.PullChange, h hlc.HLC) (bool, error) {
	deleted := false
	if rec, _ := tx.FindRecordById(col.Id, ch.Record); rec != nil {
		// the replica capture writes a delete tombstone, upgraded below
		if err := tx.DeleteWithContext(ctx, rec); err != nil {
			return false, err
		}
		deleted = true
	}
	db := tx.NonconcurrentDB()
	if _, err := db.NewQuery(`INSERT INTO _sync_tombstones (collection, record, kind, hlc, node, actor, reason, created)
  VALUES ({:c}, {:r}, 'legal', {:h}, {:n}, '', 'purge', {:t})
  ON CONFLICT(collection, record) DO UPDATE SET kind='legal', hlc=excluded.hlc, node=excluded.node,
    reason=excluded.reason, created=excluded.created
  WHERE _sync_tombstones.kind='delete'`).
		Bind(dbx.Params{"c": col.Id, "r": ch.Record, "h": int64(h), "n": ch.Node, "t": time.Now().UTC().Format(dateLayout)}).Execute(); err != nil {
		return deleted, err
	}
	if _, err := db.NewQuery("DELETE FROM _sync_meta WHERE collection={:c} AND record={:r}").
		Bind(dbx.Params{"c": col.Id, "r": ch.Record}).Execute(); err != nil {
		return deleted, err
	}
	// erasure also reaches the local log: pending changes keep their place in the
	// sequence but carry no data (the hub refuses them with legal_tombstone)
	_, err := db.NewQuery("UPDATE _changes SET patch='{}', hash=NULL WHERE collection IN ({:c},{:n}) AND record={:r} AND (patch!='{}' OR hash IS NOT NULL)").
		Bind(dbx.Params{"c": col.Id, "n": col.Name, "r": ch.Record}).Execute()
	if err != nil {
		return deleted, err
	}
	// the local copies of the data in `_sync_conflicts` (pushed patches that were
	// superseded or rejected, edits discarded by a revert) are erased too (P56-6)
	return deleted, blankLocalConflicts(tx, col, ch.Record)
}

// blankLocalConflicts erases every JSON copy and the note of the spoke-local
// `_sync_conflicts` rows of a record (matched by collection id and name).
func blankLocalConflicts(tx kernel.App, col *core.Collection, record string) error {
	cc, err := tx.FindCachedCollectionByNameOrId(conflictsCollection)
	if err != nil || cc == nil {
		return nil
	}
	var sets []string
	for _, f := range cc.Fields {
		if f.Type() == kernel.FieldTypeJSON {
			sets = append(sets, "[["+f.GetName()+"]]=NULL")
		}
	}
	if cc.Fields.GetByName("note") != nil {
		sets = append(sets, "[[note]]=''")
	}
	if len(sets) == 0 {
		return nil
	}
	_, err = tx.NonconcurrentDB().NewQuery("UPDATE {{" + conflictsCollection + "}} SET " + strings.Join(sets, ", ") +
		" WHERE [[collection]] IN ({:c},{:n}) AND [[record]]={:r}").
		Bind(dbx.Params{"c": col.Id, "n": col.Name, "r": record}).Execute()
	return err
}

// applyEvict applies op "x": the record left the partition (or the view rule) of
// this node. The local copy is deleted WITHOUT a tombstone, so the record can
// come back when it re-enters.
func (c *Client) applyEvict(tx kernel.App, ctx context.Context, col *core.Collection, ch *proto.PullChange, h hlc.HLC) (bool, error) {
	rec, _ := tx.FindRecordById(col.Id, ch.Record)
	if rec == nil {
		return false, nil
	}
	if err := tx.DeleteWithContext(ctx, rec); err != nil {
		return false, err
	}
	// the replica capture wrote a delete tombstone for the record and for every
	// cascade child (they carry the HLC and node of this change); an eviction is
	// not a delete, so none of them may stay
	_, err := tx.NonconcurrentDB().NewQuery("DELETE FROM _sync_tombstones WHERE kind='delete' AND ((collection={:c} AND record={:r}) OR (hlc={:h} AND node={:n}))").
		Bind(dbx.Params{"c": col.Id, "r": ch.Record, "h": int64(h), "n": ch.Node}).Execute()
	return true, err
}
