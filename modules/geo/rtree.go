package geo

import (
	"errors"
	"fmt"
	"github.com/tokibase/tokibase/kernel"
	"hash/fnv"
	"regexp"
	"sync"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/tools/hook"
	"github.com/tokibase/tokibase/tools/types"
)

var reIdent = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// TableName returns the R*Tree table name of an indexed field.
func TableName(collection, field string) string { return "_geo_" + collection + "_" + field }

// ridOf maps a record id to a stable 63 bit R*Tree row id (FNV-1a). The real
// id is kept in the auxiliary column rec_id and is what queries join on, so a
// hash collision (about n^2/2^64 for n records) could only drop a record from
// the index until `toki geo rebuild`, never return a wrong one.
func ridOf(id string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(id))
	return int64(h.Sum64() >> 1)
}

type idxCache struct {
	mu     sync.Mutex
	at     time.Time
	tables map[string]bool
}

const cacheKey = "__tokiGeoIdx__"

func cacheOf(app kernel.App) *idxCache {
	return app.Store().GetOrSet(cacheKey, func() any { return &idxCache{} }).(*idxCache)
}

const cacheTTL = 2 * time.Second

func invalidate(app kernel.App) {
	cache := cacheOf(app)
	cache.mu.Lock()
	cache.at = time.Time{}
	cache.mu.Unlock()
}

func existing(app kernel.App) map[string]bool {
	cache := cacheOf(app)
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.tables != nil && time.Since(cache.at) < cacheTTL {
		return cache.tables
	}
	var names []string
	err := app.ConcurrentDB().NewQuery("SELECT name FROM sqlite_master WHERE type='table' AND name LIKE '\\_geo\\_%' ESCAPE '\\'").Column(&names)
	m := map[string]bool{}
	if err == nil {
		for _, n := range names {
			m[n] = true
		}
	}
	cache.tables, cache.at = m, time.Now()
	return m
}

// indexTable returns the R*Tree table name when an index exists and is allowed.
func indexTable(app kernel.App, col *core.Collection, field string, disabled bool) string {
	if disabled {
		return ""
	}
	t := TableName(col.Name, field)
	if existing(app)[t] {
		return t
	}
	return ""
}

// HasIndex reports whether an R*Tree index exists for the field.
func HasIndex(app kernel.App, collection, field string) bool {
	invalidate(app)
	return existing(app)[TableName(collection, field)]
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
func Index(app kernel.App, collection, field string) (int, error) {
	col, err := target(app, collection, field)
	if err != nil {
		return 0, err
	}
	t := TableName(col.Name, field)
	_, err = app.DB().NewQuery(fmt.Sprintf(
		"CREATE VIRTUAL TABLE IF NOT EXISTS [[%s]] USING rtree(rid, minLat, maxLat, minLon, maxLon, +rec_id TEXT)", t)).Execute()
	if err != nil {
		return 0, err
	}
	invalidate(app)
	return Rebuild(app, collection, field)
}

// Rebuild empties and refills an existing index; returns the number of rows.
func Rebuild(app kernel.App, collection, field string) (int, error) {
	col, err := target(app, collection, field)
	if err != nil {
		return 0, err
	}
	t := TableName(col.Name, field)
	if !HasIndex(app, col.Name, field) {
		return 0, fmt.Errorf("no index for %s.%s (run: toki geo index)", col.Name, field)
	}
	err = app.RunInTransaction(func(tx kernel.App) error {
		if _, err := tx.DB().NewQuery("DELETE FROM [[" + t + "]]").Execute(); err != nil {
			return err
		}
		type row struct {
			Id  string  `db:"id"`
			Lat float64 `db:"lat"`
			Lon float64 `db:"lon"`
		}
		var rows []row
		q := "SELECT id, json_extract([[" + field + "]], '$.lat') AS lat, json_extract([[" + field + "]], '$.lon') AS lon FROM {{" + col.Name + "}}"
		if err := tx.DB().NewQuery(q).All(&rows); err != nil {
			return err
		}
		ins := "INSERT INTO [[" + t + "]](rid, minLat, maxLat, minLon, maxLon, rec_id) VALUES ({:rid}, {:lat}, {:lat}, {:lon}, {:lon}, {:id})"
		for _, r := range rows {
			if _, err := tx.DB().NewQuery(ins).Bind(dbx.Params{"rid": ridOf(r.Id), "lat": r.Lat, "lon": r.Lon, "id": r.Id}).Execute(); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return Count(app, col.Name, field)
}

// Count returns the number of rows of the index.
func Count(app kernel.App, collection, field string) (int, error) {
	var n int
	err := app.DB().NewQuery("SELECT COUNT(*) FROM [[" + TableName(collection, field) + "]]").Row(&n)
	return n, err
}

// Drop removes the index table (no error when missing).
func Drop(app kernel.App, collection, field string) error {
	if !reIdent.MatchString(collection) || !reIdent.MatchString(field) {
		return errors.New("unsupported collection or field name")
	}
	_, err := app.DB().NewQuery("DROP TABLE IF EXISTS [[" + TableName(collection, field) + "]]").Execute()
	invalidate(app)
	return err
}

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
		t := TableName(col.Name, gf.Name)
		if !tables[t] {
			continue
		}
		rid := ridOf(r.Id)
		_, err := app.DB().NewQuery("DELETE FROM [[" + t + "]] WHERE rid = {:rid}").Bind(dbx.Params{"rid": rid}).Execute()
		if err == nil && !remove {
			p, _ := r.Get(gf.Name).(types.GeoPoint)
			_, err = app.DB().NewQuery("INSERT INTO [[" + t + "]](rid, minLat, maxLat, minLon, maxLon, rec_id) VALUES ({:rid}, {:lat}, {:lat}, {:lon}, {:lon}, {:id})").
				Bind(dbx.Params{"rid": rid, "lat": p.Lat, "lon": p.Lon, "id": r.Id}).Execute()
		}
		if err != nil {
			app.Logger().Error("geo: failed to maintain rtree index", "table", t, "record", r.Id, "error", err)
		}
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
	app.OnCollectionAfterDeleteSuccess().Bind(&hook.Handler[*core.CollectionEvent]{
		Id: hookId,
		Func: func(e *core.CollectionEvent) error {
			if err := e.Next(); err != nil {
				return err
			}
			for _, f := range e.Collection.Fields {
				if _, ok := f.(*core.GeoPointField); ok {
					_ = Drop(e.App, e.Collection.Name, f.GetName())
				}
			}
			return nil
		},
	})
}
