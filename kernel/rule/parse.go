package rule

import (
	"fmt"
	"strings"

	"github.com/ganigeorgiev/fexpr"
)

// macros lists the supported time macro identifiers (@now, @todayStart, ...).
//
// It is kept in sync with the emitter's macro table by a test in tools/search.
var macros = map[string]struct{}{
	"@now": {}, "@yesterday": {}, "@tomorrow": {}, "@second": {}, "@minute": {},
	"@hour": {}, "@day": {}, "@month": {}, "@weekday": {}, "@year": {},
	"@todayStart": {}, "@todayEnd": {}, "@monthStart": {}, "@monthEnd": {},
	"@yearStart": {}, "@yearEnd": {},
}

// IsMacro reports whether name is a time macro identifier.
func IsMacro(name string) bool {
	_, ok := macros[name]
	return ok
}

// MacroNames returns the known time macro identifiers (unordered).
func MacroNames() []string {
	names := make([]string, 0, len(macros))
	for k := range macros {
		names = append(names, k)
	}
	return names
}

// Parse parses a filter/rule string into an [AST].
//
// Tokenizing and the grammar are delegated to the upstream fexpr parser, so
// the accepted language and the returned errors are identical to it.
func Parse(expr string) (*AST, error) {
	if err := CheckLimits(expr); err != nil {
		return nil, err
	}

	data, err := fexpr.Parse(expr)
	if err != nil {
		return nil, err
	}

	counter := 0
	root, err := convertGroups(data, &counter)
	if err != nil {
		return nil, err
	}

	return &AST{Source: expr, Root: root}, nil
}

func convertGroups(data []fexpr.ExprGroup, counter *int) (*Group, error) {
	g := &Group{Items: make([]Item, 0, len(data))}

	for _, eg := range data {
		join := JoinAnd
		if eg.Join == fexpr.JoinOr {
			join = JoinOr
		}

		var node Node

		switch item := eg.Item.(type) {
		case fexpr.Expr:
			*counter++
			pos := Pos{Expr: *counter}

			left, err := FromToken(item.Left, pos)
			if err != nil {
				return nil, err
			}
			right, err := FromToken(item.Right, pos)
			if err != nil {
				return nil, err
			}

			node = &Comparison{Pos: pos, Left: left, Op: Op(item.Op), Right: right}
		case fexpr.ExprGroup:
			sub, err := convertGroups([]fexpr.ExprGroup{item}, counter)
			if err != nil {
				return nil, err
			}
			node = sub
		case []fexpr.ExprGroup:
			sub, err := convertGroups(item, counter)
			if err != nil {
				return nil, err
			}
			node = sub
		default:
			return nil, ErrUnsupportedNode
		}

		g.Items = append(g.Items, Item{Join: join, Node: node})
	}

	return g, nil
}

// FromToken converts an upstream scanner token into an [Operand].
func FromToken(t fexpr.Token, pos Pos) (Operand, error) {
	switch t.Type {
	case fexpr.TokenIdentifier:
		return newIdent(t.Literal, pos), nil
	case fexpr.TokenText:
		return &Literal{Pos: pos, Kind: LiteralString, Value: t.Literal}, nil
	case fexpr.TokenNumber:
		return &Literal{Pos: pos, Kind: LiteralNumber, Value: t.Literal}, nil
	case fexpr.TokenFunction:
		args, _ := t.Meta.([]fexpr.Token)
		call := &Call{Pos: pos, Name: t.Literal, Args: make([]Operand, 0, len(args))}
		for _, a := range args {
			op, err := FromToken(a, pos)
			if err != nil {
				return nil, err
			}
			call.Args = append(call.Args, op)
		}
		return call, nil
	}

	return nil, fmt.Errorf("unsupported token type %q", t.Type)
}

// ToToken converts an [Operand] back to the upstream scanner token
// (the inverse of [FromToken]).
func ToToken(o Operand) fexpr.Token {
	switch v := o.(type) {
	case *Ident:
		return fexpr.Token{Type: fexpr.TokenIdentifier, Literal: v.Name}
	case *Literal:
		if v.Kind == LiteralNumber {
			return fexpr.Token{Type: fexpr.TokenNumber, Literal: v.Value}
		}
		return fexpr.Token{Type: fexpr.TokenText, Literal: v.Value}
	case *Call:
		args := make([]fexpr.Token, 0, len(v.Args))
		for _, a := range v.Args {
			args = append(args, ToToken(a))
		}
		return fexpr.Token{Type: fexpr.TokenFunction, Literal: v.Name, Meta: args}
	}

	return fexpr.Token{}
}

var knownModifiers = map[string]struct{}{
	"isset": {}, "length": {}, "each": {}, "lower": {}, "changed": {},
}

func newIdent(name string, pos Pos) *Ident {
	id := &Ident{Pos: pos, Name: name, Kind: KindField}

	switch strings.ToLower(name) {
	case "null", "true", "false":
		id.Kind = KindKeyword
		return id
	}

	if IsMacro(name) {
		id.Kind = KindMacro
		return id
	}

	body := name

	// trailing modifier
	if i := strings.LastIndex(body, ":"); i >= 0 {
		if _, ok := knownModifiers[body[i+1:]]; ok {
			id.Modifier = body[i+1:]
			body = body[:i]
		}
	}

	parts := strings.Split(body, ".")

	switch parts[0] {
	case "@request":
		id.Kind = KindRequest
		id.Path = parts[1:]
	case "@collection":
		id.Kind = KindCollection
		if len(parts) > 1 {
			name, alias, _ := strings.Cut(parts[1], ":")
			id.CollectionAlias = alias
			id.Path = append([]string{name}, parts[2:]...)
		}
	default:
		id.Path = parts
	}

	return id
}
