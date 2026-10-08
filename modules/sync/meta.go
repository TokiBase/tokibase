//go:build !no_sync

package sync

import (
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/modules/sync/hlc"
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

// Field clocks (docs/SYNC_DESIGN.md §2.2, §4.4). `_sync_meta.fields` holds
// {"field": "<hlc hex>"} for the plain fields of field-merge collections: the
// HLC of the change that last wrote the field. Counter and set fields never
// have a clock.

// readFieldClocks returns the field clocks of a record (empty without a row).
func readFieldClocks(db dbx.Builder, colId, id string) (map[string]hlc.HLC, error) {
	var raw string
	err := db.NewQuery("SELECT fields FROM _sync_meta WHERE collection={:c} AND record={:r}").
		Bind(dbx.Params{"c": colId, "r": id}).Row(&raw)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return map[string]hlc.HLC{}, nil
		}
		return nil, err
	}
	return parseFieldClocks(raw), nil
}

// parseFieldClocks decodes the stored JSON; unreadable entries are skipped
// (a clock of 0 means "never written", the safe direction).
func parseFieldClocks(raw string) map[string]hlc.HLC {
	out := map[string]hlc.HLC{}
	if raw == "" || raw == "{}" {
		return out
	}
	var m map[string]string
	if json.Unmarshal([]byte(raw), &m) != nil {
		return out
	}
	for f, s := range m {
		if h, err := hlc.Parse(s); err == nil {
			out[f] = h
		}
	}
	return out
}

func encodeFieldClocks(m map[string]hlc.HLC) string {
	s := make(map[string]string, len(m))
	for f, h := range m {
		s[f] = h.String()
	}
	b, _ := json.Marshal(s) // sorted keys
	return string(b)
}

// bumpFieldClocks raises the clock of every field in fields to h (never
// lowers it). The `_sync_meta` row must exist.
func bumpFieldClocks(db dbx.Builder, colId, id string, fields []string, h hlc.HLC) error {
	if len(fields) == 0 {
		return nil
	}
	cur, err := readFieldClocks(db, colId, id)
	if err != nil {
		return err
	}
	for _, f := range fields {
		if cur[f] < h {
			cur[f] = h
		}
	}
	_, err = db.NewQuery("UPDATE _sync_meta SET fields={:f} WHERE collection={:c} AND record={:r}").
		Bind(dbx.Params{"f": encodeFieldClocks(cur), "c": colId, "r": id}).Execute()
	return err
}
