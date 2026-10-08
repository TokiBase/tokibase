//go:build !no_roles

package roles

import (
	"fmt"
	"strings"

	"github.com/ganigeorgiev/fexpr"
	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/tools/search"
	"github.com/tokibase/tokibase/tools/security"
	"github.com/tokibase/tokibase/tools/types"
)

// Rule function names. A function cannot stand alone in a rule expression, so
// compare it: @role("admin") = true.
const (
	FuncRole   = "@role"
	FuncMember = "@member"
)

func init() {
	// both the legacy and the AST compiler resolve functions through this map
	search.TokenFunctions[FuncRole] = roleFunc
	search.TokenFunctions[FuncMember] = memberFunc
}

type resolveFn = func(fexpr.Token) (*search.ResolverResult, error)

// @role("name") | @role("name", scope, "collection")
//
// A scoped grant is matched on the scope record id AND its collection (record
// ids are only unique per collection), so the collection of the scope is a
// mandatory string literal (collection name or id).
func roleFunc(resolve resolveFn, args ...fexpr.Token) (*search.ResolverResult, error) {
	if len(args) != 1 && len(args) != 3 {
		return nil, fmt.Errorf("[%s] expected 1 or 3 arguments (name, [scope, \"scopeCollection\"]), got %d", FuncRole, len(args))
	}
	if args[0].Type != fexpr.TokenText || strings.TrimSpace(args[0].Literal) == "" {
		return nil, fmt.Errorf("[%s] the role name must be a non-empty string literal", FuncRole)
	}
	var scope, scopeCol *fexpr.Token
	if len(args) == 3 {
		scope, scopeCol = &args[1], &args[2]
	}
	return build(resolve, FuncRole, args[0].Literal, scope, scopeCol)
}

// @member(scope, "collection")
func memberFunc(resolve resolveFn, args ...fexpr.Token) (*search.ResolverResult, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("[%s] expected 2 arguments (scope, \"scopeCollection\"), got %d", FuncMember, len(args))
	}
	return build(resolve, FuncMember, "", &args[0], &args[1])
}

// build emits an EXISTS subquery over the memberships of the requesting auth
// record. Every value is a bound parameter. A guest has an empty auth id, which
// never matches (the id is also checked for non-emptiness explicitly).
func build(resolve resolveFn, fn, role string, scope, scopeCol *fexpr.Token) (*search.ResolverResult, error) {
	authId, err := resolve(fexpr.Token{Type: fexpr.TokenIdentifier, Literal: "@request.auth.id"})
	if err != nil || authId.Identifier == "" {
		return nil, fmt.Errorf("[%s] failed to resolve @request.auth.id: %v", fn, err)
	}
	authCol, err := resolve(fexpr.Token{Type: fexpr.TokenIdentifier, Literal: "@request.auth.collectionId"})
	if err != nil || authCol.Identifier == "" {
		return nil, fmt.Errorf("[%s] failed to resolve @request.auth.collectionId: %v", fn, err)
	}

	params := dbx.Params{}
	for _, r := range []*search.ResolverResult{authId, authCol} {
		for k, v := range r.Params {
			params[k] = v
		}
	}

	m := "tkm" + security.PseudorandomString(6)
	rl := "tkr" + security.PseudorandomString(6)
	nowP := "tkn" + security.PseudorandomString(8)
	params[nowP] = types.NowDateTime().String()

	var sb strings.Builder
	sb.WriteString("EXISTS (SELECT 1 FROM {{" + MembershipsName + "}} AS " + m)
	if role != "" {
		sb.WriteString(" INNER JOIN {{" + RolesName + "}} AS " + rl + " ON [[" + rl + ".id]] = [[" + m + ".role]]")
	}
	sb.WriteString(" WHERE [[" + m + ".user]] = " + authId.Identifier +
		" AND " + authId.Identifier + " != ''" +
		" AND [[" + m + ".user_collection]] = " + authCol.Identifier)
	if role != "" {
		rp := "tkp" + security.PseudorandomString(8)
		params[rp] = role
		sb.WriteString(" AND [[" + rl + ".name]] = {:" + rp + "}")
	}

	if scope == nil {
		sb.WriteString(" AND [[" + m + ".scope]] = ''")
	} else {
		switch scope.Type {
		case fexpr.TokenText, fexpr.TokenIdentifier:
		default:
			return nil, fmt.Errorf("[%s] the scope must be a field identifier or a string literal", fn)
		}
		sr, err := resolve(*scope)
		if err != nil || sr.Identifier == "" {
			return nil, fmt.Errorf("[%s] failed to resolve the scope: %v", fn, err)
		}
		if sr.MultiMatchSubQuery != nil {
			return nil, fmt.Errorf("[%s] a multi-value field cannot be used as scope", fn)
		}
		for k, v := range sr.Params {
			params[k] = v
		}
		sb.WriteString(" AND [[" + m + ".scope]] = " + sr.Identifier + " AND " + sr.Identifier + " != ''")
		if scopeCol == nil || scopeCol.Type != fexpr.TokenText || strings.TrimSpace(scopeCol.Literal) == "" {
			return nil, fmt.Errorf("[%s] the scope collection must be a non-empty string literal (collection name or id)", fn)
		}
		cp := "tkc" + security.PseudorandomString(8)
		params[cp] = strings.TrimSpace(scopeCol.Literal)
		sb.WriteString(" AND [[" + m + ".scope_collection]] IN (SELECT [[id]] FROM {{_collections}} WHERE [[id]] = {:" + cp + "}" +
			" OR [[name]] = {:" + cp + "})")
	}
	sb.WriteString(" AND ([[" + m + ".expires]] = '' OR [[" + m + ".expires]] > {:" + nowP + "}))")

	return &search.ResolverResult{
		NullFallback: search.NullFallbackDisabled,
		Identifier:   sb.String(),
		Params:       params,
	}, nil
}
