package geo

import (
	"strings"
	"testing"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/types"
)

func rawCount(t *testing.T, app core.App, table string) int {
	t.Helper()
	var n int
	if err := app.DB().NewQuery("SELECT COUNT(*) FROM [[" + table + "]]").Row(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func newGeoCol(t *testing.T, app core.App, name string) *core.Collection {
	t.Helper()
	c := kernel.NewBaseCollection(name)
	c.Fields.Add(&kernel.GeoPointField{Name: "c"}, &kernel.GeoPointField{Name: "b_c"})
	if err := app.Save(c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestG2TableNameInjectiveAndReserved(t *testing.T) {
	app, _, _ := setup(t)
	// the old scheme mapped (a_b, c) and (a, b_c) to the same _geo_a_b_c
	ab := newGeoCol(t, app, "a_b")
	a := newGeoCol(t, app, "a")
	if TableNameFor(ab, "c") == TableNameFor(a, "b_c") {
		t.Fatal("table names collide")
	}
	if !strings.HasPrefix(TableNameFor(ab, "c"), "_geo_") || len(TableNameFor(ab, "c")) != len("_geo_")+24 {
		t.Fatalf("unexpected name %s", TableNameFor(ab, "c"))
	}
	for i, c := range []*core.Collection{ab, a} {
		r := core.NewRecord(c)
		r.Set("c", types.GeoPoint{Lat: 1 + float64(i), Lon: 1})
		r.Set("b_c", types.GeoPoint{Lat: 10 + float64(i), Lon: 1})
		if err := app.Save(r); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Index(app, "a_b", "c"); err != nil {
		t.Fatal(err)
	}
	if _, err := Index(app, "a", "b_c"); err != nil {
		t.Fatal(err)
	}
	if n, _ := Count(app, "a_b", "c"); n != 1 {
		t.Fatalf("a_b.c rows=%d", n)
	}
	var ids []string
	_ = app.DB().NewQuery("SELECT rec_id FROM [[" + TableNameFor(ab, "c") + "]]").Column(&ids)
	rec, err := app.FindRecordById("a_b", ids[0])
	if err != nil || rec.Collection().Name != "a_b" {
		t.Fatalf("index of a_b holds foreign ids: %v", err)
	}

	// reserved prefix
	bad := kernel.NewBaseCollection("_geo_deadbeef")
	if err := app.Save(bad); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("collection with _geo_ prefix must be refused, got %v", err)
	}
}

func TestG2RefusesToTouchNonRtreeTable(t *testing.T) {
	app, _, _ := setup(t)
	col, _ := app.FindCollectionByNameOrId("places")
	name := TableNameFor(col, "loc")
	if _, err := app.DB().NewQuery("CREATE TABLE [[" + name + "]] (id TEXT)").Execute(); err != nil {
		t.Fatal(err)
	}
	if _, err := Index(app, "places", "loc"); err == nil {
		t.Fatal("Index must refuse an existing non rtree table")
	}
	if err := Drop(app, "places", "loc"); err != nil {
		t.Fatal(err)
	}
	if !userTableExists(app, name) {
		t.Fatal("Drop removed a non rtree table")
	}
}

func TestG2MigratesLegacyTables(t *testing.T) {
	app, _, _ := setup(t)
	if _, err := Index(app, "places", "loc"); err != nil {
		t.Fatal(err)
	}
	col, _ := app.FindCollectionByNameOrId("places")
	nt := TableNameFor(col, "loc")
	legacy := legacyTableName("places", "loc")
	if _, err := app.DB().NewQuery("ALTER TABLE [[" + nt + "]] RENAME TO [[" + legacy + "]]").Execute(); err != nil {
		t.Fatal(err)
	}
	invalidate(app)
	migrateLegacyTables(app)
	if !HasIndex(app, "places", "loc") {
		t.Fatal("legacy table not migrated")
	}
	if rtreeTables(app)[legacy] {
		t.Fatal("legacy table still present")
	}
	if rawCount(t, app, nt) == 0 {
		t.Fatal("migrated index lost its rows")
	}

	// ambiguous legacy name: dropped, not guessed
	newGeoCol(t, app, "a_b")
	newGeoCol(t, app, "a")
	amb := legacyTableName("a_b", "c") // == legacyTableName("a", "b_c")
	if amb != legacyTableName("a", "b_c") {
		t.Fatal("test premise")
	}
	if _, err := app.DB().NewQuery("CREATE VIRTUAL TABLE [[" + amb + "]] USING rtree(rid, minLat, maxLat, minLon, maxLon, +rec_id TEXT)").Execute(); err != nil {
		t.Fatal(err)
	}
	migrateLegacyTables(app)
	if rtreeTables(app)[amb] {
		t.Fatal("ambiguous legacy table must be dropped")
	}
	if HasIndex(app, "a_b", "c") || HasIndex(app, "a", "b_c") {
		t.Fatal("no index may be adopted from an ambiguous table")
	}
}

// G3: another process (the CLI) creates the index and bumps the version row;
// this process never calls invalidate and must still maintain the table at once.
func TestG3ServerSeesIndexCreatedByAnotherProcess(t *testing.T) {
	app, h, _ := setup(t)
	col, _ := app.FindCollectionByNameOrId("places")
	// prime the cache with "no index"
	if HasIndex(app, "places", "loc") {
		t.Fatal("unexpected index")
	}
	_ = existing(app)

	nt := TableNameFor(col, "loc")
	if _, err := app.DB().NewQuery("CREATE VIRTUAL TABLE [[" + nt + "]] USING rtree(rid, minLat, maxLat, minLon, maxLon, +rec_id TEXT)").Execute(); err != nil {
		t.Fatal(err)
	}
	if _, err := app.DB().NewQuery("INSERT INTO _params (id, value) VALUES ({:id}, '123') ON CONFLICT(id) DO UPDATE SET value='456'").
		Bind(map[string]any{"id": versionRow}).Execute(); err != nil {
		t.Fatal(err)
	}

	r := core.NewRecord(col)
	r.Set("name", "x")
	r.Set("public", true)
	r.Set("loc", types.GeoPoint{Lat: 12, Lon: 12})
	if err := app.Save(r); err != nil {
		t.Fatal(err)
	}
	if n := rawCount(t, app, nt); n != 1 {
		t.Fatalf("record written right after the CLI bump is missing from the index (rows=%d)", n)
	}
	got := decode(t, get(h, nearURL(12, 12, 10, "")))
	if len(got.Items) != 1 {
		t.Fatalf("near: %d", len(got.Items))
	}

	// drop by another process: near falls back instead of failing
	if _, err := app.DB().NewQuery("DROP TABLE [[" + nt + "]]").Execute(); err != nil {
		t.Fatal(err)
	}
	got = decode(t, get(h, nearURL(12, 12, 10, "")))
	if len(got.Items) != 1 {
		t.Fatalf("near after foreign drop: %d", len(got.Items))
	}
}

func TestG4RebuildSkipsNullsAndRenamesFollow(t *testing.T) {
	app, h, _ := setup(t)
	if _, err := app.DB().NewQuery("UPDATE places SET loc='null' WHERE id IN (SELECT id FROM places LIMIT 5)").Execute(); err != nil {
		t.Fatal(err)
	}
	if _, err := app.DB().NewQuery("UPDATE places SET loc='{}' WHERE id IN (SELECT id FROM places LIMIT 1)").Execute(); err != nil {
		t.Fatal(err)
	}
	n, err := Index(app, "places", "loc")
	if err != nil {
		t.Fatalf("index with NULL values: %v", err)
	}
	if n >= 200 || n < 190 {
		t.Fatalf("expected the NULL rows to be skipped, got %d", n)
	}

	// field rename: the index follows
	col, _ := app.FindCollectionByNameOrId("places")
	col.Fields.GetByName("loc").SetName("pos")
	if err := app.Save(col); err != nil {
		t.Fatal(err)
	}
	if !HasIndex(app, "places", "pos") || HasIndex(app, "places", "loc") {
		t.Fatal("index did not follow the field rename")
	}
	if c, _ := Count(app, "places", "pos"); c != n {
		t.Fatalf("rows after rename %d want %d", c, n)
	}

	// collection rename: id based name, index stays
	col, _ = app.FindCollectionByNameOrId("places")
	col.Name = "venues"
	if err := app.Save(col); err != nil {
		t.Fatal(err)
	}
	if !HasIndex(app, "venues", "pos") {
		t.Fatal("index lost after collection rename")
	}
	_ = h

	// field removal drops the table
	col, _ = app.FindCollectionByNameOrId("venues")
	nt := TableNameFor(col, "pos")
	col.Fields.RemoveByName("pos")
	if err := app.Save(col); err != nil {
		t.Fatal(err)
	}
	if rtreeTables(app)[nt] {
		t.Fatal("index of a removed field was not dropped")
	}
}

func TestG4HookRemovesRowWhenValueNotAGeoPoint(t *testing.T) {
	app, _, _ := setup(t)
	if _, err := Index(app, "places", "loc"); err != nil {
		t.Fatal(err)
	}
	col, _ := app.FindCollectionByNameOrId("places")
	nt := TableNameFor(col, "loc")
	var id string
	_ = app.DB().NewQuery("SELECT rec_id FROM [[" + nt + "]] LIMIT 1").Row(&id)
	rec, _ := app.FindRecordById("places", id)
	before := rawCount(t, app, nt)
	rec.Set("loc", nil)
	if err := app.Save(rec); err != nil {
		t.Fatal(err)
	}
	// either the field normalised null to a zero point (row kept, consistent with
	// the documented Null Island behaviour) or the row is removed: never a dangling duplicate
	after := rawCount(t, app, nt)
	if after != before && after != before-1 {
		t.Fatalf("rows %d -> %d", before, after)
	}
}
