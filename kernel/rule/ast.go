// Package rule defines the dialect-neutral AST of the Tokibase filter/rule
// language (the PocketBase "fexpr" grammar) and a parser that produces it.
//
// The package has no dependency on SQL, dbx or any store: emitters
// (SQL dialects, in-memory evaluation) consume the AST from other packages.
//
// The AST mirrors the structure produced by the upstream parser exactly
// (including the way nested groups are represented) because that structure
// determines the parenthesization of the emitted SQL.
// See docs/RULE_ENGINE.md.
package rule

import (
	"errors"

	"github.com/ganigeorgiev/fexpr"
)

// Parser sentinel errors (identical values to the upstream parser ones,
// so errors.Is works with either).
var (
	ErrEmpty          = fexpr.ErrEmpty
	ErrIncomplete     = fexpr.ErrIncomplete
	ErrInvalidComment = fexpr.ErrInvalidComment
)

// ErrUnsupportedNode is returned for node shapes the converter does not know.
var ErrUnsupportedNode = errors.New("unsupported expression item")

// JoinOp is the logical operator joining an [Item] with the previous one.
type JoinOp string

const (
	JoinAnd JoinOp = "&&"
	JoinOr  JoinOp = "||"
)

// Op is a comparison operator.
type Op string

const (
	OpEq    Op = "="
	OpNeq   Op = "!="
	OpLike  Op = "~"
	OpNlike Op = "!~"
	OpLt    Op = "<"
	OpLte   Op = "<="
	OpGt    Op = ">"
	OpGte   Op = ">="

	// "any/at-least-one-of" variants for multi-value fields.
	OpAnyEq    Op = "?="
	OpAnyNeq   Op = "?!="
	OpAnyLike  Op = "?~"
	OpAnyNlike Op = "?!~"
	OpAnyLt    Op = "?<"
	OpAnyLte   Op = "?<="
	OpAnyGt    Op = "?>"
	OpAnyGte   Op = "?>="
)

// IsAny reports whether the operator is a "?" (any-match) variant.
func (o Op) IsAny() bool {
	switch o {
	case OpAnyEq, OpAnyNeq, OpAnyLike, OpAnyNlike, OpAnyLt, OpAnyLte, OpAnyGt, OpAnyGte:
		return true
	}
	return false
}

// Base returns the operator without the "?" any-match prefix.
func (o Op) Base() Op {
	if o.IsAny() {
		return o[1:]
	}
	return o
}

// Pos locates a node for error messages.
//
// The upstream scanner does not expose byte offsets, so the position is the
// 1-based ordinal of the comparison in source order (0 = unknown).
type Pos struct {
	Expr int
}

// AST is a parsed rule/filter expression.
type AST struct {
	// Source is the exact string that was parsed.
	Source string
	// Root is the top level group (never empty for a successfully parsed AST).
	Root *Group
}

// Node is either a [*Group] or a [*Comparison].
type Node interface {
	node()
}

// Item is a node together with the operator joining it to the previous
// item of the same group (the first item's Join is ignored by emitters
// and defaults to [JoinAnd]).
type Item struct {
	Join JoinOp
	Node Node
}

// Group is a sequence of joined items, ie. the content of a pair
// of parentheses (or of the whole expression for the root).
type Group struct {
	Items []Item
}

// Comparison is a single `left op right` expression.
type Comparison struct {
	Pos   Pos
	Left  Operand
	Op    Op
	Right Operand
}

func (*Group) node()      {}
func (*Comparison) node() {}

// Operand is an [*Ident], [*Literal] or [*Call].
type Operand interface {
	operand()
	// Raw returns the operand literal as written
	// (function name for calls, unquoted value for strings).
	Raw() string
}

// IdentKind classifies an identifier. The classification is informational:
// resolvers always receive the full identifier name and decide the meaning.
type IdentKind uint8

const (
	// KindField is a plain (possibly dotted) collection field path.
	KindField IdentKind = iota
	// KindRequest is a `@request.*` identifier.
	KindRequest
	// KindCollection is a `@collection.name[:alias].*` cross-collection identifier.
	KindCollection
	// KindMacro is a time macro (`@now`, `@todayStart`, ...).
	KindMacro
	// KindKeyword is `null`, `true` or `false` (case-insensitive).
	// Note: a collection field with such a name takes precedence at emit time.
	KindKeyword
)

// Ident is an identifier operand.
type Ident struct {
	Pos Pos
	// Name is the full identifier exactly as written, eg. "@request.body.title:isset".
	Name string
	Kind IdentKind
	// Path holds the dot separated segments of Name without the leading
	// "@request"/"@collection" namespace marker and without the modifier,
	// eg. ["body", "title"] or ["users", "email"] (alias in CollectionAlias).
	Path []string
	// Modifier is one of "isset", "length", "each", "lower", "changed" or empty.
	Modifier string
	// CollectionAlias is the optional `:alias` of a `@collection.name:alias`.
	CollectionAlias string
}

// LiteralKind is the kind of a [Literal].
type LiteralKind uint8

const (
	LiteralString LiteralKind = iota
	LiteralNumber
)

// Literal is a quoted string or a number.
type Literal struct {
	Pos  Pos
	Kind LiteralKind
	// Value is the unquoted/unescaped string or the number as written.
	Value string
}

// Call is a function call, eg. geoDistance(a, b, 1, 2) or strftime('%Y', created).
type Call struct {
	Pos  Pos
	Name string
	Args []Operand
}

func (*Ident) operand()   {}
func (*Literal) operand() {}
func (*Call) operand()    {}

// Raw implements [Operand].
func (i *Ident) Raw() string { return i.Name }

// Raw implements [Operand].
func (l *Literal) Raw() string { return l.Value }

// Raw implements [Operand].
func (c *Call) Raw() string { return c.Name }

// Walk calls fn for every node in depth-first source order
// (groups before their items, comparisons before their operands are not visited).
func Walk(g *Group, fn func(Node)) {
	if g == nil {
		return
	}
	fn(g)
	for _, it := range g.Items {
		switch n := it.Node.(type) {
		case *Group:
			Walk(n, fn)
		case *Comparison:
			fn(n)
		}
	}
}

// CountComparisons returns the number of comparisons in the AST.
func (a *AST) CountComparisons() int {
	total := 0
	Walk(a.Root, func(n Node) {
		if _, ok := n.(*Comparison); ok {
			total++
		}
	})
	return total
}
