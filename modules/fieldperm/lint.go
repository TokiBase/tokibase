//go:build !no_fieldperm

package fieldperm

import (
	"sort"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/tools/search"
)

// Finding is one lint result.
type Finding struct {
	Collection string `json:"collection"`
	Field      string `json:"field"`
	Kind       string `json:"kind"` // read | write | rule
	Severity   string `json:"severity"`
	Message    string `json:"message"`
}

// Lint checks that every stored rule points at an existing collection and
// field and that its expression parses against the collection.
func Lint(app core.App) ([]Finding, error) {
	rules, err := List(app, "")
	if err != nil {
		return nil, err
	}
	out := []Finding{}
	add := func(r Rule, kind, msg string) {
		out = append(out, Finding{Collection: r.Collection, Field: r.Field, Kind: kind, Severity: "error", Message: msg})
	}
	for _, r := range rules {
		col, err := app.FindCachedCollectionByNameOrId(r.Collection)
		if err != nil || col == nil {
			add(r, "rule", "unknown collection")
			continue
		}
		if col.Fields.GetByName(r.Field) == nil {
			add(r, "rule", "unknown field "+r.Field)
		}
		for _, p := range []struct {
			kind string
			rule *string
		}{{"read", r.Read}, {"write", r.Write}} {
			if p.rule == nil || *p.rule == "" {
				continue
			}
			info := &core.RequestInfo{
				Context: core.RequestInfoContextDefault, Method: "GET",
				Query: map[string]string{}, Headers: map[string]string{}, Body: map[string]any{},
			}
			resolver := core.NewRecordFieldResolver(app, col, info, true)
			if _, err := search.FilterData(*p.rule).BuildExpr(resolver); err != nil {
				add(r, p.kind, "rule does not parse: "+err.Error())
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Collection < out[j].Collection })
	return out, nil
}
