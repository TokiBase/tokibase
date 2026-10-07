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
	"github.com/tokibase/tokibase/tools/security"
	"github.com/tokibase/tokibase/tools/search"
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

func buildDialect(app *tests.TestApp, col *kernel.Collection, ri *kernel.RequestInfo, expr string, usePG bool) dialectOutcome {
	restore := security.SeedPseudorandomForTest(42)
	defer restore()

	resolver := kernel.NewRecordFieldResolver(app, col, ri, false)

	ast, err := rule.Parse(expr)
	if err != nil {
		return dialectOutcome{err: err}
	}

	var e dbx.Expression
	if usePG {
		resolver.SetDialect(pg.Dialect)
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
	sqliteOnlyRegex  = regexp.MustCompile(`(?i)\b(json_each|json_valid|json_type|json_array_length|json_extract|json_object|json_array|strftime|iif)\s*\(|\bmin\(1|\bmax\(-1`)
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
	`text ~ email`, `json.a.b = 'x'`, `json.a[0] = 1`, `json = 'x'`, `json:length = 1`, `select_many:length > 1`,
	`text:lower = 'abc'`, `email:lower ~ 'a'`,
	`rel_one.title = 'x'`, `rel_one.id = 'x'`, `rel_one.title != rel_one.title`, `rel_one.rel_one.title ~ 'x'`,
	`demo1_via_rel_one.text = 'x'`, `demo1_via_rel_many.text ?= 'x'`, `demo1_via_rel_many.text = 'x'`,
	`rel_many.title = 'a'`, `rel_many.title ?= 'a'`, `select_many:each ~ 'a'`,
	`@request.auth.id != ''`, `@request.auth.id = id`, `@request.auth.verified = true`, `@request.auth.rel.title = 'x'`,
	`@request.method = 'GET'`, `@request.query.a = 'b'`, `@request.body.text = text`, `@request.body.text:isset = true`,
	`@request.body.text:lower = 'a'`, `@request.body.text:changed = true`, `@request.body.select_many:each ~ 'a'`,
	`@collection.demo2.title = 'x'`, `@collection.demo2:a.title = text`, `@collection.demo2.title = @collection.demo2:b.title`,
	`datetime > @now`, `datetime >= @todayStart && datetime <= @todayEnd`, `@year = 2026`,
	`geoDistance(1, 2, 3, 4) < 10`, `geoDistance(point.lon, point.lat, 23.32, 42.69) < 200`,
	`strftime('%Y', created) = '2026'`, `text = 'a' && missing = 1`,
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

		out := buildDialect(app, col, ri, expr, true)
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

	var problems []string
	compared, same, unsupported := 0, 0, 0
	unsupportedKinds := map[string]int{}

	for _, expr := range keys {
		for _, col := range cols {
			lite := buildDialect(app, col, ri, expr, false)
			post := buildDialect(app, col, ri, expr, true)
			compared++

			id := fmt.Sprintf("[%s] %q", col.Name, expr)

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

				if lite.params != post.params {
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

	r.SetDialect(pg.Dialect)
	if _, err := rulesql.Emit(ast, r); err == nil {
		t.Fatal("expected a dialect mismatch error for the PostgreSQL resolver")
	}
}
