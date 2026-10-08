//go:build !no_payments

package payments

import (
	"fmt"

	"github.com/ganigeorgiev/fexpr"
	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/tools/search"
)

// init registers the rule function entitled("key") (also spelled
// @entitled("key")) additively in search.TokenFunctions; it is SQLite only
// like every custom function. Use it as a boolean comparison, for example
//
//	entitled("pro") = true
//
// It is true when the authenticated record holds the entitlement in status
// active, trial or grace and `until` has not passed (or, with grace_seconds,
// until + grace has not passed). Guests never match.
func init() {
	search.TokenFunctions["entitled"] = entitledFunc
	search.TokenFunctions["@entitled"] = entitledFunc
}

func entitledFunc(
	resolve func(fexpr.Token) (*search.ResolverResult, error),
	args ...fexpr.Token,
) (*search.ResolverResult, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("[entitled] expected 1 argument, got %d", len(args))
	}
	if args[0].Type != fexpr.TokenText {
		return nil, fmt.Errorf("[entitled] the key must be a string literal")
	}
	key, err := resolve(args[0])
	if err != nil {
		return nil, fmt.Errorf("[entitled] failed to resolve the key: %w", err)
	}
	id, err := resolve(fexpr.Token{Type: fexpr.TokenIdentifier, Literal: "@request.auth.id"})
	if err != nil {
		return nil, fmt.Errorf("[entitled] failed to resolve the auth id: %w", err)
	}
	col, err := resolve(fexpr.Token{Type: fexpr.TokenIdentifier, Literal: "@request.auth.collectionName"})
	if err != nil {
		return nil, fmt.Errorf("[entitled] failed to resolve the auth collection: %w", err)
	}

	params := dbx.Params{}
	for _, r := range []*search.ResolverResult{key, id, col} {
		for k, v := range r.Params {
			params[k] = v
		}
	}

	ident := "(CASE WHEN " + id.Identifier + " != '' AND EXISTS (SELECT 1 FROM {{" + EntitlementsCollection + "}} AS [[_ent]] WHERE " +
		"[[_ent.subject]] = " + id.Identifier + " AND [[_ent.subject_collection]] = " + col.Identifier +
		" AND [[_ent.key]] = " + key.Identifier +
		" AND [[_ent.status]] IN ('active','trial','grace')" +
		" AND ([[_ent.until]] = '' OR [[_ent.until]] > strftime('%Y-%m-%d %H:%M:%fZ','now')" +
		" OR ([[_ent.status]] IN ('active','grace') AND [[_ent.grace_seconds]] > 0 AND" +
		" strftime('%Y-%m-%d %H:%M:%fZ', [[_ent.until]], '+' || [[_ent.grace_seconds]] || ' seconds') > strftime('%Y-%m-%d %H:%M:%fZ','now'))))" +
		" THEN 1 ELSE 0 END)"

	return &search.ResolverResult{
		NullFallback: search.NullFallbackDisabled,
		Identifier:   ident,
		Params:       params,
	}, nil
}
