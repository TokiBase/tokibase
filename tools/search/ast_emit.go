package search

import (
	"errors"
	"fmt"
	"os"
	"sync/atomic"

	"github.com/ganigeorgiev/fexpr"
	"github.com/pocketbase/dbx"
	"github.com/spf13/cast"
	"github.com/tokibase/tokibase/kernel/rule"
	rulesqlite "github.com/tokibase/tokibase/kernel/rule/sqlite"
	"github.com/tokibase/tokibase/tools/security"
	"github.com/tokibase/tokibase/tools/store"
)

// RuleASTEnvVar is the environment variable that switches
// [FilterData.BuildExpr] to the parse-to-AST + emit path when set to "1".
//
// The default (unset) keeps the legacy fused compiler. Both paths produce
// identical SQL and parameters (verified by the differential tests).
const RuleASTEnvVar = "TOKI_RULE_AST"

// ruleASTOn is read from the environment once at process start, so the
// compiler cannot flip mid-process (in-process os.Setenv has no effect) and
// the hot path does not touch the environment lock. Tests use
// setRuleASTForTest (export_test.go).
var ruleASTOn atomic.Bool

func init() {
	ruleASTOn.Store(os.Getenv(RuleASTEnvVar) == "1")
}

// RuleASTEnabled reports whether the AST path is enabled (decided once at init).
func RuleASTEnabled() bool {
	return ruleASTOn.Load()
}

// sqliteDialect is the dialect of the legacy path and the default of the AST path.
var sqliteDialect = rulesqlite.Dialect

// parsedFilterAST caches parsed ASTs (same role as parsedFilterData).
var parsedFilterAST = store.New(make(map[string]*rule.AST, 50))

func buildExprViaAST(d rule.Dialect, raw, cacheKey string, fieldResolver FieldResolver, maxExpressions int) (dbx.Expression, error) {
	ast, ok := parsedFilterAST.GetOk(cacheKey)
	if !ok {
		var err error

		ast, err = rule.Parse(raw)
		if err != nil {
			return nil, err
		}

		// same arbitrary cache size limit as for parsedFilterData
		// (large filters are not cached: an AST is much bigger than its source)
		if len(raw) <= 4096 {
			parsedFilterAST.SetIfLessThanLimit(cacheKey, ast, 500)
		}
	}

	return EmitASTWithDialect(ast, fieldResolver, maxExpressions, d)
}

// EmitAST is the SQLite emitter for a [rule.AST]. It produces exactly the
// same expression as the legacy [FilterData.BuildExprWithLimit] path by
// delegating identifier resolution to fieldResolver and reusing the shared
// operator helpers (buildResolversExpr and friends).
//
// Prefer calling it through kernel/rule/sql.Emit.
func EmitAST(ast *rule.AST, fieldResolver FieldResolver, maxExpressions int) (dbx.Expression, error) {
	return EmitASTWithDialect(ast, fieldResolver, maxExpressions, sqliteDialect)
}

// DialectResolver is optionally implemented by a [FieldResolver] that produces
// dialect specific SQL; the emitters use it to reject a mismatching pair.
type DialectResolver interface {
	Dialect() rule.Dialect
}

// EmitASTWithDialect is like [EmitAST] for an arbitrary [rule.Dialect].
//
// The fieldResolver must resolve identifiers for the same dialect.
func EmitASTWithDialect(ast *rule.AST, fieldResolver FieldResolver, maxExpressions int, d rule.Dialect) (dbx.Expression, error) {
	if ast == nil || ast.Root == nil {
		return nil, rule.ErrEmpty
	}

	if d == nil {
		d = sqliteDialect
	}

	if dr, ok := fieldResolver.(DialectResolver); ok && dr.Dialect().Name() != d.Name() {
		return nil, fmt.Errorf("the field resolver dialect %q doesn't match the emitter dialect %q", dr.Dialect().Name(), d.Name())
	}

	return emitGroup(d, ast.Root, fieldResolver, &maxExpressions)
}

func emitGroup(d rule.Dialect, g *rule.Group, fieldResolver FieldResolver, maxExpressions *int) (dbx.Expression, error) {
	if len(g.Items) == 0 {
		return nil, rule.ErrEmpty
	}

	result := &concatExpr{separator: " "}

	for _, item := range g.Items {
		var expr dbx.Expression
		var exprErr error

		switch n := item.Node.(type) {
		case *rule.Comparison:
			if *maxExpressions <= 0 {
				return nil, ErrFilterExprLimit
			}

			*maxExpressions--

			expr, exprErr = emitComparison(d, n, fieldResolver)
		case *rule.Group:
			expr, exprErr = emitGroup(d, n, fieldResolver, maxExpressions)
		default:
			exprErr = rule.ErrUnsupportedNode
		}

		if exprErr != nil {
			return nil, exprErr
		}

		if len(result.parts) > 0 {
			op := "AND"
			if item.Join == rule.JoinOr {
				op = "OR"
			}
			result.parts = append(result.parts, &opExpr{op})
		}

		result.parts = append(result.parts, expr)
	}

	return result, nil
}

func emitComparison(d rule.Dialect, c *rule.Comparison, fieldResolver FieldResolver) (dbx.Expression, error) {
	lResult, lErr := resolveOperand(d, c.Left, fieldResolver)
	if lErr != nil || lResult.Identifier == "" {
		return nil, operandError("left", c.Left.Raw(), lErr)
	}

	rResult, rErr := resolveOperand(d, c.Right, fieldResolver)
	if rErr != nil || rResult.Identifier == "" {
		return nil, operandError("right", c.Right.Raw(), rErr)
	}

	return buildResolversExpr(d, lResult, fexprOp(c.Op), rResult)
}

// operandError formats like the legacy path (the text is identical) but keeps
// [rule.ErrUnsupported] reachable with errors.Is.
func operandError(side, raw string, err error) error {
	if errors.Is(err, rule.ErrUnsupported) {
		return fmt.Errorf("invalid %s operand %q - %w", side, raw, err)
	}

	return fmt.Errorf("invalid %s operand %q - %v", side, raw, err)
}

// resolveOperand is the AST counterpart of resolveToken.
func resolveOperand(d rule.Dialect, operand rule.Operand, fieldResolver FieldResolver) (*ResolverResult, error) {
	switch o := operand.(type) {
	case *rule.Ident:
		// time macros
		if macroFunc, ok := identifierMacros[o.Name]; ok {
			placeholder := "t" + security.PseudorandomString(8)

			macroValue, err := macroFunc()
			if err != nil {
				return nil, err
			}

			return &ResolverResult{
				Identifier: "{:" + placeholder + "}",
				Params:     dbx.Params{placeholder: macroValue},
			}, nil
		}

		// custom resolver
		result, err := fieldResolver.Resolve(o.Name)
		if err != nil || result.Identifier == "" {
			if v, ok := normalizeKeyword(d, o.Name); ok {
				return &ResolverResult{Identifier: v}, nil
			}
			return nil, err
		}

		return result, err
	case *rule.Literal:
		placeholder := "t" + security.PseudorandomString(8)

		if o.Kind == rule.LiteralNumber {
			return &ResolverResult{
				Identifier: "{:" + placeholder + "}",
				Params:     dbx.Params{placeholder: cast.ToFloat64(o.Value)},
			}, nil
		}

		return &ResolverResult{
			Identifier: "{:" + placeholder + "}",
			Params:     dbx.Params{placeholder: o.Value},
		}, nil
	case *rule.Call:
		fn, ok := tokenFunctionsFor(d)[o.Name]
		if !ok {
			return nil, fmt.Errorf("unknown function %q", o.Name)
		}

		// the registered functions are token based (public API), so convert
		// the arguments back and resolve nested operands through the AST path.
		args := rule.ToToken(o).Meta
		tokens, _ := args.([]fexpr.Token)

		return fn(func(argToken fexpr.Token) (*ResolverResult, error) {
			argOperand, err := rule.FromToken(argToken, o.Pos)
			if err != nil {
				return nil, err
			}
			return resolveOperand(d, argOperand, fieldResolver)
		}, tokens...)
	}

	return nil, fmt.Errorf("unsupported operand type %T", operand)
}

func fexprOp(op rule.Op) fexpr.SignOp {
	return fexpr.SignOp(op)
}
