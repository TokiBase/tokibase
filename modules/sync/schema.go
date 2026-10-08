//go:build !no_sync

package sync

import (
	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
)

// ddl lists the statements one by one: the triggers contain semicolons, so
// the list must not be split on them.
var ddl = []string{
	`CREATE TABLE IF NOT EXISTS _changes (
  seq            INTEGER PRIMARY KEY AUTOINCREMENT,
  node           TEXT    NOT NULL,
  origin_seq     INTEGER NOT NULL,
  hlc            INTEGER NOT NULL,
  base_hlc       INTEGER NOT NULL DEFAULT 0,
  collection     TEXT    NOT NULL,
  record         TEXT    NOT NULL,
  op             TEXT    NOT NULL CHECK (op IN ('c','u','d','p')),
  patch          TEXT    NOT NULL DEFAULT '{}',
  hash           BLOB,
  schema_version INTEGER NOT NULL,
  actor          TEXT    NOT NULL DEFAULT '',
  tx             TEXT    NOT NULL DEFAULT '',
  part_old       TEXT    NOT NULL DEFAULT '',
  part_new       TEXT    NOT NULL DEFAULT '',
  target         TEXT    NOT NULL DEFAULT '',
  status         TEXT    NOT NULL,
  code           TEXT    NOT NULL DEFAULT '',
  created        TEXT    NOT NULL
)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx__changes_origin ON _changes (node, origin_seq)`,
	`CREATE INDEX IF NOT EXISTS idx__changes_rec ON _changes (collection, record, hlc)`,
	`CREATE INDEX IF NOT EXISTS idx__changes_out ON _changes (status, seq) WHERE status IN ('local','pushed')`,
	`CREATE INDEX IF NOT EXISTS idx__changes_pull ON _changes (seq, collection, part_new) WHERE status IN ('applied','revert')`,

	`CREATE TABLE IF NOT EXISTS _sync_meta (
  collection TEXT    NOT NULL,
  record     TEXT    NOT NULL,
  hlc        INTEGER NOT NULL,
  node       TEXT    NOT NULL,
  fields     TEXT    NOT NULL DEFAULT '{}',
  hash       BLOB,
  part       TEXT    NOT NULL DEFAULT '',
  PRIMARY KEY (collection, record)
) WITHOUT ROWID`,

	`CREATE TABLE IF NOT EXISTS _sync_tombstones (
  collection TEXT    NOT NULL,
  record     TEXT    NOT NULL,
  kind       TEXT    NOT NULL CHECK (kind IN ('delete','legal')),
  hlc        INTEGER NOT NULL,
  node       TEXT    NOT NULL,
  actor      TEXT    NOT NULL DEFAULT '',
  reason     TEXT    NOT NULL DEFAULT '',
  created    TEXT    NOT NULL,
  PRIMARY KEY (collection, record)
) WITHOUT ROWID`,
	`CREATE INDEX IF NOT EXISTS idx__sync_tomb_prune ON _sync_tombstones (created) WHERE kind = 'delete'`,
	`CREATE TRIGGER IF NOT EXISTS trg__sync_tomb_legal_del BEFORE DELETE ON _sync_tombstones
  WHEN old.kind = 'legal' BEGIN SELECT RAISE(ABORT, 'legal tombstone is permanent'); END`,
	`CREATE TRIGGER IF NOT EXISTS trg__sync_tomb_legal_upd BEFORE UPDATE ON _sync_tombstones
  WHEN old.kind = 'legal' BEGIN SELECT RAISE(ABORT, 'legal tombstone is permanent'); END`,

	`CREATE TABLE IF NOT EXISTS _sync_state (
  key   TEXT PRIMARY KEY,
  value TEXT
)`,
}

// ensureSchema creates the plain tables of the module in data.db.
func ensureSchema(app core.App) error {
	for _, q := range ddl {
		if _, err := app.NonconcurrentDB().NewQuery(q).Execute(); err != nil {
			return errf("ddl: %w", err)
		}
	}
	return nil
}

// PoliciesCollection is the system collection that chooses what is synced.
// PR1 only has the fields that capture needs; the rest arrives with PR6.
const PoliciesCollection = "_sync_policies"

// Policy directions.
const (
	DirBoth = "both"
	DirPush = "push"
	DirPull = "pull"
	DirNone = "none"
)

// EnsurePolicyCollection creates the minimal `_sync_policies` system collection.
func EnsurePolicyCollection(app core.App) error {
	if c, _ := app.FindCollectionByNameOrId(PoliciesCollection); c != nil {
		return nil
	}
	c := core.NewBaseCollection(PoliciesCollection)
	c.System = true
	c.Fields.Add(
		&core.TextField{Name: "collection", Required: true, Max: 100},
		&core.SelectField{Name: "direction", MaxSelect: 1, Values: []string{DirBoth, DirPush, DirPull, DirNone}},
		&core.JSONField{Name: "field_types", MaxSize: 16384},
		&core.JSONField{Name: "exclude", MaxSize: 16384},
		&core.BoolField{Name: "enabled"},
		&core.AutodateField{Name: "created", OnCreate: true},
		&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true},
	)
	c.AddIndex("idx_sync_policies_collection", true, "[[collection]]", "")
	return app.Save(c)
}

var _ dbx.Builder
