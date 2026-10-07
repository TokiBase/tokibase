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
	// path (empty path means the top level value). The segment keys are
	// already sanitized by the resolver.
	JSONExtract(column string, path []Segment) (string, error)
	// JSONExtractTyped is like JSONExtract but keeps the JSON type of the
	// member (PostgreSQL: jsonb instead of text) so it can be compared with a
	// number or boolean, see [Dialect.JSONScalar]. A dialect whose
	// JSONExtract already yields typed values returns the same expression.
	JSONExtractTyped(column string, path []Segment) (string, error)
	// JSONScalar converts a number or boolean SQL operand (t is [ValueNumber]
	// or [ValueBool]) to the type returned by JSONExtractTyped.
	JSONScalar(expr string, t ValueType) string
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
	// NativeCompare reports whether 2 columns of the same type t are compared
	// with their native type (instead of via COALESCE/text) in equality checks.
	NativeCompare(t ValueType) bool
	// EmptyFor returns the SQL for an empty (NULL or "") operand that is
	// compared with an operand of kind t (a boolean or number operand cannot
	// be compared with the string '' on every database).
	EmptyFor(t ValueType) string
	// NormalizeLikePattern makes a LIKE pattern (already wrapped/escaped by the
	// engine) valid for the database, e.g. a dangling escape character.
	NormalizeLikePattern(pattern string) string
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

// Segment is a single JSON path element.
type Segment struct {
	// Key is the sanitized object key or decimal index.
	Key string
	// Index marks a segment that was written as a decimal array index
	// (decided on the raw user input, before sanitization, like PocketBase does).
	Index bool
}

// Keys returns the segment keys.
func Keys(path []Segment) []string {
	keys := make([]string, len(path))
	for i, s := range path {
		keys[i] = s.Key
	}
	return keys
}

// ValueType is the (coarse) SQL value type of an operand.
type ValueType uint8

const (
	// ValueUnknown is an operand of unknown/mixed type.
	ValueUnknown ValueType = iota
	ValueText
	ValueNumber
	ValueBool
	ValueDate
	ValueJSON
)

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
	Alias  string    // table alias the column belongs to
	Column string    // sanitized column name
	Path   []Segment // RefJSON only
	Lower  bool      // ":lower" modifier
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
		// LOWER only exists for text on some databases
		sql = "LOWER(" + d.TextOf(sql) + ")"
	}

	return sql, nil
}

// EmitTyped is like [Ref.Emit] for a RefJSON but keeps the JSON type
// (see [Dialect.JSONExtractTyped]).
func (r Ref) EmitTyped(d Dialect) (string, error) {
	if r.Kind != RefJSON || r.Lower {
		return "", ErrUnsupported
	}

	return d.JSONExtractTyped(r.Alias+"."+r.Column, r.Path)
}

// JSONPathString renders path segments in the SQLite/JSONPath notation
// without the leading "$" ("a.b[0].c", "[0].a").
func JSONPathString(path []Segment) string {
	var sb strings.Builder

	for j, p := range path {
		if p.Index {
			sb.WriteString("[")
			sb.WriteString(p.Key)
			sb.WriteString("]")
		} else {
			if j > 0 {
				sb.WriteString(".")
			}
			sb.WriteString(p.Key)
		}
	}

	return sb.String()
}

// SegmentFromRaw builds a [Segment] from a raw user supplied path element:
// it is an index when the raw value is a decimal integer, its key is the
// sanitized value. The order (Atoi on the raw value, then sanitize) is the
// PocketBase one: "1é" is the object key "1", not the index [1].
func SegmentFromRaw(raw, sanitized string) Segment {
	_, err := strconv.Atoi(raw)
	return Segment{Key: sanitized, Index: err == nil}
}
