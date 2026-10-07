package rule

import (
	"errors"
	"strconv"
	"strings"
)

// ErrUnsupported is wrapped by every error a [Dialect] returns for a
// construct it cannot express. Dialects must fail with it instead of emitting
// SQL that silently means something different.
var ErrUnsupported = errors.New("rule: construct not supported by the SQL dialect")

// Dialect abstracts the SQL fragments of the rule engine that are not
// portable between databases.
//
// Everything that is plain SQL (comparison operators, AND/OR, parentheses,
// "IS NULL", placeholders "{:name}") is not part of the interface.
// Identifier quoting is not part of it either: fragments carry dbx markers
// ("[[alias.column]]" and "{{table}}") that the dbx driver in use quotes.
//
// Column arguments are unquoted "alias.column" pairs.
//
// The interface is intentionally narrow and may grow; implement it by
// embedding nothing, so that new methods break the build instead of silently
// defaulting to SQLite behavior.
type Dialect interface {
	// Name is a short identifier ("sqlite", "postgres").
	Name() string

	// JSONExtract returns a scalar expression for the JSON value of column at
	// path (empty path means the top level value). path elements are object
	// keys or decimal array indexes, already sanitized by the resolver.
	JSONExtract(column string, path []string) (string, error)
	// JSONArrayLength returns an integer expression: the length of the JSON
	// array stored in column; 0 for NULL/empty and 1 for a non-array scalar.
	JSONArrayLength(column string) (string, error)
	// JSONEach returns a table expression that yields the elements of column
	// (or the column itself as a single element when it is not an array) in a
	// "value" column. It is used as a JOIN source.
	JSONEach(column string) (string, error)
	// JSONEachParam is like JSONEach for a JSON array bound to the {:placeholder}.
	JSONEachParam(placeholder string) (string, error)
	// JSONArrayMember returns the join condition "idColumn is one of the
	// elements of jsonColumn" (jeAlias is a unique alias the dialect may use).
	JSONArrayMember(idColumn, jeAlias, jsonColumn string) (string, error)

	// Keyword resolves the null/true/false fallbacks that are used when no
	// field of that name exists. name is lower case.
	Keyword(name string) (string, bool)
	// TextOf converts expr to a text value (identity when the database
	// compares mixed types implicitly).
	TextOf(expr string) string
	// CoalesceEmpty maps NULL to the empty string: COALESCE(expr, '').
	CoalesceEmpty(expr string) string
	// NullSafeEq is the NULL-safe (in)equality "left IS right" / "left IS NOT right".
	NullSafeEq(left, right string, equal bool) string
	// Like is the case insensitive LIKE with backslash as escape character.
	// When rightIsColumn the right side is wrapped with '%' in SQL, otherwise
	// the engine already wrapped the bound parameter.
	Like(left, right string, negate, rightIsColumn bool) string

	// OptionalOn is appended after a "LEFT JOIN x y" that has no ON clause.
	OptionalOn() string
	// ExistsNone builds the "no row of sub satisfies where" check of a
	// many<->one multi-match condition (sub is a SELECT yielding "multiMatchValue").
	ExistsNone(sub, alias, where string) string
	// ExistsNoneMany is ExistsNone for many<->many conditions.
	ExistsNoneMany(leftSub, leftAlias, rightSub, rightAlias, where string) string

	// GeoDistance is the Haversine distance in km between 2 points.
	GeoDistance(lonA, latA, lonB, latB string) (string, error)
	// Strftime formats a time value; args are already resolved SQL
	// expressions (format, [time, modifiers...]).
	Strftime(args []string) (string, error)
}

// RefKind is the value shape of a resolved identifier.
type RefKind uint8

const (
	// RefColumn is a plain column.
	RefColumn RefKind = iota
	// RefJSON is a (path into a) JSON column.
	RefJSON
	// RefArrayLength is the length of a multi-value column (":length").
	RefArrayLength
)

// Ref is the typed result of resolving the final segment of an identifier:
// which table alias, column, shape and modifier it denotes. It carries no SQL;
// [Ref.Emit] renders it for a [Dialect].
type Ref struct {
	Kind   RefKind
	Alias  string   // table alias the column belongs to
	Column string   // sanitized column name
	Path   []string // RefJSON only
	Lower  bool     // ":lower" modifier
}

// Emit renders the reference as a SQL expression.
func (r Ref) Emit(d Dialect) (string, error) {
	col := r.Alias + "." + r.Column

	var (
		sql string
		err error
	)

	switch r.Kind {
	case RefColumn:
		sql = "[[" + col + "]]"
	case RefJSON:
		sql, err = d.JSONExtract(col, r.Path)
	case RefArrayLength:
		sql, err = d.JSONArrayLength(col)
	default:
		err = ErrUnsupported
	}

	if err != nil {
		return "", err
	}

	if r.Lower {
		sql = "LOWER(" + sql + ")"
	}

	return sql, nil
}

// JSONPathString renders path segments in the SQLite/JSONPath notation
// without the leading "$" ("a.b[0].c", "[0].a").
func JSONPathString(path []string) string {
	var sb strings.Builder

	for j, p := range path {
		if _, err := strconv.Atoi(p); err == nil {
			sb.WriteString("[")
			sb.WriteString(p)
			sb.WriteString("]")
		} else {
			if j > 0 {
				sb.WriteString(".")
			}
			sb.WriteString(p)
		}
	}

	return sb.String()
}
