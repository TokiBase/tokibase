package geo

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	validation "github.com/pocketbase/ozzo-validation/v4"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/hook"
	"github.com/tokibase/tokibase/tools/types"
)

var reIdent = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// TablePrefix is the prefix of every R*Tree table (and of the SQLite shadow
// tables it creates). Collection names starting with it are refused.
const TablePrefix = "_geo_"

// TableNameFor returns the R*Tree table name of collection.field. The name is
// derived from the collection id (not its name), so it is injective, cannot
// collide with a user table and survives a collection rename.
func TableNameFor(col *core.Collection, field string) string { return tableNameOf(col.Id, field) }

func tableNameOf(collectionId, field string) string {
	sum := sha256.Sum256([]byte(collectionId + "\x00" + field))
	return TablePrefix + hex.EncodeToString(sum[:])[:24]
}

// legacyTableName is the pre-hash naming scheme (ambiguous), kept for migration.
func legacyTableName(collection, field string) string { return TablePrefix + collection + "_" + field }

// TableName returns the R*Tree table name of an indexed field by collection name or id.
func TableName(app kernel.App, collection, field string) string {
	if col, err := app.FindCachedCollectionByNameOrId(collection); err == nil {
		return TableNameFor(col, field)
	}
	return tableNameOf(collection, field)
}

// ridOf maps a record id to a stable 63 bit R*Tree row id (FNV-1a). The real
// id is kept in the auxiliary column rec_id and is what queries join on, so a
// hash collision (about n^2/2^64 for n records) could only drop a record from
// the index until `toki geo rebuild`, never return a wrong one.
func ridOf(id string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(id))
	return int64(h.Sum64() >> 1)
}

// The set of index tables is cached together with a version number stored in
// the _params row versionRow. Every index creation, drop or rename (also by
// the CLI, another process) bumps it, and every lookup re-reads the row (one
// primary key read), so a running server never works with a stale table set.
const versionRow = "toki_geo_index_version"

type idxCache struct {
	mu      sync.Mutex
	version string
	tables  map[string]bool
}

const cacheKey = "__tokiGeoIdx__"

func cacheOf(app kernel.App) *idxCache {
	return app.Store().GetOrSet(cacheKey, func() any { return &idxCache{} }).(*idxCache)
}

func readVersion(app kernel.App) string {
	var v sql.NullString
	err := app.ConcurrentDB().NewQuery("SELECT CAST([[value]] AS TEXT) FROM {{_params}} WHERE [[id]]={:id}").
		Bind(dbx.Params{"id": versionRow}).Row(&v)
	if err != nil {
		return ""
	}
	return v.String
}

// bumpVersion tells every process that the index table set changed.
func bumpVersion(app kernel.App) error {
	_, err := app.NonconcurrentDB().NewQuery(
		"INSERT INTO {{_params}} ([[id]], [[value]]) VALUES ({:id}, {:v}) " +
			"ON CONFLICT([[id]]) DO UPDATE SET [[value]]=excluded.[[value]], [[updated]]=strftime('%Y-%m-%d %H:%M:%fZ')").
		Bind(dbx.Params{"id": versionRow, "v": strconv.FormatInt(time.Now().UnixNano(), 10)}).Execute()
	invalidate(app)
	return err
}

func invalidate(app kernel.App) {
	cache := cacheOf(app)
	cache.mu.Lock()
	cache.tables = nil
	cache.mu.Unlock()
}

// rtreeTables lists the R*Tree tables (virtual tables only, never user tables).
func rtreeTables(app kernel.App) map[string]bool {
	var names []string
	err := app.ConcurrentDB().NewQuery("SELECT name FROM sqlite_master WHERE type='table' AND name LIKE '\\_geo\\_%' ESCAPE '\\' AND lower(sql) LIKE 'create virtual table%using rtree%'").Column(&names)
	m := map[string]bool{}
	if err == nil {
		for _, n := range names {
			m[n] = true
		}
	}
	return m
}

func existing(app kernel.App) map[string]bool {
	cache := cacheOf(app)
	ver := readVersion(app)
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.tables != nil && cache.version == ver {
		return cache.tables
	}
	cache.tables, cache.version = rtreeTables(app), ver
	return cache.tables
}

// indexTable returns the R*Tree table name when an index exists and is allowed.
func indexTable(app kernel.App, col *core.Collection, field string, disabled bool) string {
	if disabled {
		return ""
	}
	t := TableNameFor(col, field)
	if existing(app)[t] {
		return t
	}
	return ""
}

// HasIndex reports whether an R*Tree index exists for the field.
func HasIndex(app kernel.App, collection, field string) bool {
	invalidate(app)
	return existing(app)[TableName(app, collection, field)]
}

func target(app kernel.App, collection, field string) (*core.Collection, error) {
	col, err := app.FindCachedCollectionByNameOrId(collection)
	if err != nil {
		return nil, err
	}
	if err := geoField(col, field, true, "field"); err != nil {
		return nil, err
	}
	if !reIdent.MatchString(col.Name) || !reIdent.MatchString(field) {
		return nil, errors.New("unsupported collection or field name")
	}
	return col, nil
}

// Index creates the R*Tree table for collection.field and fills it.
//
// Order matters for a running server in another process: create the table,
// bump the version (the server now maintains the table on every write), then
// rebuild (one transaction, serialized with the server writes).
func Index(app kernel.App, collection, field string) (int, error) {
	col, err := target(app, collection, field)
	if err != nil {
		return 0, err
	}
	t := TableNameFor(col, field)
	if !existing(app)[t] {
		if tbl := userTableExists(app, t); tbl {
			return 0, fmt.Errorf("a table named %s already exists and is not an R*Tree index", t)
		}
	}
	_, err = app.NonconcurrentDB().NewQuery(fmt.Sprintf(
		"CREATE VIRTUAL TABLE IF NOT EXISTS [[%s]] USING rtree(rid, minLat, maxLat, minLon, maxLon, +rec_id TEXT)", t)).Execute()
	if err != nil {
		return 0, err
	}
	if err := bumpVersion(app); err != nil {
		return 0, err
	}
	return Rebuild(app, collection, field)
}

func userTableExists(app kernel.App, name string) bool {
	var n int
	_ = app.ConcurrentDB().NewQuery("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name={:n}").Bind(dbx.Params{"n": name}).Row(&n)
	return n > 0
}

// Rebuild empties and refills an existing index; returns the number of rows.
// Rows whose geoPoint value is NULL or not a coordinate pair are skipped.
func Rebuild(app kernel.App, collection, field string) (int, error) {
	col, err := target(app, collection, field)
	if err != nil {
		return 0, err
	}
	t := TableNameFor(col, field)
	if !HasIndex(app, col.Name, field) {
		return 0, fmt.Errorf("no index for %s.%s (run: toki geo index)", col.Name, field)
	}
	err = app.RunInTransaction(func(tx kernel.App) error {
		if _, err := tx.DB().NewQuery("DELETE FROM [[" + t + "]]").Execute(); err != nil {
			return err
		}
		type row struct {
			Id  string          `db:"id"`
			Lat sql.NullFloat64 `db:"lat"`
			Lon sql.NullFloat64 `db:"lon"`
		}
		var rows []row
		q := "SELECT id, json_extract([[" + field + "]], '$.lat') AS lat, json_extract([[" + field + "]], '$.lon') AS lon FROM {{" + col.Name + "}}"
		if err := tx.DB().NewQuery(q).All(&rows); err != nil {
			return err
		}
		ins := "INSERT INTO [[" + t + "]](rid, minLat, maxLat, minLon, maxLon, rec_id) VALUES ({:rid}, {:lat}, {:lat}, {:lon}, {:lon}, {:id})"
		for _, r := range rows {
			if !r.Lat.Valid || !r.Lon.Valid {
				continue
			}
			if _, err := tx.DB().NewQuery(ins).Bind(dbx.Params{"rid": ridOf(r.Id), "lat": r.Lat.Float64, "lon": r.Lon.Float64, "id": r.Id}).Execute(); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	var n int
	err = app.DB().NewQuery("SELECT COUNT(*) FROM [[" + t + "]]").Row(&n)
	return n, err
}

// Count returns the number of rows of the index.
func Count(app kernel.App, collection, field string) (int, error) {
	var n int
	err := app.DB().NewQuery("SELECT COUNT(*) FROM [[" + TableName(app, collection, field) + "]]").Row(&n)
	return n, err
}

// Drop removes the index table (no error when missing). Only R*Tree tables are dropped.
func Drop(app kernel.App, collection, field string) error {
	t := TableName(app, collection, field)
	// bump first: servers stop writing to the table before it disappears
	if err := bumpVersion(app); err != nil {
		return err
	}
	if rtreeTables(app)[t] {
		if _, err := app.NonconcurrentDB().NewQuery("DROP TABLE IF EXISTS [[" + t + "]]").Execute(); err != nil {
			return err
		}
	}
	return bumpVersion(app)
}

func isNoTable(err error) bool { return err != nil && strings.Contains(err.Error(), "no such table") }

func syncRecord(app kernel.App, r *core.Record, remove bool) {
	tables := existing(app)
	if len(tables) == 0 {
		return
	}
	col := r.Collection()
	for _, f := range col.Fields {
		gf, ok := f.(*core.GeoPointField)
		if !ok {
			continue
		}
		t := TableNameFor(col, gf.Name)
		if !tables[t] {
			continue
		}
		rid := ridOf(r.Id)
		_, err := app.DB().NewQuery("DELETE FROM [[" + t + "]] WHERE rid = {:rid}").Bind(dbx.Params{"rid": rid}).Execute()
		if err == nil && !remove {
			// a null/non-geo value only removes the row
			if p, ok := r.GetRaw(gf.Name).(types.GeoPoint); ok {
				_, err = app.DB().NewQuery("INSERT INTO [[" + t + "]](rid, minLat, maxLat, minLon, maxLon, rec_id) VALUES ({:rid}, {:lat}, {:lat}, {:lon}, {:lon}, {:id})").
					Bind(dbx.Params{"rid": rid, "lat": p.Lat, "lon": p.Lon, "id": r.Id}).Execute()
			}
		}
		if isNoTable(err) {
			invalidate(app) // dropped by another process
			continue
		}
		if err != nil {
			app.Logger().Error("geo: failed to maintain rtree index", "table", t, "record", r.Id, "error", err)
		}
	}
}

// migrateLegacyTables renames tables created by older versions
// (_geo_<collection>_<field>) to the id based name. A legacy name that more than
// one collection/field pair maps to is ambiguous: the table is dropped (and
// must be created again with `toki geo index`) instead of guessing.
func migrateLegacyTables(app kernel.App) {
	cols, err := app.FindAllCollections()
	if err != nil {
		return
	}
	rt := rtreeTables(app)
	type pair struct {
		col   *core.Collection
		field string
	}
	byLegacy := map[string][]pair{}
	for _, c := range cols {
		for _, f := range c.Fields {
			if _, ok := f.(*core.GeoPointField); ok {
				l := legacyTableName(c.Name, f.GetName())
				byLegacy[l] = append(byLegacy[l], pair{c, f.GetName()})
			}
		}
	}
	changed := false
	for legacy, ps := range byLegacy {
		if !rt[legacy] {
			continue
		}
		if len(ps) > 1 {
			app.Logger().Warn("geo: ambiguous legacy index dropped, run `toki geo index` again", "table", legacy)
			_, _ = app.NonconcurrentDB().NewQuery("DROP TABLE IF EXISTS [[" + legacy + "]]").Execute()
			changed = true
			continue
		}
		nt := TableNameFor(ps[0].col, ps[0].field)
		if rt[nt] {
			continue
		}
		if _, err := app.NonconcurrentDB().NewQuery("ALTER TABLE [[" + legacy + "]] RENAME TO [[" + nt + "]]").Execute(); err != nil {
			app.Logger().Error("geo: failed to rename legacy index", "table", legacy, "error", err)
			continue
		}
		changed = true
	}
	if changed {
		_ = bumpVersion(app)
	}
}

// renameFieldTables follows field renames and removals of a collection save.
func renameFieldTables(app kernel.App, old, cur *core.Collection) {
	rt := rtreeTables(app)
	changed := false
	for _, f := range old.Fields {
		if _, ok := f.(*core.GeoPointField); !ok {
			continue
		}
		ot := TableNameFor(old, f.GetName())
		if !rt[ot] {
			continue
		}
		nf := cur.Fields.GetById(f.GetId())
		if gf, ok := nf.(*core.GeoPointField); ok {
			nt := TableNameFor(cur, gf.Name)
			if nt != ot && !rt[nt] {
				if _, err := app.DB().NewQuery("ALTER TABLE [[" + ot + "]] RENAME TO [[" + nt + "]]").Execute(); err != nil {
					app.Logger().Error("geo: failed to rename index", "table", ot, "error", err)
				}
				changed = true
			}
			continue
		}
		// field removed or no longer a geoPoint
		if _, err := app.DB().NewQuery("DROP TABLE IF EXISTS [[" + ot + "]]").Execute(); err != nil {
			app.Logger().Error("geo: failed to drop index", "table", ot, "error", err)
		}
		changed = true
	}
	if changed {
		_ = bumpVersion(app)
	}
}

func bindIndexHooks(app kernel.App) {
	mk := func(remove bool) func(e *core.RecordEvent) error {
		return func(e *core.RecordEvent) error {
			if err := e.Next(); err != nil {
				return err
			}
			syncRecord(e.App, e.Record, remove)
			return nil
		}
	}
	app.OnRecordAfterCreateSuccess().Bind(&hook.Handler[*core.RecordEvent]{Id: hookId, Func: mk(false)})
	app.OnRecordAfterUpdateSuccess().Bind(&hook.Handler[*core.RecordEvent]{Id: hookId, Func: mk(false)})
	app.OnRecordAfterDeleteSuccess().Bind(&hook.Handler[*core.RecordEvent]{Id: hookId, Func: mk(true)})

	app.OnBootstrap().Bind(&hook.Handler[*core.BootstrapEvent]{
		Id: hookId,
		Func: func(e *core.BootstrapEvent) error {
			if err := e.Next(); err != nil {
				return err
			}
			migrateLegacyTables(e.App)
			return nil
		},
	})

	// _geo_ is reserved for the index tables (and their SQLite shadow tables)
	app.OnCollectionValidate().Bind(&hook.Handler[*core.CollectionEvent]{
		Id: hookId,
		Func: func(e *core.CollectionEvent) error {
			if strings.HasPrefix(e.Collection.Name, TablePrefix) {
				return validation.Errors{"name": validation.NewError(ErrCode, "Collection names starting with "+TablePrefix+" are reserved for geo index tables.")}
			}
			return e.Next()
		},
	})

	app.OnCollectionUpdateExecute().Bind(&hook.Handler[*core.CollectionEvent]{
		Id: hookId,
		Func: func(e *core.CollectionEvent) error {
			old, _ := e.App.FindCollectionByNameOrId(e.Collection.Id)
			if err := e.Next(); err != nil {
				return err
			}
			if old != nil {
				renameFieldTables(e.App, old, e.Collection)
			}
			return nil
		},
	})
	app.OnCollectionAfterDeleteSuccess().Bind(&hook.Handler[*core.CollectionEvent]{
		Id: hookId,
		Func: func(e *core.CollectionEvent) error {
			if err := e.Next(); err != nil {
				return err
			}
			for _, f := range e.Collection.Fields {
				if _, ok := f.(*core.GeoPointField); ok {
					_ = Drop(e.App, e.Collection.Id, f.GetName())
				}
			}
			return nil
		},
	})
}
