// Package dbadvise reports the collections whose common sort and filter
// fields have no index (`toki db advise`).
//
// Two sources are used: the schema (the autodate `created`/`updated` fields
// that lists are sorted by, and the single relation fields that lists are
// filtered by) and the retained request logs (the `sort`/`filter` query
// parameters of slow list requests).
package dbadvise

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/tools/dbutils"
)

// Options configures [Run].
type Options struct {
	// MinRecords skips the collections with fewer records (default 1000).
	MinRecords int64
	// SlowMs is the request execTime from which the logs are considered (default 50).
	SlowMs float64
	// LogLimit is the number of most recent list request logs to scan (default 50000, 0 = default, -1 = skip the logs).
	LogLimit int
}

// Finding is one unindexed field.
type Finding struct {
	Collection string  `json:"collection"`
	Field      string  `json:"field"`
	Kind       string  `json:"kind"`   // sort or filter
	Source     string  `json:"source"` // schema or logs
	Records    int64   `json:"records"`
	Requests   int     `json:"requests,omitempty"`
	AvgMs      float64 `json:"avgMs,omitempty"`
	MaxMs      float64 `json:"maxMs,omitempty"`
	Suggestion string  `json:"suggestion"`
}

// Run returns the findings, the ones seen in slow requests first (by total time),
// then the schema heuristics by collection size.
func Run(app core.App, opts Options) ([]Finding, error) {
	if opts.MinRecords <= 0 {
		opts.MinRecords = 1000
	}
	if opts.SlowMs <= 0 {
		opts.SlowMs = 50
	}
	if opts.LogLimit == 0 {
		opts.LogLimit = 50000
	}

	collections, err := app.FindAllCollections()
	if err != nil {
		return nil, err
	}

	infos := map[string]*collInfo{}
	for _, c := range collections {
		if c.IsView() {
			continue
		}

		var n int64
		if err := app.ConcurrentDB().Select("count(*)").From(c.Name).Row(&n); err != nil || n < opts.MinRecords {
			continue
		}

		infos[c.Name] = &collInfo{c: c, indexed: LeadingColumns(c.Indexes), records: n}
	}

	var result []Finding
	seen := map[string]bool{}

	// request logs
	if opts.LogLimit > 0 {
		for _, f := range fromLogs(app, infos, opts) {
			seen[f.Collection+"."+f.Field+"."+f.Kind] = true
			result = append(result, f)
		}

		sort.SliceStable(result, func(i, j int) bool {
			return result[i].AvgMs*float64(result[i].Requests) > result[j].AvgMs*float64(result[j].Requests)
		})
	}

	// schema heuristics
	names := make([]string, 0, len(infos))
	for name := range infos {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return infos[names[i]].records > infos[names[j]].records })

	for _, name := range names {
		in := infos[name]

		for _, field := range in.c.Fields {
			kind := ""
			switch f := field.(type) {
			case *core.AutodateField:
				if f.Name == "created" || f.Name == "updated" {
					kind = "sort"
				}
			case *core.RelationField:
				if f.MaxSelect <= 1 {
					kind = "filter"
				}
			}

			if kind == "" || in.indexed[strings.ToLower(field.GetName())] || seen[name+"."+field.GetName()+"."+kind] {
				continue
			}

			result = append(result, Finding{
				Collection: name,
				Field:      field.GetName(),
				Kind:       kind,
				Source:     "schema",
				Records:    in.records,
				Suggestion: Suggest(name, field.GetName()),
			})
		}
	}

	return result, nil
}

type collInfo struct {
	c       *core.Collection
	indexed map[string]bool
	records int64
}

// Suggest returns the CREATE INDEX statement for one column.
func Suggest(collection, field string) string {
	return fmt.Sprintf("CREATE INDEX `idx_%s_%s` ON `%s` (`%s`)", collection, field, collection, field)
}

// LeadingColumns returns the lower cased first columns of the collection
// index definitions (an index can only serve a sort or filter on its first column).
func LeadingColumns(indexes []string) map[string]bool {
	out := map[string]bool{"id": true}

	for _, raw := range indexes {
		idx := dbutils.ParseIndex(raw)
		if len(idx.Columns) > 0 && idx.Columns[0].Name != "" {
			out[strings.ToLower(idx.Columns[0].Name)] = true
		}
	}

	return out
}

// SortFields returns the fields of a `sort` query parameter ("-created,title" -> created, title).
func SortFields(sortParam string) []string {
	var out []string

	for _, part := range strings.Split(sortParam, ",") {
		part = strings.TrimLeft(strings.TrimSpace(part), "+-")
		if part == "" || strings.HasPrefix(part, "@") || strings.Contains(part, "(") || strings.Contains(part, ".") {
			continue
		}
		out = append(out, part)
	}

	return out
}

var (
	quotedRegex   = regexp.MustCompile(`'(?:[^'\\]|\\.)*'|"(?:[^"\\]|\\.)*"`)
	filterFieldRx = regexp.MustCompile(`(?:^|[\s(&|!])([A-Za-z_][A-Za-z0-9_]*)\s*(?:\?)?(?:!=|>=|<=|=|>|<|!~|~)`)
)

// FilterFields returns the plain (not relation path, not macro) fields compared in a `filter` expression.
func FilterFields(filter string) []string {
	filter = quotedRegex.ReplaceAllString(filter, "''")

	var out []string

	for _, m := range filterFieldRx.FindAllStringSubmatch(filter, -1) {
		out = append(out, m[1])
	}

	return out
}

type agg struct {
	n         int
	sum, max  float64
	collName  string
	field, kd string
}

func fromLogs(app core.App, colls map[string]*collInfo, opts Options) []Finding {
	if !app.AuxHasTable("_logs") {
		return nil
	}

	var rows []struct {
		URL string  `db:"url"`
		Ms  float64 `db:"ms"`
	}

	err := app.AuxConcurrentDB().NewQuery(`
		SELECT json_extract([[data]], '$.url') AS url, CAST(json_extract([[data]], '$.execTime') AS REAL) AS ms
		FROM {{_logs}}
		WHERE [[message]] LIKE 'GET /api/collections/%/records%'
		ORDER BY rowid DESC
		LIMIT {:limit}`).Bind(dbx.Params{"limit": opts.LogLimit}).All(&rows)
	if err != nil {
		return nil
	}

	aggs := map[string]*agg{}

	for _, r := range rows {
		if r.Ms < opts.SlowMs {
			continue
		}

		u, err := url.Parse(r.URL)
		if err != nil {
			continue
		}

		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) < 4 || parts[0] != "api" || parts[1] != "collections" || parts[3] != "records" {
			continue
		}

		coll, ok := colls[parts[2]]
		if !ok {
			continue
		}

		add := func(field, kind string) {
			if coll.indexed[strings.ToLower(field)] {
				return
			}
			key := parts[2] + "." + field + "." + kind
			a := aggs[key]
			if a == nil {
				a = &agg{collName: parts[2], field: field, kd: kind}
				aggs[key] = a
			}
			a.n++
			a.sum += r.Ms
			a.max = max(a.max, r.Ms)
		}

		q := u.Query()
		for _, f := range SortFields(q.Get("sort")) {
			add(f, "sort")
		}
		for _, f := range FilterFields(q.Get("filter")) {
			add(f, "filter")
		}
	}

	out := make([]Finding, 0, len(aggs))
	for _, a := range aggs {
		out = append(out, Finding{
			Collection: a.collName,
			Field:      a.field,
			Kind:       a.kd,
			Source:     "logs",
			Records:    colls[a.collName].records,
			Requests:   a.n,
			AvgMs:      a.sum / float64(a.n),
			MaxMs:      a.max,
			Suggestion: Suggest(a.collName, a.field),
		})
	}

	return out
}
