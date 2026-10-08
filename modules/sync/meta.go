//go:build !no_sync

package sync

import (
	"database/sql"
	"errors"

	"github.com/pocketbase/dbx"
)

// metaHLC returns the record clock the writer saw (0 when the record has no
// `_sync_meta` row). Any error other than "no rows" is returned: a failed read
// must not turn into base_hlc=0 ("unknown"), which hides conflicts.
func metaHLC(db dbx.Builder, colId, id string) (int64, error) {
	var h int64
	err := db.NewQuery("SELECT hlc FROM _sync_meta WHERE collection={:c} AND record={:r}").
		Bind(dbx.Params{"c": colId, "r": id}).Row(&h)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	return h, nil
}

func upsertMeta(db dbx.Builder, colId, id string, h int64, node string, hash []byte) error {
	_, err := db.NewQuery(`INSERT INTO _sync_meta (collection, record, hlc, node, hash) VALUES ({:c}, {:r}, {:h}, {:n}, {:x})
  ON CONFLICT(collection, record) DO UPDATE SET hlc=excluded.hlc, node=excluded.node, hash=excluded.hash`).
		Bind(dbx.Params{"c": colId, "r": id, "h": h, "n": node, "x": hash}).Execute()
	return err
}

// refreshMetaHash updates only the hash of an existing `_sync_meta` row (the
// record clock stays): used when a write changed the stored row but produced no
// change row (derived/autodate-only update), so meta.hash keeps matching
// RecordHash of the stored record.
func refreshMetaHash(db dbx.Builder, colId, id string, hash []byte) error {
	_, err := db.NewQuery("UPDATE _sync_meta SET hash={:x} WHERE collection={:c} AND record={:r}").
		Bind(dbx.Params{"c": colId, "r": id, "x": hash}).Execute()
	return err
}

// deleteMeta removes the per record clock of a deleted record.
func deleteMeta(db dbx.Builder, colId, id string) error {
	_, err := db.NewQuery("DELETE FROM _sync_meta WHERE collection={:c} AND record={:r}").
		Bind(dbx.Params{"c": colId, "r": id}).Execute()
	return err
}
