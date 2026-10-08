//go:build !no_sync

package sync

import (
	"github.com/pocketbase/dbx"
	validation "github.com/pocketbase/ozzo-validation/v4"
)

// guardTombstone refuses to create a record whose id has a tombstone.
func guardTombstone(db dbx.Builder, colId, id string) error {
	var n int
	if err := db.NewQuery("SELECT COUNT(*) FROM _sync_tombstones WHERE collection={:c} AND record={:r}").
		Bind(dbx.Params{"c": colId, "r": id}).Row(&n); err != nil {
		return err
	}
	if n > 0 {
		return validation.Errors{"id": validation.NewError("validation_sync_tombstoned",
			"This record id was deleted (tombstone) and cannot be created again.")}
	}
	return nil
}

func putTombstone(db dbx.Builder, colId, id, kind string, h int64, node, actor, reason, created string) error {
	_, err := db.NewQuery(`INSERT INTO _sync_tombstones (collection, record, kind, hlc, node, actor, reason, created)
  VALUES ({:c}, {:r}, {:k}, {:h}, {:n}, {:a}, {:why}, {:t})
  ON CONFLICT(collection, record) DO UPDATE SET hlc=excluded.hlc, node=excluded.node, actor=excluded.actor, created=excluded.created
  WHERE kind='delete'`).
		Bind(dbx.Params{"c": colId, "r": id, "k": kind, "h": h, "n": node, "a": actor, "why": reason, "t": created}).Execute()
	return err
}
