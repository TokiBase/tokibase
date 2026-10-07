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
	"unicode"

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

// pgPath renders the text[] literal of a path. Every element is double quoted
// so that keys like "null" stay strings (an unquoted NULL array element makes
// the whole "#>"/"#>>" result NULL).
func pgPath(path []rule.Segment) (string, error) {
	quoted := make([]string, len(path))

	for i, p := range path {
		if p.Key == "" || strings.ContainsAny(p.Key, "{},\"\\' ") || strings.IndexFunc(p.Key, unicode.IsControl) >= 0 {
			return "", unsupported("invalid JSON path segment " + fmt.Sprintf("%q", p.Key))
		}
		quoted[i] = `"` + p.Key + `"`
	}

	return "'{" + strings.Join(quoted, ",") + "}'", nil
}

func (dialect) JSONExtract(column string, path []rule.Segment) (string, error) {
	pth, err := pgPath(path)
	if err != nil {
		return "", err
	}

	return "(" + jsonb(column) + " #>> " + pth + ")", nil
}

func (dialect) JSONExtractTyped(column string, path []rule.Segment) (string, error) {
	pth, err := pgPath(path)
	if err != nil {
		return "", err
	}

	return "(" + jsonb(column) + " #> " + pth + ")", nil
}

func (dialect) JSONScalar(expr string, t rule.ValueType) string {
	switch t {
	case rule.ValueBool:
		return "to_jsonb(CAST(" + expr + " AS BOOLEAN))"
	default:
		return "to_jsonb(CAST(" + expr + " AS DOUBLE PRECISION))"
	}
}

func (dialect) NativeCompare(t rule.ValueType) bool {
	return t == rule.ValueNumber || t == rule.ValueBool || t == rule.ValueDate
}

func (dialect) EmptyFor(t rule.ValueType) string {
	switch t {
	case rule.ValueBool:
		return "CAST(NULL AS BOOLEAN)"
	case rule.ValueNumber:
		return "CAST(NULL AS DOUBLE PRECISION)"
	}
	return "''"
}

// NormalizeLikePattern doubles a dangling escape character (PostgreSQL rejects
// "LIKE pattern must not end with escape character", SQLite ignores it).
func (dialect) NormalizeLikePattern(pattern string) string {
	n := 0
	for i := len(pattern) - 1; i >= 0 && pattern[i] == '\\'; i-- {
		n++
	}
	if n%2 == 1 {
		return pattern + `\`
	}
	return pattern
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
		return fmt.Sprintf("%s %s ('%%' || %s || '%%') ESCAPE E'\\\\'", d.TextOf(left), op, d.TextOf(right))
	}

	return fmt.Sprintf("%s %s %s ESCAPE E'\\\\'", d.TextOf(left), op, right)
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

// numberRegex matches the text values that CAST(.. AS DOUBLE PRECISION)
// accepts and SQLite would treat as numbers. Written without "[[" (dbx marker).
const numberRegex = `^ *[-+]?([0-9]+[.]?[0-9]*|[.][0-9]+)([eE][-+]?[0-9]+)? *$`

// GeoDistance evaluates every argument exactly once (derived tables) and
// converts it with a guarded cast: a non-numeric value yields NULL like in
// SQLite instead of aborting the whole query.
func (dialect) GeoDistance(lonA, latA, lonB, latB string) (string, error) {
	safe := func(col string) string {
		return "CASE WHEN " + col + " ~ '" + numberRegex + "' THEN CAST(" + col + " AS DOUBLE PRECISION) END"
	}

	inner := `cos(radians(g.la1)) * cos(radians(g.la2)) * cos(radians(g.lo2) - radians(g.lo1)) + sin(radians(g.la1)) * sin(radians(g.la2))`

	// LEAST/GREATEST ignore NULL arguments (SQLite's min/max return NULL), so
	// the NULL propagation is explicit.
	return `(SELECT CASE WHEN (` + inner + `) IS NULL THEN NULL ELSE ` +
		`6371 * acos(LEAST(1, GREATEST(-1, ` + inner + `))) END FROM (SELECT ` +
		safe("t.lo1") + ` AS lo1, ` + safe("t.la1") + ` AS la1, ` + safe("t.lo2") + ` AS lo2, ` + safe("t.la2") + ` AS la2 ` +
		// (the arguments are listed in the order of the SQLite output so that the
		// placeholders keep the same order)
		`FROM (SELECT CAST(` + latA + ` AS TEXT) AS la1, CAST(` + latB + ` AS TEXT) AS la2, ` +
		`CAST(` + lonB + ` AS TEXT) AS lo2, CAST(` + lonA + ` AS TEXT) AS lo1) t) g)`, nil
}

func (dialect) Strftime(args []string) (string, error) {
	return "", unsupported("strftime (SQLite format and modifier language has no PostgreSQL equivalent)")
}
