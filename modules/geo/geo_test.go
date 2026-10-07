package geo

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"

	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tests"
	"github.com/tokibase/tokibase/tools/types"
)

type pt struct {
	id       string
	lat, lon float64
	public   bool
}

func setup(t *testing.T) (*tests.TestApp, http.Handler, []pt) {
	t.Helper()
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)
	Register(app)

	c := kernel.NewBaseCollection("places")
	c.Fields.Add(&kernel.TextField{Name: "name"}, &kernel.BoolField{Name: "public"}, &kernel.GeoPointField{Name: "loc"})
	rule := "public = true"
	c.ListRule = &rule
	if err := app.Save(c); err != nil {
		t.Fatal(err)
	}

	rnd := rand.New(rand.NewSource(42))
	var pts []pt
	for i := 0; i < 200; i++ {
		p := pt{lat: 35 + rnd.Float64()*20, lon: -15 + rnd.Float64()*30, public: i%4 != 0}
		if i >= 190 { // near the antimeridian / pole
			p.lat, p.lon = 89.5-float64(i-190)*0.01, rnd.Float64()*360-180
		}
		if i == 150 {
			p.lat, p.lon = 10, 179.9
		}
		if i == 151 {
			p.lat, p.lon = 10.1, -179.9
		}
		r := core.NewRecord(c)
		r.Set("name", fmt.Sprintf("p%d", i))
		r.Set("public", p.public)
		r.Set("loc", types.GeoPoint{Lat: p.lat, Lon: p.lon})
		if err := app.Save(r); err != nil {
			t.Fatal(err)
		}
		p.id = r.Id
		pts = append(pts, p)
	}

	router, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	var h http.Handler
	err = app.OnServe().Trigger(&core.ServeEvent{App: app, Router: router}, func(se *core.ServeEvent) error {
		h, err = se.Router.BuildMux()
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return app, h, pts
}

func get(h http.Handler, path string, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

type resp struct {
	Page       int              `json:"page"`
	PerPage    int              `json:"perPage"`
	TotalItems int              `json:"totalItems"`
	TotalPages int              `json:"totalPages"`
	Items      []map[string]any `json:"items"`
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) resp {
	t.Helper()
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var r resp
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

type exp struct {
	id string
	d  float64
}

func brute(pts []pt, lat, lon, km float64, onlyPublic bool) []exp {
	var out []exp
	for _, p := range pts {
		if onlyPublic && !p.public {
			continue
		}
		if d := Haversine(lat, lon, p.lat, p.lon); d <= km {
			out = append(out, exp{p.id, d})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].d != out[j].d {
			return out[i].d < out[j].d
		}
		return out[i].id < out[j].id
	})
	return out
}

func compare(t *testing.T, label string, r resp, want []exp) {
	t.Helper()
	if r.TotalItems != len(want) {
		t.Fatalf("%s: totalItems %d want %d", label, r.TotalItems, len(want))
	}
	if len(r.Items) != len(want) {
		t.Fatalf("%s: got %d items want %d", label, len(r.Items), len(want))
	}
	for i, it := range r.Items {
		if it["id"] != want[i].id {
			t.Fatalf("%s: item %d id %v want %s", label, i, it["id"], want[i].id)
		}
		d, ok := it["distance_km"].(float64)
		if !ok || math.Abs(d-want[i].d) > 1e-6 {
			t.Fatalf("%s: item %d distance %v want %v", label, i, it["distance_km"], want[i].d)
		}
	}
}

var queries = []struct{ lat, lon, km float64 }{
	{45, 0, 500}, {45, 0, 50}, {40, -10, 1200}, {50, 10, 0}, {45, 0, 20000},
	{89.9, 0, 300}, {10, 180, 100}, {10, -179.95, 200}, {-33, 151, 1000},
}

func nearURL(lat, lon, km float64, extra string) string {
	return fmt.Sprintf("/api/collections/places/records/near?near=loc:%v,%v,%v&perPage=500%s", lat, lon, km, extra)
}

func TestNearMatchesBruteForceGuestAndSuperuser(t *testing.T) {
	app, h, pts := setup(t)
	su := superuserToken(t, app)

	for _, q := range queries {
		got := decode(t, get(h, nearURL(q.lat, q.lon, q.km, "")))
		compare(t, fmt.Sprintf("guest %v", q), got, brute(pts, q.lat, q.lon, q.km, true))
		got = decode(t, get(h, nearURL(q.lat, q.lon, q.km, ""), "Authorization", su))
		compare(t, fmt.Sprintf("su %v", q), got, brute(pts, q.lat, q.lon, q.km, false))
	}
	// something non trivial must have matched
	if r := decode(t, get(h, nearURL(45, 0, 500, ""))); len(r.Items) < 5 {
		t.Fatalf("test data too sparse: %d", len(r.Items))
	}
}

func TestRTreeSameResultsAndMaintained(t *testing.T) {
	app, h, pts := setup(t)
	su := superuserToken(t, app)

	n, err := Index(app, "places", "loc")
	if err != nil || n != 200 {
		t.Fatalf("index: %d %v", n, err)
	}
	if !HasIndex(app, "places", "loc") {
		t.Fatal("index missing")
	}
	for _, q := range queries {
		got := decode(t, get(h, nearURL(q.lat, q.lon, q.km, "")))
		compare(t, fmt.Sprintf("rtree guest %v", q), got, brute(pts, q.lat, q.lon, q.km, true))
	}

	// update: move a public point into the query area
	var target pt
	for _, p := range pts {
		if p.public && Haversine(45, 0, p.lat, p.lon) > 3000 {
			target = p
			break
		}
	}
	if target.id == "" {
		t.Skip("no far point")
	}
	rec, _ := app.FindRecordById("places", target.id)
	rec.Set("loc", types.GeoPoint{Lat: 45.01, Lon: 0.01})
	if err := app.Save(rec); err != nil {
		t.Fatal(err)
	}
	for i := range pts {
		if pts[i].id == target.id {
			pts[i].lat, pts[i].lon = 45.01, 0.01
		}
	}
	got := decode(t, get(h, nearURL(45, 0, 5, "")))
	compare(t, "after update", got, brute(pts, 45, 0, 5, true))
	if len(got.Items) != 1 || got.Items[0]["id"] != target.id {
		t.Fatalf("moved record not found: %+v", got.Items)
	}

	// delete
	if err := app.Delete(rec); err != nil {
		t.Fatal(err)
	}
	kept := pts[:0]
	for _, p := range pts {
		if p.id != target.id {
			kept = append(kept, p)
		}
	}
	pts = kept
	got = decode(t, get(h, nearURL(45, 0, 5, "")))
	compare(t, "after delete", got, brute(pts, 45, 0, 5, true))
	if c, _ := Count(app, "places", "loc"); c != 199 {
		t.Fatalf("index rows %d want 199", c)
	}

	// tamper with the index, rebuild repairs it
	if _, err := app.DB().NewQuery("DELETE FROM [[" + TableName("places", "loc") + "]]").Execute(); err != nil {
		t.Fatal(err)
	}
	if r := decode(t, get(h, nearURL(45, 0, 500, ""))); len(r.Items) != 0 {
		t.Fatalf("emptied index must return nothing, got %d", len(r.Items))
	}
	if n, err := Rebuild(app, "places", "loc"); err != nil || n != 199 {
		t.Fatalf("rebuild %d %v", n, err)
	}
	got = decode(t, get(h, nearURL(45, 0, 500, ""), "Authorization", su))
	compare(t, "after rebuild", got, brute(pts, 45, 0, 500, false))

	// drop falls back to the scan
	if err := Drop(app, "places", "loc"); err != nil {
		t.Fatal(err)
	}
	if HasIndex(app, "places", "loc") {
		t.Fatal("still indexed")
	}
	got = decode(t, get(h, nearURL(45, 0, 500, "")))
	compare(t, "after drop", got, brute(pts, 45, 0, 500, true))
}

func TestPaginationAndFilterAndFields(t *testing.T) {
	_, h, pts := setup(t)
	want := brute(pts, 45, 0, 800, true)
	var all []exp
	for page := 1; ; page++ {
		r := decode(t, get(h, fmt.Sprintf("/api/collections/places/records/near?near=loc:45,0,800&perPage=7&page=%d", page)))
		if r.TotalItems != len(want) || r.PerPage != 7 {
			t.Fatalf("envelope %+v", r)
		}
		for _, it := range r.Items {
			all = append(all, exp{it["id"].(string), it["distance_km"].(float64)})
		}
		if page >= r.TotalPages {
			break
		}
	}
	if len(all) != len(want) {
		t.Fatalf("paged %d want %d", len(all), len(want))
	}
	for i := range all {
		if all[i].id != want[i].id {
			t.Fatalf("page order differs at %d", i)
		}
	}

	r := decode(t, get(h, nearURL(45, 0, 800, "&filter="+url.QueryEscape(`name ~ "p1"`))))
	for _, it := range r.Items {
		if !strings.Contains(it["name"].(string), "p1") {
			t.Fatalf("filter ignored: %v", it["name"])
		}
	}
	r = decode(t, get(h, nearURL(45, 0, 800, "&fields=id,distance_km&skipTotal=true")))
	if r.TotalItems != -1 || len(r.Items) == 0 {
		t.Fatalf("skipTotal: %+v", r)
	}
	for _, it := range r.Items {
		if len(it) != 2 {
			t.Fatalf("fields: %v", it)
		}
	}
}

func TestBBox(t *testing.T) {
	_, h, pts := setup(t)
	var want []string
	for _, p := range pts {
		if p.public && p.lat >= 40 && p.lat <= 45 && p.lon >= -5 && p.lon <= 5 {
			want = append(want, p.id)
		}
	}
	sort.Strings(want)
	r := decode(t, get(h, "/api/collections/places/records/near?perPage=500&bbox=loc:40,-5,45,5"))
	var got []string
	for _, it := range r.Items {
		got = append(got, it["id"].(string))
		if _, has := it["distance_km"]; has {
			t.Fatal("bbox alone must not add distance_km")
		}
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") || len(want) == 0 {
		t.Fatalf("bbox got %d want %d", len(got), len(want))
	}
	// bbox + near: intersection, ordered by distance
	r = decode(t, get(h, "/api/collections/places/records/near?perPage=500&bbox=loc:40,-5,45,5&near=loc:42,0,200"))
	for _, it := range r.Items {
		if it["distance_km"].(float64) > 200 {
			t.Fatal("radius exceeded")
		}
	}
}

func TestListRuleAndAccess(t *testing.T) {
	app, h, _ := setup(t)
	c, _ := app.FindCollectionByNameOrId("places")
	// guests only ever see public records
	r := decode(t, get(h, nearURL(45, 0, 20000, "")))
	for _, it := range r.Items {
		if it["public"] != true {
			t.Fatal("non public record leaked")
		}
	}
	// null listRule: guests get 403, superusers 200
	c.ListRule = nil
	if err := app.Save(c); err != nil {
		t.Fatal(err)
	}
	if rec := get(h, nearURL(45, 0, 100, "")); rec.Code != 403 {
		t.Fatalf("want 403 got %d", rec.Code)
	}
	if rec := get(h, nearURL(45, 0, 100, ""), "Authorization", superuserToken(t, app)); rec.Code != 200 {
		t.Fatalf("superuser want 200 got %d", rec.Code)
	}
}

func TestInvalidInputs(t *testing.T) {
	_, h, _ := setup(t)
	base := "/api/collections/places/records/near?"
	for name, q := range map[string]string{
		"nothing":        "",
		"lat high":       "near=loc:91,0,10",
		"lat low":        "near=loc:-90.1,0,10",
		"lon high":       "near=loc:0,181,10",
		"lon low":        "near=loc:0,-180.5,10",
		"km negative":    "near=loc:0,0,-1",
		"km too big":     "near=loc:0,0,20001",
		"km nan":         "near=loc:0,0,NaN",
		"not number":     "near=loc:a,0,1",
		"too few":        "near=loc:1,2",
		"no field":       "near=1,2,3",
		"unknown field":  "near=nope:1,2,3",
		"non geo field":  "near=name:1,2,3",
		"perPage big":    "near=loc:0,0,1&perPage=501",
		"perPage zero":   "near=loc:0,0,1&perPage=0",
		"page zero":      "near=loc:0,0,1&page=0",
		"sort with near": "near=loc:0,0,1&sort=name",
		"bbox min>max":   "bbox=loc:10,0,5,1",
		"bbox lon":       "bbox=loc:0,10,1,5",
		"bbox count":     "bbox=loc:0,0,1",
		"bbox lat":       "bbox=loc:0,0,91,1",
		"skipTotal":      "near=loc:0,0,1&skipTotal=maybe",
		"bad filter":     "near=loc:0,0,1&filter=" + url.QueryEscape("nope ="),
	} {
		rec := get(h, base+q)
		if rec.Code != 400 {
			t.Errorf("%s: want 400 got %d %s", name, rec.Code, rec.Body.String())
		}
	}
	if rec := get(h, "/api/collections/missing/records/near?near=loc:0,0,1"); rec.Code != 404 {
		t.Errorf("missing collection: %d", rec.Code)
	}
	if rec := get(h, base+"near=loc:91,0,1"); !strings.Contains(rec.Body.String(), ErrCode) {
		t.Errorf("error code missing: %s", rec.Body.String())
	}
}

func TestGoAPI(t *testing.T) {
	app, _, pts := setup(t)
	res, err := Near(app, "places", "loc", 45, 0, 500, Options{PerPage: 500})
	if err != nil {
		t.Fatal(err)
	}
	want := brute(pts, 45, 0, 500, false)
	if len(res.Hits) != len(want) {
		t.Fatalf("%d vs %d", len(res.Hits), len(want))
	}
	for i, h := range res.Hits {
		if h.Record.Id != want[i].id || math.Abs(h.DistanceKm-want[i].d) > 1e-6 {
			t.Fatalf("hit %d mismatch", i)
		}
	}
	if _, err := Near(app, "places", "loc", 91, 0, 1, Options{}); err == nil {
		t.Fatal("expected error")
	}
	if _, err := Near(app, "places", "loc", 0, 0, 1, Options{PerPage: 501}); err == nil {
		t.Fatal("expected perPage error")
	}
}

func TestNearBoxCoversRadius(t *testing.T) {
	rnd := rand.New(rand.NewSource(7))
	for i := 0; i < 20000; i++ {
		c := Point{rnd.Float64()*180 - 90, rnd.Float64()*360 - 180}
		km := rnd.Float64() * 3000
		minLat, maxLat, lons := nearBox(c, km)
		// random point at distance <= km (uniform bearing)
		d := rnd.Float64() * km / EarthRadiusKm
		br := rnd.Float64() * 2 * math.Pi
		la1, lo1 := c.Lat*math.Pi/180, c.Lon*math.Pi/180
		la2 := math.Asin(math.Sin(la1)*math.Cos(d) + math.Cos(la1)*math.Sin(d)*math.Cos(br))
		lo2 := lo1 + math.Atan2(math.Sin(br)*math.Sin(d)*math.Cos(la1), math.Cos(d)-math.Sin(la1)*math.Sin(la2))
		lat, lon := la2*180/math.Pi, math.Mod(lo2*180/math.Pi+540, 360)-180
		in := false
		for _, l := range lons {
			if lon >= l[0] && lon <= l[1] {
				in = true
			}
		}
		if lat < minLat || lat > maxLat || !in {
			t.Fatalf("box misses point: c=%v km=%v p=(%v,%v)", c, km, lat, lon)
		}
	}
}

func superuserToken(t *testing.T, app core.App) string {
	t.Helper()
	col, err := app.FindCollectionByNameOrId(core.CollectionNameSuperusers)
	if err != nil {
		t.Fatal(err)
	}
	r := core.NewRecord(col)
	r.SetEmail("geo@example.com")
	r.SetPassword("1234567890")
	if err := app.Save(r); err != nil {
		t.Fatal(err)
	}
	tok, err := r.NewAuthToken()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}
