# Module `geo`

Radius and bounding-box queries on `geoPoint` fields with exact distance ordering. Package `modules/geo`. The filter language of the kernel is unchanged; this is an additive endpoint plus an optional SQLite R*Tree index.

- Enabled by default: `tokibase.go` calls `geo.Register(app)` and adds the `geo` command.
- Works on base and auth collections (not views). Needs no schema change.

## Endpoint

`GET /api/collections/{collection}/records/near`

| Param | Meaning |
| --- | --- |
| `near=<field>:<lat>,<lon>,<km>` | records within `km` kilometres, ordered by distance ascending (ties by `id`); adds `distance_km` to every item |
| `bbox=<field>:<minLat>,<minLon>,<maxLat>,<maxLon>` | records inside the box (inclusive); no distance. May be combined with `near` (intersection) |
| `filter`, `page`, `perPage`, `skipTotal`, `expand`, `fields` | as on the regular list endpoint |
| `sort` | only with `bbox` alone (regular sort syntax). With `near` it is rejected: order is distance |

At least one of `near` / `bbox` is required. Validation (400, `data.<param>.code = "validation_invalid_geo"`): lat within -90..90, lon within -180..180, `km` within 0..20000, `perPage` 1..500 (default 30), `page` >= 1, `bbox` needs `minLat <= maxLat` and `minLon <= maxLon`, the field must be a `geoPoint` field of the collection (a hidden field only for superusers).

Access: same as the list endpoint. `listRule` is applied as a SQL condition (a `null` rule means superusers only, 403 otherwise; guests only see records the rule allows), the collection `list` rate limit applies, superuser-only filter fields are rejected, `OnRecordsListRequest` and `OnRecordEnrich` run (so `expand`, `fieldperm` and `crypto` behave as usual).

Response: the standard list envelope. Each item is the normal record JSON plus one top-level key `distance_km` (float, kilometres on a 6371 km sphere; present only with `near`). `fields=` filters it like any key: include `distance_km` to keep it.

```json
{"page":1,"perPage":30,"totalItems":2,"totalPages":1,"items":[
  {"id":"abc","collectionName":"places","name":"A","loc":{"lon":0.01,"lat":45.01},"distance_km":1.4},
  {"id":"def","collectionName":"places","name":"B","loc":{"lon":0.2,"lat":45.1},"distance_km":15.7}]}
```

## SQL

Distance (haversine, `2R = 12742`), `c` = `{{coll}}.[[field]]`:

```
(12742 * asin(min(1.0, sqrt(
  sin(radians(json_extract(c,'$.lat') - :lat)/2) * sin(radians(json_extract(c,'$.lat') - :lat)/2) +
  cos(radians(:lat)) * cos(radians(json_extract(c,'$.lat'))) *
  sin(radians(json_extract(c,'$.lon') - :lon)/2) * sin(radians(json_extract(c,'$.lon') - :lon)/2)))))
```

Query: `WHERE <listRule> AND <bounding-box prefilter> AND distance <= :km ORDER BY distance ASC, id ASC LIMIT/OFFSET`. Without an index the prefilter is `json_extract(c,'$.lat') >= :minLat AND ... <= :maxLat AND ((json_extract(c,'$.lon') >= :lo AND ... <= :hi) [OR second range])`. With an index it is `id IN (SELECT rec_id FROM "_geo_<coll>_<field>" WHERE maxLat >= :minLat AND minLat <= :maxLat AND ((maxLon >= :lo AND minLon <= :hi) [OR ...]))`. The distance condition is always applied, so the prefilter only has to be conservative. The page distances are read with the same expression restricted to the page ids.

The radius box handles the antimeridian (two longitude ranges) and the poles (full longitude range). A `bbox` parameter does not: `minLon > maxLon` is rejected, query two boxes instead.

## R*Tree index

```
toki geo index <collection> <field>     # create _geo_<collection>_<field> and fill it
toki geo rebuild <collection> <field>   # empty and refill (after bulk SQL imports, or if it drifted)
toki geo drop <collection> <field>      # remove; queries fall back to the JSON prefilter
```

The virtual table is `rtree(rid, minLat, maxLat, minLon, maxLon, +rec_id TEXT)` in `data.db`. `rid` is a stable 63-bit FNV-1a hash of the record id (the real id lives in `rec_id` and is what queries join on, so rowid renumbering by `VACUUM` is harmless; a hash collision, odds about n^2/2^64, could only omit a record until `rebuild`). SQLite stores R*Tree coordinates as 32-bit floats rounded outward, which is why the exact check is always done on the real values.

The index is maintained by the running server through record create/update/delete hooks (also for Go/JS saves) and dropped when the collection is deleted. Writes done directly in SQL or by a process that does not run the module do not update it: run `rebuild`. The server notices a new or dropped index within about 2 seconds. Renaming the collection or the field leaves a stale index under the old name: `drop` it (by old name) and `index` again.

## Go API

```go
res, err := geo.Near(app, "places", "loc", 45, 0, 500, geo.Options{PerPage: 100, Filter: `name ~ "x"`})
// res.Hits[i].Record, res.Hits[i].DistanceKm; res.TotalItems, res.TotalPages
```

`Options.RequestInfo` set to a non-superuser info applies the `listRule`; nil means trusted code (no rule). Also `geo.Haversine`, `geo.Index|Rebuild|Drop|HasIndex|Count`.

## Limits

- Sphere model (error up to about 0.5% against WGS84).
- Cost without an index is a scan of the collection (JSON extraction per row); index large collections.
- `bbox` across the antimeridian is not supported.
- `distance_km` is only available on this endpoint (not as a `sort=@distance(...)` on the regular list).
- Collections with a record at the default `{"lon":0,"lat":0}` ("Null Island") include it when the area covers it.
