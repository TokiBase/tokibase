package search

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/ganigeorgiev/fexpr"
	"github.com/pocketbase/dbx"
	"github.com/spf13/cast"
	"github.com/tokibase/tokibase/kernel/rule"
	"github.com/tokibase/tokibase/tools/security"
	"github.com/tokibase/tokibase/tools/store"
)

// FilterData is a filter expression string following the `fexpr` package grammar.
//
// The filter string can also contain dbx placeholder parameters (eg. "title = {:name}"),
// that will be safely replaced and properly quoted inplace with the placeholderReplacements values.
//
// Example:
//
//	var filter FilterData = "id = null || (name = 'test' && status = true) || (total >= {:min} && total <= {:max})"
//	resolver := search.NewSimpleFieldResolver("id", "name", "status")
//	expr, err := filter.BuildExpr(resolver, dbx.Params{"min": 100, "max": 200})
type FilterData string

// parsedFilterData holds a cache with previously parsed filter data expressions
// (initialized with some preallocated empty data map)
var parsedFilterData = store.New(make(map[string][]fexpr.ExprGroup, 50))

// BuildExpr parses the current filter data and returns a new db WHERE expression.
//
// The filter string can also contain dbx placeholder parameters (eg. "title = {:name}"),
// that will be safely replaced and properly quoted inplace with the placeholderReplacements values.
//
// The parsed expressions are limited up to DefaultFilterExprLimit.
// Use [FilterData.BuildExprWithLimit] if you want to set a custom limit.
func (f FilterData) BuildExpr(
	fieldResolver FieldResolver,
	placeholderReplacements ...dbx.Params,
) (dbx.Expression, error) {
	return f.BuildExprWithLimit(fieldResolver, DefaultFilterExprLimit, placeholderReplacements...)
}

// BuildExpr parses the current filter data and returns a new db WHERE expression.
//
// The filter string can also contain dbx placeholder parameters (eg. "title = {:name}"),
// that will be safely replaced and properly quoted inplace with the placeholderReplacements values.
func (f FilterData) BuildExprWithLimit(
	fieldResolver FieldResolver,
	maxExpressions int,
	placeholderReplacements ...dbx.Params,
) (dbx.Expression, error) {
	raw := string(f)

	// replace the placeholder params in the raw string filter
	if len(placeholderReplacements) > 0 {
		replacements := make([]string, 0, len(placeholderReplacements[0])*2)

		for _, p := range placeholderReplacements {
			for key, value := range p {
				var replacement string

				switch v := value.(type) {
				case nil:
					replacement = "null"
				case bool, float64, float32, int, int64, int32, int16, int8, uint, uint64, uint32, uint16, uint8:
					replacement = cast.ToString(v)
				default:
					casted, err := cast.ToStringE(v)

					// try to json serialize as fallback
					if err != nil {
						fallback, err := json.Marshal(v, json.Deterministic(true))
						if err != nil {
							return nil, fmt.Errorf("failed to serialize param %q: %w", key, err)
						}
						casted = string(fallback)
					}

					replacement = strconv.Quote(casted)
				}

				replacements = append(replacements, "{:"+key+"}", replacement)
			}
		}

		replacer := strings.NewReplacer(replacements...)
		raw = replacer.Replace(raw)
	}

	cacheKey := raw + "/" + strconv.Itoa(maxExpressions)

	// a resolver for another dialect always goes through the AST path
	// (the legacy compiler is SQLite only)
	d := sqliteDialect
	if dr, ok := fieldResolver.(DialectResolver); ok {
		d = dr.Dialect()
		if d == nil {
			return nil, errNilDialect
		}
	}

	// experimental rule AST path (see docs/RULE_ENGINE.md)
	if RuleASTEnabled() || !isSQLiteDialect(d) {
		return buildExprViaAST(d, raw, cacheKey, fieldResolver, maxExpressions)
	}

	if data, ok := parsedFilterData.GetOk(cacheKey); ok {
		return buildParsedFilterExpr(sqliteDialect, data, fieldResolver, &maxExpressions)
	}

	// same length/nesting limits as the AST path (rule.Parse)
	if err := rule.CheckLimits(raw); err != nil {
		return nil, err
	}

	data, err := fexpr.Parse(raw)
	if err != nil {
		// depending on the users demand we may allow empty expressions
		// (aka. expressions consisting only of whitespaces or comments)
		// but for now disallow them as it seems unnecessary
		// if errors.Is(err, fexpr.ErrEmpty) {
		// return dbx.NewExp("1=1"), nil
		// }

		return nil, err
	}

	// store in cache
	// (the limit size is arbitrary and it is there to prevent the cache growing too big)
	parsedFilterData.SetIfLessThanLimit(cacheKey, data, 500)

	return buildParsedFilterExpr(sqliteDialect, data, fieldResolver, &maxExpressions)
}

func buildParsedFilterExpr(d rule.Dialect, data []fexpr.ExprGroup, fieldResolver FieldResolver, maxExpressions *int) (dbx.Expression, error) {
	if len(data) == 0 {
		return nil, fexpr.ErrEmpty
	}

	result := &concatExpr{separator: " "}

	for _, group := range data {
		var expr dbx.Expression
		var exprErr error

		switch item := group.Item.(type) {
		case fexpr.Expr:
			if *maxExpressions <= 0 {
				return nil, ErrFilterExprLimit
			}

			*maxExpressions--

			expr, exprErr = resolveTokenizedExpr(d, item, fieldResolver)
		case fexpr.ExprGroup:
			expr, exprErr = buildParsedFilterExpr(d, []fexpr.ExprGroup{item}, fieldResolver, maxExpressions)
		case []fexpr.ExprGroup:
			expr, exprErr = buildParsedFilterExpr(d, item, fieldResolver, maxExpressions)
		default:
			exprErr = errors.New("unsupported expression item")
		}

		if exprErr != nil {
			return nil, exprErr
		}

		if len(result.parts) > 0 {
			var op string
			if group.Join == fexpr.JoinOr {
				op = "OR"
			} else {
				op = "AND"
			}
			result.parts = append(result.parts, &opExpr{op})
		}

		result.parts = append(result.parts, expr)
	}

	return result, nil
}

func resolveTokenizedExpr(d rule.Dialect, expr fexpr.Expr, fieldResolver FieldResolver) (dbx.Expression, error) {
	lResult, lErr := resolveToken(d, expr.Left, fieldResolver)
	if lErr != nil || lResult.Identifier == "" {
		return nil, fmt.Errorf("invalid left operand %q - %v", expr.Left.Literal, lErr)
	}

	rResult, rErr := resolveToken(d, expr.Right, fieldResolver)
	if rErr != nil || rResult.Identifier == "" {
		return nil, fmt.Errorf("invalid right operand %q - %v", expr.Right.Literal, rErr)
	}

	return buildResolversExpr(d, lResult, expr.Op, rResult)
}

func buildResolversExpr(
	d rule.Dialect,
	left *ResolverResult,
	op fexpr.SignOp,
	right *ResolverResult,
) (dbx.Expression, error) {
	var expr dbx.Expression

	left, right, err := adaptJSONOperands(d, op, left, right)
	if err != nil {
		return nil, err
	}

	switch op {
	case fexpr.SignEq, fexpr.SignAnyEq:
		expr = resolveEqualExpr(d, true, left, right)
	case fexpr.SignNeq, fexpr.SignAnyNeq:
		expr = resolveEqualExpr(d, false, left, right)
	case fexpr.SignLike, fexpr.SignAnyLike:
		expr = buildLikeExpr(d, false, left, right)
	case fexpr.SignNlike, fexpr.SignAnyNlike:
		expr = buildLikeExpr(d, true, left, right)
	case fexpr.SignLt, fexpr.SignAnyLt:
		expr = dbx.NewExp(fmt.Sprintf("%s < %s", left.Identifier, right.Identifier), mergeParams(left.Params, right.Params))
	case fexpr.SignLte, fexpr.SignAnyLte:
		expr = dbx.NewExp(fmt.Sprintf("%s <= %s", left.Identifier, right.Identifier), mergeParams(left.Params, right.Params))
	case fexpr.SignGt, fexpr.SignAnyGt:
		expr = dbx.NewExp(fmt.Sprintf("%s > %s", left.Identifier, right.Identifier), mergeParams(left.Params, right.Params))
	case fexpr.SignGte, fexpr.SignAnyGte:
		expr = dbx.NewExp(fmt.Sprintf("%s >= %s", left.Identifier, right.Identifier), mergeParams(left.Params, right.Params))
	}

	if expr == nil {
		return nil, fmt.Errorf("unknown expression operator %q", op)
	}

	// multi-match expressions
	if !isAnyMatchOp(op) {
		if left.MultiMatchSubQuery != nil && right.MultiMatchSubQuery != nil {
			mm := &manyVsManyExpr{
				d:     d,
				left:  left,
				right: right,
				op:    op,
			}

			expr = dbx.Enclose(dbx.And(expr, mm))
		} else if left.MultiMatchSubQuery != nil {
			mm := &manyVsOneExpr{
				d:            d,
				nullFallback: left.NullFallback,
				subQuery:     left.MultiMatchSubQuery,
				op:           op,
				otherOperand: right,
			}

			expr = dbx.Enclose(dbx.And(expr, mm))
		} else if right.MultiMatchSubQuery != nil {
			mm := &manyVsOneExpr{
				d:            d,
				nullFallback: right.NullFallback,
				subQuery:     right.MultiMatchSubQuery,
				op:           op,
				otherOperand: left,
				inverse:      true,
			}

			expr = dbx.Enclose(dbx.And(expr, mm))
		}
	}

	if left.AfterBuild != nil {
		expr = left.AfterBuild(expr)
	}

	if right.AfterBuild != nil {
		expr = right.AfterBuild(expr)
	}

	return expr, nil
}

// buildLikeExpr builds the ~ and !~ comparison.
func buildLikeExpr(d rule.Dialect, negate bool, left, right *ResolverResult) dbx.Expression {
	// if the right side is a column wrap it with "%" for contains like behavior
	if len(right.Params) == 0 {
		return dbx.NewExp(d.Like(left.Identifier, right.Identifier, negate, true), left.Params)
	}

	return dbx.NewExp(d.Like(left.Identifier, right.Identifier, negate, false), mergeParams(left.Params, wrapLikeParams(d, right.Params)))
}

// keywordIdentifiers are the identifiers that, if no field of that name exists,
// resolve to the dialect literal (see [rule.Dialect.Keyword]).
var keywordIdentifiers = []string{"null", "true", "false"}

// normalizeKeyword returns the literal for a missing `null`, `true` or `false` field.
func normalizeKeyword(d rule.Dialect, name string) (string, bool) {
	for _, k := range keywordIdentifiers {
		if strings.EqualFold(k, name) {
			return d.Keyword(k)
		}
	}

	return "", false
}

func resolveToken(d rule.Dialect, token fexpr.Token, fieldResolver FieldResolver) (*ResolverResult, error) {
	switch token.Type {
	case fexpr.TokenIdentifier:
		// check for macros
		// ---
		if macroFunc, ok := identifierMacros[token.Literal]; ok {
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
		// ---
		result, err := fieldResolver.Resolve(token.Literal)
		if err != nil || result.Identifier == "" {
			if v, ok := normalizeKeyword(d, token.Literal); ok {
				return &ResolverResult{Identifier: v}, nil
			}
			return nil, err
		}

		return result, err
	case fexpr.TokenText:
		placeholder := "t" + security.PseudorandomString(8)

		return &ResolverResult{
			Identifier: "{:" + placeholder + "}",
			Params:     dbx.Params{placeholder: token.Literal},
		}, nil
	case fexpr.TokenNumber:
		placeholder := "t" + security.PseudorandomString(8)

		return &ResolverResult{
			Identifier: "{:" + placeholder + "}",
			Params:     dbx.Params{placeholder: cast.ToFloat64(token.Literal)},
		}, nil
	case fexpr.TokenFunction:
		fn, ok := tokenFunctionsFor(d)[token.Literal]
		if !ok {
			if _, custom := TokenFunctions[token.Literal]; custom && !isSQLiteDialect(d) {
				return nil, fmt.Errorf("function %q: %w: only available for the sqlite dialect", token.Literal, rule.ErrUnsupported)
			}
			return nil, fmt.Errorf("unknown function %q", token.Literal)
		}

		args, _ := token.Meta.([]fexpr.Token)
		return fn(func(argToken fexpr.Token) (*ResolverResult, error) {
			return resolveToken(d, argToken, fieldResolver)
		}, args...)
	}

	return nil, fmt.Errorf("unsupported token type %q", token.Type)
}

// Resolves = and != expressions in an attempt to minimize the COALESCE
// usage and to gracefully handle null vs empty string normalizations.
//
// The expression `a = "" OR a is null` tends to perform better than
// `COALESCE(a, "") = ""` since the direct match can be accomplished
// with a seek while the COALESCE will induce a table scan.
func resolveEqualExpr(d rule.Dialect, equal bool, left, right *ResolverResult) dbx.Expression {
	concatOp := "OR"
	nullExpr := "IS NULL"
	if !equal {
		concatOp = "AND"
		nullExpr = "IS NOT NULL"
	}

	// cmp is the comparison of 2 operands.
	//
	// For non-equal it is always the null-safe form (`IS NOT` in SQLite)
	// instead of `!=` because direct non-equal comparisons
	// to nullable column values that are actually NULL yields to NULL instead of TRUE, eg.:
	// `'example' != nullableColumn` -> NULL even if nullableColumn row value is NULL
	cmp := func(l, r string) string {
		if equal {
			return l + " = " + r
		}
		return d.NullSafeEq(l, r, false)
	}

	// no coalesce fallback (eg. compare to a json field)
	// a IS b
	// a IS NOT b
	if left.NullFallback == NullFallbackDisabled ||
		right.NullFallback == NullFallbackDisabled {
		return dbx.NewExp(
			d.NullSafeEq(left.Identifier, right.Identifier, equal),
			mergeParams(left.Params, right.Params),
		)
	}

	// 2 columns of the same non-text type (number, bool, date) are compared
	// natively by the dialects that need it (a text comparison would make
	// 1.00 != 1); NULL = NULL and NULL != value keep the COALESCE semantics
	if left.Type != rule.ValueUnknown && left.Type == right.Type &&
		left.NullFallback != NullFallbackEnforced && right.NullFallback != NullFallbackEnforced &&
		d.NativeCompare(left.Type) {
		return dbx.NewExp(
			d.NullSafeEq(left.Identifier, right.Identifier, equal),
			mergeParams(left.Params, right.Params),
		)
	}

	isLeftEmpty := isEmptyIdentifier(left) ||
		(left.NullFallback == NullFallbackAuto && len(left.Params) == 1 && hasEmptyParamValue(left))

	isRightEmpty := isEmptyIdentifier(right) ||
		(right.NullFallback == NullFallbackAuto && len(right.Params) == 1 && hasEmptyParamValue(right))

	// both operands are empty
	if isLeftEmpty && isRightEmpty {
		return dbx.NewExp(cmp("''", "''"), mergeParams(left.Params, right.Params))
	}

	// direct compare since at least one of the operands is known to be non-empty
	// eg. a = 'example'
	if isKnownNonEmptyIdentifier(left) || isKnownNonEmptyIdentifier(right) {
		leftIdentifier := left.Identifier
		if isLeftEmpty {
			leftIdentifier = d.EmptyFor(scalarKind(right))
		}
		rightIdentifier := right.Identifier
		if isRightEmpty {
			rightIdentifier = d.EmptyFor(scalarKind(left))
		}

		// a typed NULL replaced the empty string (it is not comparable with
		// "=" the way '' is), so compare null-safe
		if (isLeftEmpty && leftIdentifier != "''") || (isRightEmpty && rightIdentifier != "''") {
			return dbx.NewExp(
				d.NullSafeEq(leftIdentifier, rightIdentifier, equal),
				mergeParams(left.Params, right.Params),
			)
		}

		return dbx.NewExp(
			cmp(leftIdentifier, rightIdentifier),
			mergeParams(left.Params, right.Params),
		)
	}

	// "" = b OR b IS NULL
	// "" IS NOT b AND b IS NOT NULL
	if isLeftEmpty {
		return dbx.NewExp(
			"("+cmp("''", d.TextOf(right.Identifier))+" "+concatOp+" "+right.Identifier+" "+nullExpr+")",
			mergeParams(left.Params, right.Params),
		)
	}

	// a = "" OR a IS NULL
	// a IS NOT "" AND a IS NOT NULL
	if isRightEmpty {
		return dbx.NewExp(
			"("+cmp(d.TextOf(left.Identifier), "''")+" "+concatOp+" "+left.Identifier+" "+nullExpr+")",
			mergeParams(left.Params, right.Params),
		)
	}

	// fallback to a COALESCE comparison
	return dbx.NewExp(
		cmp(d.CoalesceEmpty(left.Identifier), d.CoalesceEmpty(right.Identifier)),
		mergeParams(left.Params, right.Params),
	)
}

func hasEmptyParamValue(result *ResolverResult) bool {
	for _, p := range result.Params {
		switch v := p.(type) {
		case nil:
			return true
		case string:
			if v == "" {
				return true
			}
		}
	}

	return false
}

func isKnownNonEmptyIdentifier(result *ResolverResult) bool {
	if result.NullFallback == NullFallbackEnforced {
		return false
	}

	switch strings.ToLower(result.Identifier) {
	case "1", "0", "false", `true`:
		return true
	}

	return len(result.Params) > 0 && !hasEmptyParamValue(result) && !isEmptyIdentifier(result)
}

func isEmptyIdentifier(result *ResolverResult) bool {
	switch strings.ToLower(result.Identifier) {
	case "", "null", "''", `""`, "``":
		return true
	default:
		return false
	}
}

func isAnyMatchOp(op fexpr.SignOp) bool {
	switch op {
	case
		fexpr.SignAnyEq,
		fexpr.SignAnyNeq,
		fexpr.SignAnyLike,
		fexpr.SignAnyNlike,
		fexpr.SignAnyLt,
		fexpr.SignAnyLte,
		fexpr.SignAnyGt,
		fexpr.SignAnyGte:
		return true
	}

	return false
}

// mergeParams returns new dbx.Params where each provided params item
// is merged in the order they are specified.
func mergeParams(params ...dbx.Params) dbx.Params {
	result := dbx.Params{}

	for _, p := range params {
		for k, v := range p {
			result[k] = v
		}
	}

	return result
}

// @todo consider adding support for custom single character wildcard
//
// wrapLikeParams wraps each provided param value string with `%`
// if the param doesn't contain an explicit wildcard (`%`) character already.
func wrapLikeParams(d rule.Dialect, params dbx.Params) dbx.Params {
	result := dbx.Params{}

	for k, v := range params {
		vStr := cast.ToString(v)
		if !containsUnescapedChar(vStr, '%') {
			// note: this is done to minimize the breaking changes and to preserve the original autoescape behavior
			vStr = escapeUnescapedChars(vStr, '\\', '%', '_')
			vStr = "%" + vStr + "%"
		}
		result[k] = d.NormalizeLikePattern(vStr)
	}

	return result
}

func escapeUnescapedChars(str string, escapeChars ...rune) string {
	rs := []rune(str)
	total := len(rs)
	result := make([]rune, 0, total)

	var match bool

	for i := total - 1; i >= 0; i-- {
		if match {
			// check if already escaped
			if rs[i] != '\\' {
				result = append(result, '\\')
			}
			match = false
		} else {
			for _, ec := range escapeChars {
				if rs[i] == ec {
					match = true
					break
				}
			}
		}

		result = append(result, rs[i])

		// in case the matching char is at the beginning
		if i == 0 && match {
			result = append(result, '\\')
		}
	}

	// reverse
	for i, j := 0, len(result)-1; i < j; i, j = i+1, j-1 {
		result[i], result[j] = result[j], result[i]
	}

	return string(result)
}

func containsUnescapedChar(str string, ch rune) bool {
	var prev rune

	for _, c := range str {
		if c == ch && prev != '\\' {
			return true
		}

		if c == '\\' && prev == '\\' {
			prev = rune(0) // reset escape sequence
		} else {
			prev = c
		}
	}

	return false
}

// -------------------------------------------------------------------

var _ dbx.Expression = (*opExpr)(nil)

// opExpr defines an expression that contains a raw sql operator string.
type opExpr struct {
	op string
}

// Build converts the expression into a SQL fragment.
//
// Implements [dbx.Expression] interface.
func (e *opExpr) Build(db *dbx.DB, params dbx.Params) string {
	return e.op
}

// -------------------------------------------------------------------

var _ dbx.Expression = (*concatExpr)(nil)

// concatExpr defines an expression that concatenates multiple
// other expressions with a specified separator.
type concatExpr struct {
	separator string
	parts     []dbx.Expression
}

// Build converts the expression into a SQL fragment.
//
// Implements [dbx.Expression] interface.
func (e *concatExpr) Build(db *dbx.DB, params dbx.Params) string {
	if len(e.parts) == 0 {
		return ""
	}

	stringParts := make([]string, 0, len(e.parts))

	for _, p := range e.parts {
		if p == nil {
			continue
		}

		if sql := p.Build(db, params); sql != "" {
			stringParts = append(stringParts, sql)
		}
	}

	// skip extra parenthesis for single concat expression
	if len(stringParts) == 1 &&
		// check for already concatenated raw/plain expressions
		!strings.Contains(strings.ToUpper(stringParts[0]), " AND ") &&
		!strings.Contains(strings.ToUpper(stringParts[0]), " OR ") {
		return stringParts[0]
	}

	return "(" + strings.Join(stringParts, e.separator) + ")"
}

// -------------------------------------------------------------------

var _ dbx.Expression = (*manyVsManyExpr)(nil)

// manyVsManyExpr constructs a multi-match many<->many db where expression.
//
// Expects leftSubQuery and rightSubQuery to return a subquery with a
// single "multiMatchValue" column.
type manyVsManyExpr struct {
	d     rule.Dialect
	left  *ResolverResult
	right *ResolverResult
	op    fexpr.SignOp
}

// Build converts the expression into a SQL fragment.
//
// Implements [dbx.Expression] interface.
func (e *manyVsManyExpr) Build(db *dbx.DB, params dbx.Params) string {
	if e.left.MultiMatchSubQuery == nil || e.right.MultiMatchSubQuery == nil {
		return "0=1"
	}

	lAlias := "__ml" + security.PseudorandomString(8)
	rAlias := "__mr" + security.PseudorandomString(8)

	whereExpr, buildErr := buildResolversExpr(
		e.d,
		&ResolverResult{
			NullFallback: e.left.NullFallback,
			Identifier:   "[[" + lAlias + ".multiMatchValue]]",
		},
		e.op,
		&ResolverResult{
			NullFallback: e.right.NullFallback,
			Identifier:   "[[" + rAlias + ".multiMatchValue]]",
			// note: the AfterBuild needs to be handled only once and it
			// doesn't matter whether it is applied on the left or right subquery operand
			AfterBuild: dbx.Not, // inverse for the not-exist expression
		},
	)

	if buildErr != nil {
		return "0=1"
	}

	return e.d.ExistsNoneMany(
		e.left.MultiMatchSubQuery.Build(db, params),
		lAlias,
		e.right.MultiMatchSubQuery.Build(db, params),
		rAlias,
		whereExpr.Build(db, params),
	)
}

// -------------------------------------------------------------------

var _ dbx.Expression = (*manyVsOneExpr)(nil)

// manyVsOneExpr constructs a multi-match many<->one db where expression.
//
// Expects subQuery to return a subquery with a single "multiMatchValue" column.
//
// You can set inverse=false to reverse the condition sides (aka. one<->many).
type manyVsOneExpr struct {
	d            rule.Dialect
	otherOperand *ResolverResult
	subQuery     dbx.Expression
	op           fexpr.SignOp
	inverse      bool
	nullFallback NullFallbackPreference
}

// Build converts the expression into a SQL fragment.
//
// Implements [dbx.Expression] interface.
func (e *manyVsOneExpr) Build(db *dbx.DB, params dbx.Params) string {
	if e.subQuery == nil {
		return "0=1"
	}

	alias := "__sm" + security.PseudorandomString(8)

	r1 := &ResolverResult{
		NullFallback: e.nullFallback,
		Identifier:   "[[" + alias + ".multiMatchValue]]",
		AfterBuild:   dbx.Not, // inverse for the not-exist expression
	}

	r2 := &ResolverResult{
		Identifier: e.otherOperand.Identifier,
		Params:     e.otherOperand.Params,
	}

	var whereExpr dbx.Expression
	var buildErr error

	if e.inverse {
		whereExpr, buildErr = buildResolversExpr(e.d, r2, e.op, r1)
	} else {
		whereExpr, buildErr = buildResolversExpr(e.d, r1, e.op, r2)
	}

	if buildErr != nil {
		return "0=1"
	}

	return e.d.ExistsNone(
		e.subQuery.Build(db, params),
		alias,
		whereExpr.Build(db, params),
	)
}

var errNilDialect = errors.New("the field resolver returned a nil dialect")

// isSQLiteDialect reports whether d is the SQLite dialect (compared by
// identity when the dynamic type allows it, so a dialect that merely reuses
// the name "sqlite" doesn't pass).
func isSQLiteDialect(d rule.Dialect) bool {
	return sameDialect(d, sqliteDialect)
}

// sameDialect compares 2 dialects without panicking on non-comparable types.
func sameDialect(a, b rule.Dialect) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}

	ta, tb := reflect.TypeOf(a), reflect.TypeOf(b)
	if ta != tb {
		return false
	}

	if ta.Comparable() {
		return a == b
	}

	return a.Name() == b.Name()
}

// scalarKind returns the number/bool kind of an operand (unknown otherwise).
func scalarKind(r *ResolverResult) rule.ValueType {
	if r.Type == rule.ValueNumber || r.Type == rule.ValueBool {
		return r.Type
	}

	if len(r.Params) == 0 {
		switch strings.ToLower(r.Identifier) {
		case "true", "false":
			return rule.ValueBool
		}
		if _, err := strconv.Atoi(r.Identifier); err == nil {
			return rule.ValueNumber
		}
		return rule.ValueUnknown
	}

	if len(r.Params) != 1 {
		return rule.ValueUnknown
	}

	for k, v := range r.Params {
		if r.Identifier != "{:"+k+"}" {
			return rule.ValueUnknown
		}
		switch v.(type) {
		case bool:
			return rule.ValueBool
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
			return rule.ValueNumber
		}
	}

	return rule.ValueUnknown
}

// adaptJSONOperands switches a JSON member that is compared with a number or
// boolean to its typed form (and converts the other side), for dialects whose
// plain JSON extraction yields text.
func adaptJSONOperands(d rule.Dialect, op fexpr.SignOp, left, right *ResolverResult) (*ResolverResult, *ResolverResult, error) {
	switch op {
	case fexpr.SignLike, fexpr.SignAnyLike, fexpr.SignNlike, fexpr.SignAnyNlike:
		return left, right, nil
	}

	adapt := func(j, other *ResolverResult) (*ResolverResult, *ResolverResult, bool, error) {
		kind := scalarKind(other)
		if j.JSONTyped == "" || j.JSONTyped == j.Identifier || (kind != rule.ValueNumber && kind != rule.ValueBool) {
			return j, other, false, nil
		}

		if j.MultiMatchSubQuery != nil || other.MultiMatchSubQuery != nil {
			return nil, nil, false, fmt.Errorf("%w: %s: comparing a JSON member of a multi-match path with a number or boolean", rule.ErrUnsupported, d.Name())
		}

		jc, oc := *j, *other
		jc.Identifier = j.JSONTyped
		oc.Identifier = d.JSONScalar(other.Identifier, kind)

		return &jc, &oc, true, nil
	}

	if l, r, ok, err := adapt(left, right); err != nil || ok {
		return l, r, err
	}

	if r, l, ok, err := adapt(right, left); err != nil || ok {
		return l, r, err
	}

	return left, right, nil
}
