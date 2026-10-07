package search_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/ganigeorgiev/fexpr"
	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/tools/search"
)

// beforeBuildResolver marks the field "secret" with a BeforeBuild hook.
type beforeBuildResolver struct {
	*search.SimpleFieldResolver
	calls *[]string
	err   error
}

func (r beforeBuildResolver) Resolve(field string) (*search.ResolverResult, error) {
	res, err := r.SimpleFieldResolver.Resolve(field)
	if err != nil || field != "secret" {
		return res, err
	}
	res.BeforeBuild = func(other *search.ResolverResult, op fexpr.SignOp) error {
		*r.calls = append(*r.calls, string(op)+" "+other.Identifier)
		if r.err != nil {
			return r.err
		}
		res.Identifier = "(CASE WHEN [[id]] = 'x' THEN " + other.Identifier + " END)"
		return nil
	}
	return res, nil
}

func TestResolverResultBeforeBuild(t *testing.T) {
	for _, expr := range []string{`secret = "a"`, `"a" = secret`} {
		var calls []string
		r := beforeBuildResolver{search.NewSimpleFieldResolver("secret", "id"), &calls, nil}
		e, err := search.FilterData(expr).BuildExpr(r)
		if err != nil {
			t.Fatal(err)
		}
		sql := e.Build(&dbx.DB{}, dbx.Params{})
		if len(calls) != 1 || !strings.Contains(sql, "CASE WHEN") {
			t.Fatalf("%s: calls=%v sql=%s", expr, calls, sql)
		}
	}

	var calls []string
	boom := errors.New("boom")
	r := beforeBuildResolver{search.NewSimpleFieldResolver("secret", "id"), &calls, boom}
	if _, err := search.FilterData(`secret = "a"`).BuildExpr(r); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected the hook error, got %v", err)
	}
}
