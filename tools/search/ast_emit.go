package search

import (
	"fmt"
	"os"
	"strings"

	"github.com/ganigeorgiev/fexpr"
	"github.com/pocketbase/dbx"
	"github.com/spf13/cast"
	"github.com/tokibase/tokibase/kernel/rule"
	"github.com/tokibase/tokibase/tools/security"
	"github.com/tokibase/tokibase/tools/store"
)

// RuleASTEnvVar is the environment variable that switches
// [FilterData.BuildExpr] to the parse-to-AST + emit path when set to "1".
//
// The default (unset) keeps the legacy fused compiler. Both paths produce
// identical SQL and parameters (verified by the differential tests).
const RuleASTEnvVar = "TOKI_RULE_AST"

// RuleASTEnabled reports whether the AST path is enabled (read on every call).
func RuleASTEnabled() bool {
	return os.Getenv(RuleASTEnvVar) == "1"
}

// parsedFilterAST caches parsed ASTs (same role as parsedFilterData).
var parsedFilterAST = store.New(make(map[string]*rule.AST, 50))

func buildExprViaAST(raw, cacheKey string, fieldResolver FieldResolver, maxExpressions int) (dbx.Expression, error) {
	ast, ok := parsedFilterAST.GetOk(cacheKey)
	if !ok {
		var err error

		ast, err = rule.Parse(raw)
		if err != nil {
			return nil, err
		}

		// same arbitrary cache size limit as for parsedFilterData
		parsedFilterAST.SetIfLessThanLimit(cacheKey, ast, 500)
	}

	return EmitAST(ast, fieldResolver, maxExpressions)
}

// EmitAST is the SQLite emitter for a [rule.AST]. It produces exactly the
// same expression as the legacy [FilterData.BuildExprWithLimit] path by
// delegating identifier resolution to fieldResolver and reusing the shared
// operator helpers (buildResolversExpr and friends).
//
// Prefer calling it through kernel/rule/sql.Emit.
func EmitAST(ast *rule.AST, fieldResolver FieldResolver, maxExpressions int) (dbx.Expression, error) {
	if ast == nil || ast.Root == nil {
		return nil, rule.ErrEmpty
	}

	return emitGroup(ast.Root, fieldResolver, &maxExpressions)
}

func emitGroup(g *rule.Group, fieldResolver FieldResolver, maxExpressions *int) (dbx.Expression, error) {
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

			expr, exprErr = emitComparison(n, fieldResolver)
		case *rule.Group:
			expr, exprErr = emitGroup(n, fieldResolver, maxExpressions)
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

func emitComparison(c *rule.Comparison, fieldResolver FieldResolver) (dbx.Expression, error) {
	lResult, lErr := resolveOperand(c.Left, fieldResolver)
	if lErr != nil || lResult.Identifier == "" {
		return nil, fmt.Errorf("invalid left operand %q - %v", c.Left.Raw(), lErr)
	}

	rResult, rErr := resolveOperand(c.Right, fieldResolver)
	if rErr != nil || rResult.Identifier == "" {
		return nil, fmt.Errorf("invalid right operand %q - %v", c.Right.Raw(), rErr)
	}

	return buildResolversExpr(lResult, fexprOp(c.Op), rResult)
}

// resolveOperand is the AST counterpart of resolveToken.
func resolveOperand(operand rule.Operand, fieldResolver FieldResolver) (*ResolverResult, error) {
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
			for k, v := range normalizedIdentifiers {
				if strings.EqualFold(k, o.Name) {
					return &ResolverResult{Identifier: v}, nil
				}
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
		fn, ok := TokenFunctions[o.Name]
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
			return resolveOperand(argOperand, fieldResolver)
		}, tokens...)
	}

	return nil, fmt.Errorf("unsupported operand type %T", operand)
}

func fexprOp(op rule.Op) fexpr.SignOp {
	return fexpr.SignOp(op)
}
