// Package sqlite is the SQLite [rule.Dialect]. It reproduces, byte for byte,
// the SQL the legacy fused rule compiler produced.
package sqlite

import (
	"fmt"

	"github.com/tokibase/tokibase/kernel/rule"
	"github.com/tokibase/tokibase/tools/dbutils"
)

// Dialect is the SQLite dialect.
var Dialect rule.Dialect = dialect{}

type dialect struct{}

func (dialect) Name() string { return "sqlite" }

func (dialect) JSONExtract(column string, path []string) (string, error) {
	return dbutils.JSONExtract(column, rule.JSONPathString(path)), nil
}

func (dialect) JSONArrayLength(column string) (string, error) {
	return dbutils.JSONArrayLength(column), nil
}

func (dialect) JSONEach(column string) (string, error) {
	return dbutils.JSONEach(column), nil
}

func (dialect) JSONEachParam(placeholder string) (string, error) {
	return fmt.Sprintf("json_each({:%s})", placeholder), nil
}

func (dialect) JSONArrayMember(idColumn, jeAlias, jsonColumn string) (string, error) {
	return fmt.Sprintf(
		"[[%s]] IN (SELECT [[%s.value]] FROM %s {{%s}})",
		idColumn,
		jeAlias,
		dbutils.JSONEach(jsonColumn),
		jeAlias,
	), nil
}

func (dialect) Keyword(name string) (string, bool) {
	switch name {
	case "null":
		return "NULL", true
	case "true":
		return "1", true
	case "false":
		return "0", true
	}
	return "", false
}

func (dialect) TextOf(expr string) string { return expr }

func (dialect) CoalesceEmpty(expr string) string { return "COALESCE(" + expr + ", '')" }

func (dialect) NullSafeEq(left, right string, equal bool) string {
	if equal {
		return left + " IS " + right
	}
	return left + " IS NOT " + right
}

func (dialect) Like(left, right string, negate, rightIsColumn bool) string {
	op := "LIKE"
	if negate {
		op = "NOT LIKE"
	}

	if rightIsColumn {
		return fmt.Sprintf("%s %s ('%%' || %s || '%%') ESCAPE '\\'", left, op, right)
	}

	return fmt.Sprintf("%s %s %s ESCAPE '\\'", left, op, right)
}

func (dialect) OptionalOn() string { return "" }

func (dialect) ExistsNone(sub, alias, where string) string {
	return fmt.Sprintf("NOT EXISTS (SELECT 1 FROM (%s) {{%s}} WHERE %s)", sub, alias, where)
}

func (dialect) ExistsNoneMany(leftSub, leftAlias, rightSub, rightAlias, where string) string {
	return fmt.Sprintf(
		"NOT EXISTS (SELECT 1 FROM (%s) {{%s}} LEFT JOIN (%s) {{%s}} WHERE %s)",
		leftSub, leftAlias, rightSub, rightAlias, where,
	)
}

func (dialect) GeoDistance(lonA, latA, lonB, latB string) (string, error) {
	// the clamping is to prevent floating point rounding errors for values like
	// "1.0002" that could occur for example when comparing identical points
	// (see the NULL note for arccosine in https://sqlite.org/lang_mathfunc.html#overview)
	return `(6371 * acos(min(1, max(-1, ` +
		`cos(radians(` + latA + `)) * cos(radians(` + latB + `)) * ` +
		`cos(radians(` + lonB + `) - radians(` + lonA + `)) + ` +
		`sin(radians(` + latA + `)) * sin(radians(` + latB + `))` +
		`))))`, nil
}

func (dialect) Strftime(args []string) (string, error) {
	return "strftime(" + joinComma(args) + ")", nil
}

func joinComma(args []string) string {
	out := ""
	for i, a := range args {
		if i > 0 {
			out += ","
		}
		out += a
	}
	return out
}
