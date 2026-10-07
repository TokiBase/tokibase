package search_test

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/kernel/rule"
	"github.com/tokibase/tokibase/kernel/rule/pg"
	rulesql "github.com/tokibase/tokibase/kernel/rule/sql"
	"github.com/tokibase/tokibase/tests"
	"github.com/tokibase/tokibase/tools/search"
	"github.com/tokibase/tokibase/tools/security"
)

// There is no PostgreSQL in CI. The PostgreSQL emitter is verified by
//   - golden SQL strings for a curated list (testdata/rule_pg_golden.txt,
//     regenerate with TOKI_UPDATE_GOLDEN=1 and review the diff),
//   - a structural comparison with the SQLite output for the whole corpus:
//     same parameters, same placeholder order, no SQLite-only constructs, and
//     every failure is an explicit rule.ErrUnsupported (or an error SQLite has too).

type dialectOutcome struct {
	sql, params string
	rawParams   map[string]any
	err         error
}

func buildDialect(app *tests.TestApp, col *kernel.Collection, ri *kernel.RequestInfo, expr string, usePG, hidden bool) dialectOutcome {
	restore := security.SeedPseudorandomForTest(42)
	defer restore()

	resolver := kernel.NewRecordFieldResolver(app, col, ri, hidden)

	ast, err := rule.Parse(expr)
	if err != nil {
		return dialectOutcome{err: err}
	}

	var e dbx.Expression
	if usePG {
		if err := resolver.SetDialect(pg.Dialect); err != nil {
			return dialectOutcome{err: err}
		}
		e, err = pg.Emit(ast, resolver)
	} else {
		e, err = rulesql.Emit(ast, resolver)
	}
	if err != nil {
		return dialectOutcome{err: err}
	}

	q := app.DB().Select("*").From(col.Name).AndWhere(e)
	if err := resolver.UpdateQuery(q); err != nil {
		return dialectOutcome{err: err}
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

	raw := map[string]any{}
	for k, v := range built.Params() {
		raw[k] = v
	}

	return dialectOutcome{sql: built.SQL(), params: sb.String(), rawParams: raw}
}

var (
	placeholderRegex = regexp.MustCompile(`\{:([A-Za-z0-9_]+)\}`)
	randAliasRegex   = regexp.MustCompile(`__(sm|ml|mr)[A-Za-z0-9]{8}`)
	isRegex          = regexp.MustCompile(`\bIS\s+(NOT\s+)?(\w+)`)
	likeRegex        = regexp.MustCompile(`(^|[^I])LIKE\b`)
	sqliteOnlyRegex  = regexp.MustCompile(`(?i)\b(json_each|json_valid|json_type|json_array_length|json_extract|json_object|json_array|strftime|iif|ifnull|typeof|instr|substr|datetime|group_concat)\s*\(|\bglob\b|\bmin\(1|\bmax\(-1`)
)

// normalizePG renders the dbx output the way a PostgreSQL connection would
// quote it (double quotes) and makes the random names stable.
func normalizePG(sql string, params map[string]any) (string, string) {
	names := map[string]string{}
	var order []string
	for _, m := range placeholderRegex.FindAllStringSubmatch(sql, -1) {
		if _, ok := names[m[1]]; !ok {
			names[m[1]] = fmt.Sprintf("p%d", len(names)+1)
			order = append(order, m[1])
		}
	}

	sql = placeholderRegex.ReplaceAllStringFunc(sql, func(s string) string {
		return "{:" + names[s[2:len(s)-1]] + "}"
	})
	sql = randAliasRegex.ReplaceAllString(sql, "__${1}X")
	sql = strings.ReplaceAll(sql, "`", `"`)

	var sb strings.Builder
	for _, k := range order {
		fmt.Fprintf(&sb, "%s=%#v ", names[k], params[k])
	}

	return sql, strings.TrimSpace(sb.String())
}

// paramsEqualModLikeNormalization reports whether the params are equal except
// for LIKE patterns that the PostgreSQL dialect had to normalize (dangling escape).
func paramsEqualModLikeNormalization(lite, post map[string]any) bool {
	if len(lite) != len(post) {
		return false
	}

	for k, lv := range lite {
		pv, ok := post[k]
		if !ok {
			return false
		}
		if fmt.Sprintf("%#v", lv) == fmt.Sprintf("%#v", pv) {
			continue
		}
		ls, isStr := lv.(string)
		if !isStr || pv != pg.Dialect.NormalizeLikePattern(ls) {
			return false
		}
	}

	return true
}

func placeholderOrder(sql string) []string {
	var order []string
	for _, m := range placeholderRegex.FindAllStringSubmatch(sql, -1) {
		if !slices.Contains(order, m[1]) {
			order = append(order, m[1])
		}
	}
	return order
}

// pgViolations returns SQLite-only constructs found in a PostgreSQL query.
func pgViolations(sql string) []string {
	var v []string

	for _, m := range isRegex.FindAllStringSubmatch(sql, -1) {
		switch strings.ToUpper(m[2]) {
		case "NULL", "DISTINCT":
		default:
			v = append(v, "null-safe IS: "+m[0])
		}
	}

	if likeRegex.MatchString(sql) {
		v = append(v, "case sensitive LIKE")
	}

	if m := sqliteOnlyRegex.FindString(sql); m != "" {
		v = append(v, "sqlite construct: "+m)
	}

	return v
}

var pgGoldenExprs = []string{
	`text = 'abc'`, `text != 'abc'`, `text ~ 'abc'`, `text !~ 'a_b'`, `number > 1`, `number <= 1.5`,
	`text = null`, `text != ''`, `text = number`, `number != text`, `bool = true`, `bool != false`, `true = false`,
	`text = email && number != 3 || bool = true`, `(text ~ 'a' || text ~ 'b') && number >= 1`,
	`text ~ email`, `json.a.b = 'x'`, `json.a.0 = 1`, `json = 'x'`, `json:length = 1`, `select_many:length > 1`,
	`text:lower = 'abc'`, `email:lower ~ 'a'`,
	`rel_one.text = 'x'`, `rel_one.id = 'x'`, `rel_one.text != rel_one.text`, `rel_one.rel_one.text ~ 'x'`,
	`demo1_via_rel_one.text = 'x'`, `demo1_via_rel_many.text ?= 'x'`, `demo1_via_rel_many.text = 'x'`,
	`rel_many.title = 'a'`, `rel_many.title ?= 'a'`, `select_many:each ~ 'a'`,
	`@request.auth.id != ''`, `@request.auth.id = id`, `@request.auth.verified = true`, `@request.auth.rel.title = 'x'`,
	`@request.method = 'GET'`, `@request.query.a = 'b'`, `@request.body.text = text`, `@request.body.text:isset = true`,
	`@request.body.text:lower = 'a'`, `@request.body.text:changed = true`, `@request.body.select_many:each ~ 'a'`,
	`@collection.demo2.title = 'x'`, `@collection.demo2:a.title = text`, `@collection.demo2.title = @collection.demo2:b.title`,
	`datetime > @now`, `datetime >= @todayStart && datetime <= @todayEnd`, `@year = 2026`,
	`geoDistance(1, 2, 3, 4) < 10`, `geoDistance(point.lon, point.lat, 23.32, 42.69) < 200`,
	`strftime('%Y', created) = '2026'`, `text = 'a' && missing = 1`,
	// typed semantics (QC round 5)
	`@request.body.missingbool = true`, `@request.body.missingbool != false`, `@request.body.missingnum = 5`,
	`@request.body.missingnum = number`, `@request.body.bool = true`,
	`json.flag = true`, `json.flag != false`, `json.count > 5`, `json.count <= 5.5`, `json.count = number`, `json.flag = bool`,
	`json.null = 'x'`, `json:lower = 'a'`, `json.a.b:lower = 'a'`,
	`number:lower = 1`, `number = number`, `number != number`, `bool = bool`, `datetime = datetime`,
	`text ~ "a%\\"`, `text ~ "a\\"`, `text !~ "100%"`,
	`geoDistance(point.lon, point.lat, json.lon, json.lat) < 10`,
	`@collection.demo2.title = 'x' && json.a = true`, `@collection.demo1:x.json.a = 1`, `@collection.demo1:x.json.a = 'x'`,
	`@request.body.number:lower = '12.5'`,
	`rel_many.json.a = 1`, `rel_many.json.a = 'x'`,
}

func pgTestApp(t *testing.T) (*tests.TestApp, *kernel.RequestInfo, func()) {
	t.Helper()

	restoreClock := search.SetTimeNowForTest(func() time.Time {
		return time.Date(2026, 10, 7, 12, 34, 56, 789000000, time.UTC)
	})

	app, _ := tests.NewTestApp()

	authRecord, err := app.FindFirstRecordByData("users", "email", "test@example.com")
	if err != nil {
		t.Fatal(err)
	}

	ri := &kernel.RequestInfo{
		Context: kernel.RequestInfoContextDefault,
		Method:  "POST",
		Query:   map[string]string{"q": "query-val", "a": "b"},
		Headers: map[string]string{"x_token": "tok"},
		Body: map[string]any{
			"text": "body text", "number": 12.5, "bool": true, "id": authRecord.Id,
			"select_many": []string{"optionA", "optionB"}, "rel_many": []string{"a", "b"},
			"rel_one": "a", "datetime": "2026-01-01 00:00:00.000Z", "lon": 23.3, "lat": 42.7,
		},
		Auth: authRecord,
	}

	return app, ri, func() {
		app.Cleanup()
		restoreClock()
	}
}

func TestRulePostgresGolden(t *testing.T) {
	defer search.SetRuleASTForTest(false)()

	app, ri, cleanup := pgTestApp(t)
	defer cleanup()

	col, err := app.FindCollectionByNameOrId("demo1")
	if err != nil {
		t.Fatal(err)
	}

	var sb strings.Builder
	for _, expr := range pgGoldenExprs {
		sb.WriteString("## " + expr + "\n")

		out := buildDialect(app, col, ri, expr, true, true)
		if out.err != nil {
			sb.WriteString("ERR " + out.err.Error() + "\n\n")
			continue
		}

		sql, params := normalizePG(out.sql, out.rawParams)
		sb.WriteString(sql + "\n")
		sb.WriteString(params + "\n\n")
	}

	const goldenPath = "testdata/rule_pg_golden.txt"

	if os.Getenv("TOKI_UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(goldenPath, []byte(sb.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatal(err)
	}

	if string(want) != sb.String() {
		gotLines := strings.Split(sb.String(), "\n")
		wantLines := strings.Split(string(want), "\n")
		for i := 0; i < len(gotLines) && i < len(wantLines); i++ {
			if gotLines[i] != wantLines[i] {
				t.Fatalf("golden mismatch at line %d:\n got:  %s\n want: %s\n(regenerate with TOKI_UPDATE_GOLDEN=1 and review)", i+1, gotLines[i], wantLines[i])
			}
		}
		t.Fatalf("golden length mismatch: got %d lines, want %d", len(gotLines), len(wantLines))
	}
}

func TestRulePostgresStructuralParity(t *testing.T) {
	defer search.SetRuleASTForTest(false)()

	app, ri, cleanup := pgTestApp(t)
	defer cleanup()

	seen := map[string]struct{}{}
	var keys []string
	for _, group := range [][]string{handCorpus(), collectionRules(app), pgGoldenExprs} {
		for _, s := range group {
			if _, ok := seen[s]; !ok {
				seen[s] = struct{}{}
				keys = append(keys, s)
			}
		}
	}
	slices.Sort(keys)

	var cols []*kernel.Collection
	for _, name := range []string{"demo1", "users", "demo4"} {
		c, err := app.FindCollectionByNameOrId(name)
		if err != nil {
			t.Fatal(err)
		}
		cols = append(cols, c)
	}
	hiddenVariants := []bool{false, true}

	var problems []string
	compared, same, unsupported := 0, 0, 0
	unsupportedKinds := map[string]int{}

	for _, expr := range keys {
		for _, col := range cols {
			for _, hidden := range hiddenVariants {
				lite := buildDialect(app, col, ri, expr, false, hidden)
				post := buildDialect(app, col, ri, expr, true, hidden)
				compared++

				id := fmt.Sprintf("[%s hidden=%v] %q", col.Name, hidden, expr)

				switch {
				case lite.err != nil && post.err == nil:
					problems = append(problems, id+": sqlite fails ("+lite.err.Error()+") but postgres builds")
				case lite.err != nil && post.err != nil:
					// same failure class; unsupported must not mask a parse/resolve error
					if errors.Is(post.err, rule.ErrUnsupported) && !errors.Is(lite.err, rule.ErrUnsupported) {
						unsupported++
					}
				case lite.err == nil && post.err != nil:
					if !errors.Is(post.err, rule.ErrUnsupported) {
						problems = append(problems, id+": postgres fails without ErrUnsupported: "+post.err.Error())
						continue
					}
					unsupported++
					msg := post.err.Error()
					if i := strings.LastIndex(msg, "postgres: "); i >= 0 {
						msg = msg[i:]
					}
					if j := strings.Index(msg, ";"); j > 0 {
						msg = msg[:j]
					}
					unsupportedKinds[msg]++
				default:
					same++

					if lite.params != post.params && !paramsEqualModLikeNormalization(lite.rawParams, post.rawParams) {
						problems = append(problems, id+": params differ\n  sqlite:   "+lite.params+"\n  postgres: "+post.params)
					}

					if !slices.Equal(placeholderOrder(lite.sql), placeholderOrder(post.sql)) {
						problems = append(problems, id+": placeholder order differs")
					}

					if v := pgViolations(post.sql); len(v) > 0 {
						problems = append(problems, id+": "+strings.Join(v, "; ")+"\n  "+post.sql)
					}
				}
			}
		}
	}

	t.Logf("compared %d builds: %d built by both, %d unsupported by postgres", compared, same, unsupported)
	for k, n := range unsupportedKinds {
		t.Logf("unsupported (%d): %s", n, k)
	}

	if same < 1000 {
		t.Fatalf("too few builds on both dialects (%d)", same)
	}

	if len(problems) > 0 {
		for i, p := range problems {
			if i >= 20 {
				break
			}
			t.Error(p)
		}
		t.Fatalf("%d structural problems", len(problems))
	}
}

// A resolver for a different dialect than the emitter is rejected instead of
// silently mixing fragments.
func TestRuleEmitDialectMismatch(t *testing.T) {
	app, _, cleanup := pgTestApp(t)
	defer cleanup()

	col, _ := app.FindCollectionByNameOrId("demo1")
	ast, _ := rule.Parse(`text = 'a'`)

	r := kernel.NewRecordFieldResolver(app, col, nil, false)
	if _, err := pg.Emit(ast, r); err == nil {
		t.Fatal("expected a dialect mismatch error for the SQLite resolver")
	}

	if err := r.SetDialect(pg.Dialect); err != nil {
		t.Fatal(err)
	}
	if _, err := rulesql.Emit(ast, r); err == nil {
		t.Fatal("expected a dialect mismatch error for the PostgreSQL resolver")
	}
}

// Dialect specific typed constructs must appear where the engine knows the
// operand types (the structural test cannot see type errors).
func TestRulePostgresTypedCasts(t *testing.T) {
	defer search.SetRuleASTForTest(false)()

	app, ri, cleanup := pgTestApp(t)
	defer cleanup()

	col, err := app.FindCollectionByNameOrId("demo1")
	if err != nil {
		t.Fatal(err)
	}

	scenarios := []struct {
		expr     string
		contains []string
		excludes []string
	}{
		{`json.flag = true`, []string{`#> '{"flag"}') IS NOT DISTINCT FROM to_jsonb(CAST(TRUE AS BOOLEAN))`}, []string{`#>>`}},
		{`json.count > 5`, []string{`#> '{"count"}') > to_jsonb(CAST({:`, `AS DOUBLE PRECISION))`}, []string{`#>>`}},
		{`json.a = 'x'`, []string{`#>> '{"a"}')`}, []string{`to_jsonb(CAST(`}},
		{`json.null = 'x'`, []string{`'{"null"}'`}, nil},
		{`@request.body.missingbool = true`, []string{`CAST(NULL AS BOOLEAN) IS NOT DISTINCT FROM TRUE`}, []string{`'' = TRUE`}},
		{`@request.body.missingbool != true`, []string{`CAST(NULL AS BOOLEAN) IS DISTINCT FROM TRUE`}, nil},
		{`@request.body.missingnum = 5`, []string{`CAST(NULL AS DOUBLE PRECISION) IS NOT DISTINCT FROM {:`}, nil},
		{`number = number`, []string{`[[demo1.number]] IS NOT DISTINCT FROM [[demo1.number]]`}, []string{`CAST(`}},
		{`datetime != datetime`, []string{`[[demo1.datetime]] IS DISTINCT FROM [[demo1.datetime]]`}, []string{`CAST(`}},
		{`number:lower = 1`, []string{`LOWER(CAST([[demo1.number]] AS TEXT))`}, nil},
		{`json:lower = 'a'`, []string{`LOWER(CAST(`}, nil},
		{`@request.body.text:lower = 'a'`, []string{`LOWER(CAST({:`}, nil},
		{`text ~ 'a'`, []string{`ESCAPE E'\\'`}, nil},
		{`geoDistance(1, 2, 3, 4) < 10`, []string{`CASE WHEN t.lo1 ~ '^ *[-+]?`, `AS DOUBLE PRECISION) END`}, nil},
	}

	for _, s := range scenarios {
		out := buildDialect(app, col, ri, s.expr, true, true)
		if out.err != nil {
			t.Errorf("%q: %v", s.expr, out.err)
			continue
		}
		for _, c := range s.contains {
			if !strings.Contains(out.sql, c) {
				t.Errorf("%q: expected %q in\n%s", s.expr, c, out.sql)
			}
		}
		for _, c := range s.excludes {
			if strings.Contains(out.sql, c) {
				t.Errorf("%q: unexpected %q in\n%s", s.expr, c, out.sql)
			}
		}
	}

	// a dangling escape character in the user pattern is doubled
	out := buildDialect(app, col, ri, `text ~ "a%\\"`, true, true)
	if out.err != nil {
		t.Fatal(out.err)
	}
	found := false
	for _, v := range out.rawParams {
		if v == "a%\\\\" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the doubled trailing backslash, got %v", out.rawParams)
	}

	// SQLite keeps the user pattern untouched
	lite := buildDialect(app, col, ri, `text ~ "a%\\"`, false, true)
	for _, v := range lite.rawParams {
		if v != "a%\\" {
			t.Fatalf("sqlite pattern must stay unchanged, got %q", v)
		}
	}
}

// SetDialect after Resolve would leave joins of the previous dialect behind.
func TestRuleSetDialectAfterResolve(t *testing.T) {
	app, _, cleanup := pgTestApp(t)
	defer cleanup()

	col, _ := app.FindCollectionByNameOrId("demo1")

	r := kernel.NewRecordFieldResolver(app, col, nil, true)
	if _, err := r.Resolve("text"); err != nil {
		t.Fatal(err)
	}
	if err := r.SetDialect(pg.Dialect); err == nil {
		t.Fatal("expected an error when SetDialect is called after Resolve")
	}
}

type nilDialectResolver struct{ *kernel.RecordFieldResolver }

func (nilDialectResolver) Dialect() rule.Dialect { return nil }

// A resolver reporting a nil dialect is an error, not a panic.
func TestRuleNilDialectResolver(t *testing.T) {
	app, _, cleanup := pgTestApp(t)
	defer cleanup()

	col, _ := app.FindCollectionByNameOrId("demo1")
	ast, _ := rule.Parse(`text = 'a'`)

	r := nilDialectResolver{kernel.NewRecordFieldResolver(app, col, nil, true)}
	if _, err := rulesql.Emit(ast, r); err == nil {
		t.Fatal("expected an error for a nil resolver dialect")
	}
	if _, err := search.FilterData(`text = 'a'`).BuildExpr(r); err == nil {
		t.Fatal("expected an error for a nil resolver dialect (BuildExpr)")
	}
}

// The multi-value back relation (demo1.rel_many -> users) builds the member
// join (and the multi-match join) end to end with the array-member condition.
func TestRulePostgresMultiBackRelation(t *testing.T) {
	defer search.SetRuleASTForTest(false)()

	app, ri, cleanup := pgTestApp(t)
	defer cleanup()

	col, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatal(err)
	}

	for _, expr := range []string{`demo1_via_rel_many.text = 'x'`, `demo1_via_rel_many.text ?= 'x'`} {
		out := buildDialect(app, col, ri, expr, true, true)
		if out.err != nil {
			t.Fatalf("%q: %v", expr, out.err)
		}

		want := 1
		if !strings.Contains(expr, "?=") {
			want = 2 // main join + multi-match subquery
		}

		if n := strings.Count(out.sql, "jsonb_array_elements_text("); n != want {
			t.Fatalf("%q: expected %d array-member conditions, got %d:\n%s", expr, want, n, out.sql)
		}

		if !strings.Contains(out.sql, "LEFT JOIN") || pgViolations(out.sql) != nil {
			t.Fatalf("%q: unexpected SQL (%v):\n%s", expr, pgViolations(out.sql), out.sql)
		}
	}
}
