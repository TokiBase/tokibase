// Package pg is the PostgreSQL [rule.Dialect] and emitter entry point.
//
// Type assumptions (documented in docs/RULE_ENGINE.md):
//
//   - JSON, geoPoint and multi-value fields are stored as jsonb (json works
//     too: values go through to_jsonb). A multi-value field holds a JSON
//     array, a single value field holds a plain scalar.
//   - date fields hold text in the PocketBase format or timestamptz; both
//     compare correctly with the text parameters the engine binds.
//   - bool fields are boolean, number fields are double precision/numeric.
//   - JSON members come back as text (the "#>>" operator). Comparing a JSON
//     member with a numeric parameter therefore needs an explicit cast, the
//     emitter does not guess it.
//
// Constructs that cannot be expressed return an error wrapping
// [rule.ErrUnsupported] instead of different SQL.
package pg

import (
	"fmt"
	"strings"

	"github.com/tokibase/tokibase/kernel/rule"
)

// Dialect is the PostgreSQL dialect.
var Dialect rule.Dialect = dialect{}

type dialect struct{}

func (dialect) Name() string { return "postgres" }

func unsupported(what string) error {
	return fmt.Errorf("%w: postgres: %s", rule.ErrUnsupported, what)
}

func jsonb(column string) string { return "to_jsonb([[" + column + "]])" }

func (dialect) JSONExtract(column string, path []string) (string, error) {
	for _, p := range path {
		if p == "" || strings.ContainsAny(p, `{},"\ `) {
			return "", unsupported("invalid JSON path segment " + fmt.Sprintf("%q", p))
		}
	}

	return "(" + jsonb(column) + " #>> '{" + strings.Join(path, ",") + "}')", nil
}

func (dialect) JSONArrayLength(column string) (string, error) {
	c := "[[" + column + "]]"
	j := jsonb(column)

	return "(CASE WHEN " + c + " IS NULL THEN 0 " +
		"WHEN jsonb_typeof(" + j + ") = 'array' THEN jsonb_array_length(" + j + ") " +
		"WHEN CAST(" + c + " AS TEXT) = '' THEN 0 ELSE 1 END)", nil
}

func (dialect) JSONEach(column string) (string, error) {
	return "", unsupported("JOIN over the elements of a multi-value column (:each, multi relations); needs LATERAL jsonb_array_elements_text support in the join builder")
}

func (dialect) JSONEachParam(placeholder string) (string, error) {
	return "", unsupported("@request.body.*:each; needs LATERAL jsonb_array_elements_text support in the join builder")
}

func (dialect) JSONArrayMember(idColumn, jeAlias, jsonColumn string) (string, error) {
	j := jsonb(jsonColumn)

	return "[[" + idColumn + "]] IN (SELECT jsonb_array_elements_text(CASE WHEN jsonb_typeof(" + j + ") = 'array' THEN " + j +
		" ELSE jsonb_build_array([[" + jsonColumn + "]]) END))", nil
}

func (dialect) Keyword(name string) (string, bool) {
	switch name {
	case "null":
		return "NULL", true
	case "true":
		return "TRUE", true
	case "false":
		return "FALSE", true
	}
	return "", false
}

func (dialect) TextOf(expr string) string { return "CAST(" + expr + " AS TEXT)" }

func (d dialect) CoalesceEmpty(expr string) string { return "COALESCE(" + d.TextOf(expr) + ", '')" }

func (dialect) NullSafeEq(left, right string, equal bool) string {
	if equal {
		return left + " IS NOT DISTINCT FROM " + right
	}
	return left + " IS DISTINCT FROM " + right
}

func (d dialect) Like(left, right string, negate, rightIsColumn bool) string {
	// ILIKE matches the case insensitive LIKE of SQLite (which is only
	// ASCII-insensitive; ILIKE follows the database locale).
	op := "ILIKE"
	if negate {
		op = "NOT ILIKE"
	}

	if rightIsColumn {
		return fmt.Sprintf("%s %s ('%%' || %s || '%%') ESCAPE '\\'", d.TextOf(left), op, d.TextOf(right))
	}

	return fmt.Sprintf("%s %s %s ESCAPE '\\'", d.TextOf(left), op, right)
}

func (dialect) OptionalOn() string { return " ON TRUE" }

func (dialect) ExistsNone(sub, alias, where string) string {
	return fmt.Sprintf("NOT EXISTS (SELECT 1 FROM (%s) {{%s}} WHERE %s)", sub, alias, where)
}

func (dialect) ExistsNoneMany(leftSub, leftAlias, rightSub, rightAlias, where string) string {
	return fmt.Sprintf(
		"NOT EXISTS (SELECT 1 FROM (%s) {{%s}} LEFT JOIN (%s) {{%s}} ON TRUE WHERE %s)",
		leftSub, leftAlias, rightSub, rightAlias, where,
	)
}

func (dialect) GeoDistance(lonA, latA, lonB, latB string) (string, error) {
	num := func(s string) string { return "CAST(" + s + " AS DOUBLE PRECISION)" }

	inner := `cos(radians(` + num(latA) + `)) * cos(radians(` + num(latB) + `)) * ` +
		`cos(radians(` + num(lonB) + `) - radians(` + num(lonA) + `)) + ` +
		`sin(radians(` + num(latA) + `)) * sin(radians(` + num(latB) + `))`

	// LEAST/GREATEST ignore NULL arguments (SQLite's min/max return NULL), so
	// the NULL propagation is explicit.
	return `(CASE WHEN (` + inner + `) IS NULL THEN NULL ELSE ` +
		`6371 * acos(LEAST(1, GREATEST(-1, ` + inner + `))) END)`, nil
}

func (dialect) Strftime(args []string) (string, error) {
	return "", unsupported("strftime (SQLite format and modifier language has no PostgreSQL equivalent)")
}
