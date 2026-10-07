// Package sql emits SQL (dbx expressions) from a [rule.AST].
//
// The SQLite emitter currently lives in tools/search (it shares the
// operator helpers, the null-fallback logic and the multi-match subquery
// builders with the legacy compiler, see docs/RULE_ENGINE.md); this package is
// its stable entry point.
package sql

import (
	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/kernel/rule"
	"github.com/tokibase/tokibase/tools/search"
)

// Emit converts ast into a dbx WHERE expression using the SQLite dialect.
//
// Identifier resolution is delegated to resolver, exactly like
// [search.FilterData.BuildExpr] does. The expression count is limited to
// [search.DefaultFilterExprLimit].
func Emit(ast *rule.AST, resolver search.FieldResolver) (dbx.Expression, error) {
	return search.EmitAST(ast, resolver, search.DefaultFilterExprLimit)
}

// EmitWithLimit is like [Emit] with a custom max comparisons limit.
func EmitWithLimit(ast *rule.AST, resolver search.FieldResolver, maxExpressions int) (dbx.Expression, error) {
	return search.EmitAST(ast, resolver, maxExpressions)
}
