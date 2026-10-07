package rule_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ganigeorgiev/fexpr"
	"github.com/tokibase/tokibase/kernel/rule"
)

func TestParseErrorsMatchUpstream(t *testing.T) {
	for _, s := range []string{
		``, `  `, `// c`, `a`, `a =`, `= 'a'`, `a == 1`, `(a = 1`, `a = 1)`, `a = 'x`, `a = 1 &&`, `a = 1 b = 2`,
		`f(1 = 1`, `a = f(f(f(f(1)))) `, `a = 1 /* x`,
	} {
		_, want := fexpr.Parse(s)
		_, got := rule.Parse(s)
		if want == nil || got == nil || want.Error() != got.Error() {
			t.Errorf("%q: upstream %v, got %v", s, want, got)
		}
	}

	if _, err := rule.Parse(""); !errors.Is(err, rule.ErrEmpty) {
		t.Fatalf("expected ErrEmpty, got %v", err)
	}
	if _, err := rule.Parse("a = "); !errors.Is(err, rule.ErrIncomplete) {
		t.Fatalf("expected ErrIncomplete, got %v", err)
	}
}

func TestParseStructure(t *testing.T) {
	ast, err := rule.Parse(`a = 1 && (b ?~ 'x' || @request.body.c:isset = true) || geoDistance(p.lon, p.lat, 1, 2) < 3`)
	if err != nil {
		t.Fatal(err)
	}

	if n := ast.CountComparisons(); n != 4 {
		t.Fatalf("expected 4 comparisons, got %d", n)
	}
	if len(ast.Root.Items) != 3 {
		t.Fatalf("expected 3 root items, got %d", len(ast.Root.Items))
	}
	if ast.Root.Items[1].Join != rule.JoinAnd || ast.Root.Items[2].Join != rule.JoinOr {
		t.Fatalf("unexpected joins: %+v", ast.Root.Items)
	}

	grp, ok := ast.Root.Items[1].Node.(*rule.Group)
	if !ok || len(grp.Items) != 2 {
		t.Fatalf("expected nested group with 2 items, got %#v", ast.Root.Items[1].Node)
	}

	c1 := grp.Items[0].Node.(*rule.Comparison)
	if c1.Op != rule.OpAnyLike || !c1.Op.IsAny() || c1.Op.Base() != rule.OpLike {
		t.Fatalf("unexpected op %q", c1.Op)
	}
	if lit := c1.Right.(*rule.Literal); lit.Kind != rule.LiteralString || lit.Value != "x" {
		t.Fatalf("unexpected literal %+v", lit)
	}

	c2 := grp.Items[1].Node.(*rule.Comparison)
	id := c2.Left.(*rule.Ident)
	if id.Kind != rule.KindRequest || id.Modifier != "isset" || !reflect.DeepEqual(id.Path, []string{"body", "c"}) {
		t.Fatalf("unexpected ident %+v", id)
	}
	if kw := c2.Right.(*rule.Ident); kw.Kind != rule.KindKeyword {
		t.Fatalf("expected keyword ident, got %+v", kw)
	}

	c3 := ast.Root.Items[2].Node.(*rule.Comparison)
	call := c3.Left.(*rule.Call)
	if call.Name != "geoDistance" || len(call.Args) != 4 {
		t.Fatalf("unexpected call %+v", call)
	}
	if c3.Pos.Expr != 4 {
		t.Fatalf("unexpected position %+v", c3.Pos)
	}
}

func TestIdentClassification(t *testing.T) {
	cases := []struct {
		in       string
		kind     rule.IdentKind
		path     []string
		modifier string
		alias    string
	}{
		{"title", rule.KindField, []string{"title"}, "", ""},
		{"a.b.c:each", rule.KindField, []string{"a", "b", "c"}, "each", ""},
		{"a:length", rule.KindField, []string{"a"}, "length", ""},
		{"a:lower", rule.KindField, []string{"a"}, "lower", ""},
		{"@now", rule.KindMacro, nil, "", ""},
		{"@todayStart", rule.KindMacro, nil, "", ""},
		{"NULL", rule.KindKeyword, nil, "", ""},
		{"@collection.users:al.email", rule.KindCollection, []string{"users", "email"}, "", "al"},
		{"@request.auth.id", rule.KindRequest, []string{"auth", "id"}, "", ""},
		{"@request.body.x:isset", rule.KindRequest, []string{"body", "x"}, "isset", ""},
	}

	for _, c := range cases {
		ast, err := rule.Parse(c.in + " = 1")
		if err != nil {
			t.Fatalf("%q: %v", c.in, err)
		}
		id := ast.Root.Items[0].Node.(*rule.Comparison).Left.(*rule.Ident)
		if id.Kind != c.kind || id.Modifier != c.modifier || id.CollectionAlias != c.alias || !reflect.DeepEqual(id.Path, c.path) {
			t.Errorf("%q: got %+v", c.in, id)
		}
		if id.Name != c.in {
			t.Errorf("%q: name %q", c.in, id.Name)
		}
	}
}

func TestTokenRoundTrip(t *testing.T) {
	data, err := fexpr.Parse(`strftime('%Y', a, '+1 day') = 1 && b > 2.5 && c ~ "x"`)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range data {
		e := g.Item.(fexpr.Expr)
		for _, tok := range []fexpr.Token{e.Left, e.Right} {
			op, err := rule.FromToken(tok, rule.Pos{})
			if err != nil {
				t.Fatal(err)
			}
			if back := rule.ToToken(op); !reflect.DeepEqual(back, tok) {
				t.Errorf("round trip mismatch: %#v vs %#v", back, tok)
			}
		}
	}
}

func TestParseLimits(t *testing.T) {
	deep := func(n int) string {
		return strings.Repeat("(", n) + "a = 1" + strings.Repeat(")", n)
	}

	if _, err := rule.Parse(deep(rule.MaxGroupDepth)); err != nil {
		t.Fatalf("depth %d must be accepted: %v", rule.MaxGroupDepth, err)
	}
	for _, s := range []string{deep(rule.MaxGroupDepth + 1), strings.Repeat("(", 60000)} {
		if _, err := rule.Parse(s); !errors.Is(err, rule.ErrExprTooDeep) {
			t.Fatalf("expected ErrExprTooDeep, got %v", err)
		}
	}

	// parentheses inside quoted text are not groups
	if _, err := rule.Parse(`a = '` + strings.Repeat("(", 500) + `' && b = "\"` + strings.Repeat("(", 500) + `"`); err != nil {
		t.Fatalf("quoted parens must not count: %v", err)
	}

	long := `a = '` + strings.Repeat("x", rule.MaxExprLen) + `'`
	if _, err := rule.Parse(long); !errors.Is(err, rule.ErrExprTooLong) {
		t.Fatalf("expected ErrExprTooLong, got %v", err)
	}
}

func TestParseCollectionModifierIdents(t *testing.T) {
	for _, s := range []string{`@collection.x:lower = 1`, `@collection.x:alias:length = 1`, `@collection.x:a.f:lower = 1`} {
		if _, err := rule.Parse(s); err != nil {
			t.Errorf("%q: %v", s, err)
		}
	}
}
