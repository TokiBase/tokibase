package search_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/kernel/rule"
	rulesql "github.com/tokibase/tokibase/kernel/rule/sql"
	"github.com/tokibase/tokibase/tests"
	"github.com/tokibase/tokibase/tools/search"
	"github.com/tokibase/tokibase/tools/security"
)

type buildOutcome struct {
	SQL    string
	Params string
	Err    string
}

func (o buildOutcome) String() string {
	if o.Err != "" {
		return "ERR: " + o.Err
	}
	return o.SQL + " ;; " + o.Params
}

// buildOnce compiles expr for the collection with the legacy (useAST=false)
// or the AST path and renders the final query SQL and parameters.
//
// The pseudorandom source is reseeded so that both paths must produce
// byte-identical output (placeholders and aliases included).
func buildOnce(app *tests.TestApp, col *kernel.Collection, ri *kernel.RequestInfo, expr string, useAST bool, hidden bool, extra ...dbx.Params) buildOutcome {
	defer search.SetRuleASTForTest(useAST)()

	restore := security.SeedPseudorandomForTest(42)
	defer restore()

	resolver := kernel.NewRecordFieldResolver(app, col, ri, hidden)

	e, err := search.FilterData(expr).BuildExpr(resolver, extra...)
	if err != nil {
		return buildOutcome{Err: err.Error()}
	}

	q := app.DB().Select("*").From(col.Name).AndWhere(e)
	if err := resolver.UpdateQuery(q); err != nil {
		return buildOutcome{Err: "UpdateQuery: " + err.Error()}
	}

	built := q.Build()

	keys := make([]string, 0, len(built.Params()))
	for k := range built.Params() {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sb strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&sb, "%s=%#v,", k, built.Params()[k])
	}

	return buildOutcome{SQL: built.SQL(), Params: sb.String()}
}

// repoExpressions extracts every Go string literal in the repository's
// *_test.go files that parses as a filter expression.
func repoExpressions(t *testing.T) []string {
	t.Helper()

	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}

	seen := map[string]struct{}{}
	fset := token.NewFileSet()

	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "ui", "lost+found", "data":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}

		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return nil // not our concern here
		}

		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil || len(s) > 600 || strings.TrimSpace(s) == "" {
				return true
			}
			if _, err := rule.Parse(s); err == nil {
				seen[s] = struct{}{}
			}
			return true
		})

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	result := make([]string, 0, len(seen))
	for s := range seen {
		result = append(result, s)
	}
	sort.Strings(result)

	return result
}

func collectionRules(app *tests.TestApp) []string {
	var rules []string
	cols, _ := app.FindAllCollections()
	for _, c := range cols {
		for _, r := range []*string{c.ListRule, c.ViewRule, c.CreateRule, c.UpdateRule, c.DeleteRule} {
			if r != nil && strings.TrimSpace(*r) != "" {
				rules = append(rules, *r)
			}
		}
		if c.IsAuth() {
			for _, r := range []*string{c.AuthRule, c.ManageRule} {
				if r != nil && strings.TrimSpace(*r) != "" {
					rules = append(rules, *r)
				}
			}
		}
	}
	return rules
}

func TestRuleASTDifferential(t *testing.T) {
	defer search.SetRuleASTForTest(false)()

	restoreClock := search.SetTimeNowForTest(func() time.Time {
		return time.Date(2026, 10, 7, 12, 34, 56, 789000000, time.UTC)
	})
	defer restoreClock()

	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	authRecord, err := app.FindFirstRecordByData("users", "email", "test@example.com")
	if err != nil {
		t.Fatal(err)
	}

	ri := &kernel.RequestInfo{
		Context: kernel.RequestInfoContextDefault,
		Method:  "POST",
		Query:   map[string]string{"q": "query-val", "a": "b"},
		Headers: map[string]string{"x_token": "tok", "content_type": "application/json"},
		Body: map[string]any{
			"text": "body text", "number": 12.5, "bool": true, "id": authRecord.Id,
			"select_many": []string{"optionA", "optionB"}, "rel_many": []string{"a", "b"},
			"rel_one": "a", "list": []any{"x", "y"}, "datetime": "2026-01-01 00:00:00.000Z",
			"lon": 23.3, "lat": 42.7, "username": "Abc", "total": 3,
			"nested": map[string]any{"field": "v"}, "json": map[string]any{"a": 1},
		},
		Auth: authRecord,
	}

	hand := handCorpus()
	handwritten := hand[:len(hand)-12*16*11]
	generated := hand[len(hand)-12*16*11:]
	repo := repoExpressions(t)
	rules := collectionRules(app)

	// unique full corpus
	set := map[string]struct{}{}
	for _, group := range [][]string{hand, repo, rules} {
		for _, s := range group {
			set[s] = struct{}{}
		}
	}
	t.Logf("corpus: handwritten=%d generated=%d repo-test-strings=%d collection-rules=%d unique-total=%d",
		len(handwritten), len(generated), len(repo), len(rules), len(set))

	if len(handwritten) < 150 {
		t.Fatalf("handwritten corpus must have 150+ entries, got %d", len(handwritten))
	}

	allCollections, err := app.FindAllCollections()
	if err != nil {
		t.Fatal(err)
	}

	type variant struct {
		col    *kernel.Collection
		ri     *kernel.RequestInfo
		hidden bool
		name   string
	}

	var wide []variant
	for _, c := range allCollections {
		wide = append(wide, variant{c, ri, false, c.Name + "/ri"})
		if c.Name == "demo1" || c.Name == "users" {
			wide = append(wide, variant{c, nil, false, c.Name + "/nil"})
			wide = append(wide, variant{c, ri, true, c.Name + "/ri+hidden"})
		}
	}

	var narrow []variant
	for _, v := range wide {
		switch v.col.Name {
		case "demo1", "users", "demo4":
			narrow = append(narrow, v)
		}
	}

	genSet := map[string]struct{}{}
	for _, s := range generated {
		genSet[s] = struct{}{}
	}

	compared, bothOK, bothErr := 0, 0, 0
	var mismatches []string

	check := func(expr string, v variant) {
		legacy := buildOnce(app, v.col, v.ri, expr, false, v.hidden)
		viaAST := buildOnce(app, v.col, v.ri, expr, true, v.hidden)

		compared++
		if legacy.Err != "" {
			bothErr++
		} else {
			bothOK++
		}

		if legacy != viaAST {
			mismatches = append(mismatches, fmt.Sprintf("[%s] %q\n  legacy: %s\n  ast:    %s", v.name, expr, legacy, viaAST))
		}
	}

	keys := make([]string, 0, len(set))
	for s := range set {
		keys = append(keys, s)
	}
	slices.Sort(keys)

	for _, expr := range keys {
		vs := wide
		if _, ok := genSet[expr]; ok {
			vs = narrow
		}
		for _, v := range vs {
			check(expr, v)
		}
	}

	// kernel/rule/sql.Emit entry point must match too (default limit, no env switch)
	for _, expr := range keys {
		ast, perr := rule.Parse(expr)
		if perr != nil {
			continue
		}
		render := func(build func(*kernel.RecordFieldResolver) (dbx.Expression, error)) (string, error) {
			restore := security.SeedPseudorandomForTest(7)
			defer restore()
			r := kernel.NewRecordFieldResolver(app, allCollections[0], ri, false)
			e, err := build(r)
			if err != nil {
				return "", err
			}
			return renderExpr(app, e), nil
		}
		s1, err1 := render(func(r *kernel.RecordFieldResolver) (dbx.Expression, error) {
			return search.FilterData(expr).BuildExpr(r)
		})
		s2, err2 := render(func(r *kernel.RecordFieldResolver) (dbx.Expression, error) {
			return rulesql.Emit(ast, r)
		})
		if (err1 == nil) != (err2 == nil) || (err1 != nil && err1.Error() != err2.Error()) {
			t.Errorf("Emit entry point error mismatch for %q: %v vs %v", expr, err1, err2)
			continue
		}
		if s1 != s2 {
			t.Errorf("Emit entry point SQL mismatch for %q:\n %s\n %s", expr, s1, s2)
		}
	}

	t.Logf("compared %d (expression x collection) builds: %d built OK, %d resolver/parse errors (error text compared too)", compared, bothOK, bothErr)

	if bothOK < 1000 {
		t.Fatalf("too few successful builds (%d), the corpus is not exercising the emitter", bothOK)
	}

	if len(mismatches) > 0 {
		for i, m := range mismatches {
			if i >= 20 {
				break
			}
			t.Error(m)
		}
		t.Fatalf("%d parity mismatches", len(mismatches))
	}
}

func TestRuleASTDifferentialPlaceholdersAndLimit(t *testing.T) {
	defer search.SetRuleASTForTest(false)()

	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	col, _ := app.FindCollectionByNameOrId("demo1")

	params := []dbx.Params{{"a": "x'y", "n": 12, "z": nil, "b": true, "f": 1.5, "o": map[string]any{"k": "v"}}}
	for _, expr := range []string{
		`text = {:a} && number > {:n}`, `text = {:z}`, `bool = {:b} || number = {:f}`, `json = {:o}`, `text ~ {:a}`, `text = {:missing}`,
	} {
		l := buildOnce(app, col, nil, expr, false, false, params...)
		a := buildOnce(app, col, nil, expr, true, false, params...)
		if l != a {
			t.Errorf("%q\n legacy: %s\n ast:    %s", expr, l, a)
		}
	}

	// expression limit parity
	for _, limit := range []int{0, 1, 2, 3} {
		expr := `text = 'a' && (number = 1 || bool = true) && email != ''`
		var res [2]string
		for i, useAST := range []bool{false, true} {
			restoreAST := search.SetRuleASTForTest(useAST)
			r := kernel.NewRecordFieldResolver(app, col, nil, false)
			e, err := search.FilterData(expr).BuildExprWithLimit(r, limit)
			restoreAST()
			if err != nil {
				res[i] = "ERR " + err.Error()
			} else {
				res[i] = "OK " + renderExpr(app, e)
			}
		}
		if res[0] != res[1] {
			t.Errorf("limit %d mismatch: %q vs %q", limit, res[0], res[1])
		}
	}
}

// TestRuleASTMacrosInSync keeps kernel/rule's macro list in sync with the emitter's.
func TestRuleASTMacrosInSync(t *testing.T) {
	a := search.IdentifierMacroNames()
	b := rule.MacroNames()
	slices.Sort(a)
	slices.Sort(b)
	if !slices.Equal(a, b) {
		t.Fatalf("macro lists differ:\n search: %v\n rule:   %v", a, b)
	}
}

func renderExpr(app *tests.TestApp, e dbx.Expression) string {
	b := app.DB().Select("*").From("demo1").AndWhere(e).Build()
	keys := make([]string, 0, len(b.Params()))
	for k := range b.Params() {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := b.SQL()
	for _, k := range keys {
		out += fmt.Sprintf(" %s=%#v", k, b.Params()[k])
	}
	return out
}
