package pg

import (
	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/kernel/rule"
	"github.com/tokibase/tokibase/tools/search"
)

// Emit converts ast into a dbx WHERE expression using the PostgreSQL dialect.
//
// resolver must produce PostgreSQL fragments too: for the record resolver
// call SetDialect([pg.Dialect]) on it first (Emit rejects a resolver that
// reports a different dialect). The expression count is limited to
// [search.DefaultFilterExprLimit].
func Emit(ast *rule.AST, resolver search.FieldResolver) (dbx.Expression, error) {
	return search.EmitASTWithDialect(ast, resolver, search.DefaultFilterExprLimit, Dialect)
}

// EmitWithLimit is like [Emit] with a custom max comparisons limit.
func EmitWithLimit(ast *rule.AST, resolver search.FieldResolver, maxExpressions int) (dbx.Expression, error) {
	return search.EmitASTWithDialect(ast, resolver, maxExpressions, Dialect)
}
