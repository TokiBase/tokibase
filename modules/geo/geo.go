// Package geo adds radius and bounding-box queries on geoPoint fields with
// exact distance ordering, via the additive endpoint
// GET /api/collections/{collection}/records/near, a Go API (Near) and an
// optional SQLite R*Tree accelerator maintained by record hooks.
//
// The filter language of the kernel is not changed. Distance is computed in
// SQL (haversine, sphere radius 6371 km) so pagination and ordering are exact.
package geo

import (
	"errors"
	"fmt"
	"github.com/tokibase/tokibase/kernel"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/tools/hook"
	"github.com/tokibase/tokibase/tools/search"
)

const (
	hookId = "__tokiGeo__"

	// EarthRadiusKm is the sphere radius used by every distance computation.
	EarthRadiusKm = 6371.0

	// MaxKm is the largest accepted radius.
	MaxKm = 20000.0
	// MaxPerPage is the largest accepted perPage.
	MaxPerPage = 500

	// ErrCode is the validation error code of invalid geo parameters.
	ErrCode = "validation_invalid_geo"
)

// Register binds the endpoint and the R*Tree maintenance hooks.
func Register(app core.App) {
	app.OnServe().Bind(&hook.Handler[*core.ServeEvent]{
		Id: hookId,
		Func: func(se *core.ServeEvent) error {
			se.Router.GET("/api/collections/{collection}/records/near", nearHandler)
			return se.Next()
		},
	})
	bindIndexHooks(app)
}

// Point is a validated coordinate pair.
type Point struct{ Lat, Lon float64 }

// BBox is a bounding box (minLon <= maxLon, no antimeridian crossing).
type BBox struct{ MinLat, MinLon, MaxLat, MaxLon float64 }

// Hit is one Near result.
type Hit struct {
	Record     *core.Record
	DistanceKm float64
}

// Result is a page of hits.
type Result struct {
	Hits       []Hit
	Page       int
	PerPage    int
	TotalItems int // -1 when skipTotal
	TotalPages int // -1 when skipTotal
}

// Options for Near.
type Options struct {
	Page      int    // default 1
	PerPage   int    // default 30, max 500
	Filter    string // extra filter in the regular filter language
	SkipTotal bool
	// RequestInfo, when set and not a superuser, makes the collection listRule
	// and filter access checks apply as for an API call. Nil means trusted code:
	// no rule is applied.
	RequestInfo *core.RequestInfo
	// NoIndex forces the plain JSON bounding box even when an R*Tree exists.
	NoIndex bool
}

// spec is the validated internal query.
type spec struct {
	col       *core.Collection
	field     string
	near      *Point
	km        float64
	bbox      *BBox
	bboxField string
	filter    string
	sort      string
	page      int
	perPage   int
	skipTotal bool
	info      *core.RequestInfo
	enforce   bool
	noIndex   bool
}

type paramError struct{ param, msg string }

func (p *paramError) Error() string { return p.param + ": " + p.msg }

func bad(param, format string, a ...any) error {
	return &paramError{param, fmt.Sprintf(format, a...)}
}

func validLat(v float64) bool { return !math.IsNaN(v) && v >= -90 && v <= 90 }
func validLon(v float64) bool { return !math.IsNaN(v) && v >= -180 && v <= 180 }

func parseNums(s string, n int, param string) ([]float64, error) {
	parts := strings.Split(s, ",")
	if len(parts) != n {
		return nil, bad(param, "expected %d comma separated numbers.", n)
	}
	out := make([]float64, n)
	for i, p := range parts {
		v, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil || math.IsInf(v, 0) || math.IsNaN(v) {
			return nil, bad(param, "%q is not a number.", p)
		}
		out[i] = v
	}
	return out, nil
}

func splitField(v, param string) (string, string, error) {
	i := strings.IndexByte(v, ':')
	if i <= 0 {
		return "", "", bad(param, "expected <field>:<values>.")
	}
	return v[:i], v[i+1:], nil
}

func checkNear(lat, lon, km float64) error {
	if !validLat(lat) {
		return bad("near", "lat must be within -90..90.")
	}
	if !validLon(lon) {
		return bad("near", "lon must be within -180..180.")
	}
	if math.IsNaN(km) || km < 0 || km > MaxKm {
		return bad("near", "km must be within 0..%d.", int(MaxKm))
	}
	return nil
}

func checkBBox(b BBox) error {
	if !validLat(b.MinLat) || !validLat(b.MaxLat) {
		return bad("bbox", "latitudes must be within -90..90.")
	}
	if !validLon(b.MinLon) || !validLon(b.MaxLon) {
		return bad("bbox", "longitudes must be within -180..180.")
	}
	if b.MinLat > b.MaxLat {
		return bad("bbox", "minLat must be <= maxLat.")
	}
	if b.MinLon > b.MaxLon {
		return bad("bbox", "minLon must be <= maxLon (boxes crossing the antimeridian are not supported).")
	}
	return nil
}

func geoField(col *core.Collection, name string, superuser bool, param string) error {
	if col.IsView() {
		return bad(param, "view collections are not supported.")
	}
	f, ok := col.Fields.GetByName(name).(*core.GeoPointField)
	if !ok || f == nil {
		return bad(param, "%q is not a geoPoint field of %q.", name, col.Name)
	}
	if f.Hidden && !superuser {
		return bad(param, "%q is not a geoPoint field of %q.", name, col.Name)
	}
	return nil
}

// Near returns the records of collection whose geoPoint field lies within km
// kilometres of (lat, lon), ordered by distance ascending (then id).
func Near(app kernel.App, collection, field string, lat, lon, km float64, opts Options) (*Result, error) {
	col, err := app.FindCachedCollectionByNameOrId(collection)
	if err != nil {
		return nil, err
	}
	if err := checkNear(lat, lon, km); err != nil {
		return nil, err
	}
	if err := geoField(col, field, true, "near"); err != nil {
		return nil, err
	}
	if opts.PerPage > MaxPerPage {
		return nil, bad("perPage", "must be <= %d.", MaxPerPage)
	}
	s := &spec{
		col: col, field: field, near: &Point{lat, lon}, km: km, filter: opts.Filter,
		page: opts.Page, perPage: opts.PerPage, skipTotal: opts.SkipTotal,
		info: opts.RequestInfo, noIndex: opts.NoIndex,
	}
	if s.info == nil {
		s.info = &core.RequestInfo{Context: core.RequestInfoContextDefault, Method: http.MethodGet}
	} else if !s.info.HasSuperuserAuth() {
		s.enforce = true
	}
	if s.enforce && col.ListRule == nil {
		return nil, errors.New("only superusers can perform this action")
	}
	return s.run(app)
}

// ---- geometry --------------------------------------------------------------

// Haversine returns the great-circle distance in km (sphere radius 6371 km).
func Haversine(lat1, lon1, lat2, lon2 float64) float64 {
	const rad = math.Pi / 180
	dLat := (lat2 - lat1) * rad
	dLon := (lon2 - lon1) * rad
	h := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * EarthRadiusKm * math.Asin(math.Min(1, math.Sqrt(h)))
}

// nearBox returns a conservative latitude range and the longitude ranges
// (one or two when the box wraps the antimeridian) that contain every point
// within km of p.
func nearBox(p Point, km float64) (minLat, maxLat float64, lons [][2]float64) {
	const margin = 1e-6
	dLat := km/(EarthRadiusKm*math.Pi/180) + margin
	minLat, maxLat = p.Lat-dLat, p.Lat+dLat
	full := [][2]float64{{-180, 180}}
	r := km / EarthRadiusKm
	if maxLat >= 90 || minLat <= -90 || r >= math.Pi/2 {
		return math.Max(minLat, -90), math.Min(maxLat, 90), full
	}
	ratio := math.Sin(r) / math.Cos(p.Lat*math.Pi/180)
	if ratio >= 1 {
		return minLat, maxLat, full
	}
	dLon := math.Asin(ratio)*180/math.Pi + margin
	lo, hi := p.Lon-dLon, p.Lon+dLon
	switch {
	case lo < -180:
		lons = [][2]float64{{lo + 360, 180}, {-180, hi}}
	case hi > 180:
		lons = [][2]float64{{lo, 180}, {-180, hi - 360}}
	default:
		lons = [][2]float64{{lo, hi}}
	}
	return minLat, maxLat, lons
}

// ---- query building --------------------------------------------------------

func distanceSQL(col *core.Collection, field string) string {
	c := "{{" + col.Name + "}}.[[" + field + "]]"
	lat := "json_extract(" + c + ", '$.lat')"
	lon := "json_extract(" + c + ", '$.lon')"
	return "(" + fmt.Sprintf("%v", 2*EarthRadiusKm) + " * asin(min(1.0, sqrt(" +
		"sin(radians(" + lat + " - {:geoqlat}) / 2) * sin(radians(" + lat + " - {:geoqlat}) / 2) + " +
		"cos(radians({:geoqlat})) * cos(radians(" + lat + ")) * " +
		"sin(radians(" + lon + " - {:geoqlon}) / 2) * sin(radians(" + lon + " - {:geoqlon}) / 2)))))"
}

// boxSQL builds "lat in range AND lon in range(s)" over the JSON column or the
// rtree columns; prefix is "p<n>".
func boxSQL(latExpr, lonExpr string, minLat, maxLat float64, lons [][2]float64, prefix string, params dbx.Params) string {
	params[prefix+"minlat"], params[prefix+"maxlat"] = minLat, maxLat
	parts := make([]string, 0, len(lons))
	for i, l := range lons {
		a, b := fmt.Sprintf("%slonlo%d", prefix, i), fmt.Sprintf("%slonhi%d", prefix, i)
		params[a], params[b] = l[0], l[1]
		parts = append(parts, "("+lonExpr+" >= {:"+a+"} AND "+lonExpr+" <= {:"+b+"})")
	}
	return latExpr + " >= {:" + prefix + "minlat} AND " + latExpr + " <= {:" + prefix + "maxlat} AND (" + strings.Join(parts, " OR ") + ")"
}

func rtreeBoxSQL(table string, minLat, maxLat float64, lons [][2]float64, prefix string, params dbx.Params) string {
	params[prefix+"minlat"], params[prefix+"maxlat"] = minLat, maxLat
	parts := make([]string, 0, len(lons))
	for i, l := range lons {
		a, b := fmt.Sprintf("%slonlo%d", prefix, i), fmt.Sprintf("%slonhi%d", prefix, i)
		params[a], params[b] = l[0], l[1]
		parts = append(parts, "(maxLon >= {:"+a+"} AND minLon <= {:"+b+"})")
	}
	return "SELECT rec_id FROM [[" + table + "]] WHERE maxLat >= {:" + prefix + "minlat} AND minLat <= {:" + prefix + "maxlat} AND (" + strings.Join(parts, " OR ") + ")"
}

func jsonCol(col *core.Collection, field, key string) string {
	return "json_extract({{" + col.Name + "}}.[[" + field + "]], '$." + key + "')"
}

func (s *spec) run(app kernel.App) (*Result, error) {
	query := app.RecordQuery(s.col)
	resolver := core.NewRecordFieldResolver(app, s.col, s.info, true)

	if s.enforce && s.col.ListRule != nil && *s.col.ListRule != "" {
		expr, err := search.FilterData(*s.col.ListRule).BuildExpr(resolver)
		if err != nil {
			return nil, err
		}
		query.AndWhere(expr)
	}
	resolver.SetAllowHiddenFields(s.info.HasSuperuserAuth())

	params := dbx.Params{}
	idCol := "{{" + s.col.Name + "}}.[[id]]"

	if s.bbox != nil {
		f := s.bboxField
		b := s.bbox
		lons := [][2]float64{{b.MinLon, b.MaxLon}}
		if tbl := indexTable(app, s.col, f, s.noIndex); tbl != "" {
			// the rtree rounds outward: conservative prefilter, exact check below
			query.AndWhere(dbx.NewExp(idCol + " IN (" + rtreeBoxSQL(tbl, b.MinLat, b.MaxLat, lons, "bbi", params) + ")"))
		}
		query.AndWhere(dbx.NewExp(boxSQL(jsonCol(s.col, f, "lat"), jsonCol(s.col, f, "lon"), b.MinLat, b.MaxLat, lons, "bbx", params)))
	}

	if s.near != nil {
		minLat, maxLat, lons := nearBox(*s.near, s.km)
		if tbl := indexTable(app, s.col, s.field, s.noIndex); tbl != "" {
			query.AndWhere(dbx.NewExp(idCol + " IN (" + rtreeBoxSQL(tbl, minLat, maxLat, lons, "nri", params) + ")"))
		} else {
			query.AndWhere(dbx.NewExp(boxSQL(jsonCol(s.col, s.field, "lat"), jsonCol(s.col, s.field, "lon"), minLat, maxLat, lons, "nrx", params)))
		}
		dist := distanceSQL(s.col, s.field)
		params["geoqlat"], params["geoqlon"], params["geokm"] = s.near.Lat, s.near.Lon, s.km
		query.AndWhere(dbx.NewExp(dist + " <= {:geokm}"))
		query.OrderBy(dist + " ASC")
		query.AndOrderBy(idCol + " ASC")
	}
	query.Bind(params)

	provider := search.NewProvider(resolver).Query(query).SkipTotal(s.skipTotal)
	provider.CountCol("_rowid_")
	if s.page > 0 {
		provider.Page(s.page)
	}
	if s.perPage > 0 {
		provider.PerPage(s.perPage)
	}
	if s.filter != "" {
		provider.AddFilter(search.FilterData(s.filter))
	}
	if s.sort != "" && s.near == nil {
		for _, sf := range search.ParseSortFromString(s.sort) {
			provider.AddSort(sf)
		}
	}

	records := []*core.Record{}
	res, err := provider.Exec(&records)
	if err != nil {
		return nil, err
	}
	out := &Result{Page: res.Page, PerPage: res.PerPage, TotalItems: res.TotalItems, TotalPages: res.TotalPages}

	// distances for the page: the same SQL expression, restricted to the page ids
	// (the record scanner drops columns that are not collection fields)
	dists := map[string]float64{}
	if s.near != nil && len(records) > 0 {
		ids := make([]any, len(records))
		for i, r := range records {
			ids[i] = r.Id
		}
		var rows []struct {
			Id string  `db:"id"`
			D  float64 `db:"d"`
		}
		dq := app.ConcurrentDB().Select("{{"+s.col.Name+"}}.[[id]] AS [[id]]", distanceSQL(s.col, s.field)+" AS [[d]]").
			From(s.col.Name).AndWhere(dbx.In("{{"+s.col.Name+"}}.[[id]]", ids...)).
			Bind(dbx.Params{"geoqlat": s.near.Lat, "geoqlon": s.near.Lon})
		if err := dq.All(&rows); err != nil {
			return nil, err
		}
		for _, r := range rows {
			dists[r.Id] = r.D
		}
	}
	for _, r := range records {
		h := Hit{Record: r, DistanceKm: math.NaN()}
		if d, ok := dists[r.Id]; ok {
			h.DistanceKm = d
		}
		out.Hits = append(out.Hits, h)
	}
	return out, nil
}
