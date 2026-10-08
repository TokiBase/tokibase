//go:build !no_roles

package roles

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel/rule"
	"github.com/tokibase/tokibase/tests"
	"github.com/tokibase/tokibase/tools/search"
	"github.com/tokibase/tokibase/tools/types"
)

type env struct {
	app          *tests.TestApp
	alice, bob   *core.Record
	docs         *core.Collection
	teamA, teamB *core.Record
}

func setup(t *testing.T) *env {
	t.Helper()
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)
	Register(app)
	if err := EnsureCollections(app); err != nil {
		t.Fatal(err)
	}
	e := &env{app: app}
	e.alice, _ = app.FindAuthRecordByEmail("users", "test@example.com")
	e.bob, _ = app.FindAuthRecordByEmail("users", "test2@example.com")

	teams := core.NewBaseCollection("teams")
	teams.Fields.Add(&core.TextField{Name: "title"})
	if err := app.Save(teams); err != nil {
		t.Fatal(err)
	}
	e.teamA = core.NewRecord(teams)
	e.teamB = core.NewRecord(teams)
	for _, r := range []*core.Record{e.teamA, e.teamB} {
		if err := app.Save(r); err != nil {
			t.Fatal(err)
		}
	}
	e.docs = core.NewBaseCollection("docs")
	e.docs.Fields.Add(&core.TextField{Name: "team"})
	if err := app.Save(e.docs); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) role(t *testing.T, name string) *core.Record {
	t.Helper()
	col, _ := e.app.FindCollectionByNameOrId(RolesName)
	r := core.NewRecord(col)
	r.Set("name", name)
	if err := e.app.Save(r); err != nil {
		t.Fatal(err)
	}
	return r
}

func (e *env) grant(t *testing.T, who *core.Record, role *core.Record, scope string, expires time.Duration) *core.Record {
	t.Helper()
	col, _ := e.app.FindCollectionByNameOrId(MembershipsName)
	m := core.NewRecord(col)
	m.Set("user_collection", who.Collection().Name) // name is normalized to the id
	m.Set("user", who.Id)
	m.Set("role", role.Id)
	m.Set("scope", scope)
	if scope != "" {
		m.Set("scope_collection", "teams")
	}
	if expires != 0 {
		m.Set("expires", types.NowDateTime().Add(expires))
	}
	if err := e.app.Save(m); err != nil {
		t.Fatal(err)
	}
	return m
}

func (e *env) can(t *testing.T, who *core.Record, rule string, team string) bool {
	t.Helper()
	rec := core.NewRecord(e.docs)
	rec.Set("team", team)
	if err := e.app.Save(rec); err != nil {
		t.Fatal(err)
	}
	info := &core.RequestInfo{Context: core.RequestInfoContextDefault, Method: "GET", Auth: who,
		Query: map[string]string{}, Headers: map[string]string{}, Body: map[string]any{}}
	ok, err := e.app.CanAccessRecord(rec, info, &rule)
	if err != nil {
		t.Fatalf("%s: %v", rule, err)
	}
	return ok
}

func TestRoleGlobal(t *testing.T) {
	e := setup(t)
	admin := e.role(t, "admin")
	e.grant(t, e.alice, admin, "", 0)
	const r = `@role("admin") = true`
	if !e.can(t, e.alice, r, "") {
		t.Fatal("admin denied")
	}
	if e.can(t, e.bob, r, "") {
		t.Fatal("non-admin allowed")
	}
	if e.can(t, nil, r, "") {
		t.Fatal("guest allowed")
	}
	if !e.can(t, e.bob, `@role("admin") = false`, "") || e.can(t, e.alice, `@role("admin") != true`, "") {
		t.Fatal("negation")
	}
	if e.can(t, e.alice, `@role("nope") = true`, "") {
		t.Fatal("unknown role allowed")
	}
	// composes with other terms
	if e.can(t, e.alice, `@role("admin") = true && team = "x"`, "y") || !e.can(t, e.alice, `@role("admin") = true && team = "x"`, "x") {
		t.Fatal("compose")
	}
	if !Has(e.app, e.alice, "admin", "") || Has(e.app, e.bob, "admin", "") || Has(e.app, nil, "admin", "") {
		t.Fatal("Has")
	}
}

func TestRoleScoped(t *testing.T) {
	e := setup(t)
	editor := e.role(t, "editor")
	e.grant(t, e.alice, editor, e.teamA.Id, 0)
	const r = `@role("editor", team) = true`
	if !e.can(t, e.alice, r, e.teamA.Id) {
		t.Fatal("scoped editor denied in own team")
	}
	if e.can(t, e.alice, r, e.teamB.Id) {
		t.Fatal("scoped editor allowed in other team")
	}
	if e.can(t, e.alice, r, "") {
		t.Fatal("empty scope operand must be false")
	}
	if e.can(t, e.alice, `@role("editor") = true`, e.teamA.Id) {
		t.Fatal("scoped grant must not be global")
	}
	if !e.can(t, e.alice, `@role("editor", "`+e.teamA.Id+`") = true`, "") {
		t.Fatal("literal scope")
	}
	if e.can(t, e.bob, r, e.teamA.Id) {
		t.Fatal("other user allowed")
	}
	if !Has(e.app, e.alice, "editor", e.teamA.Id) || Has(e.app, e.alice, "editor", "") {
		t.Fatal("Has scope")
	}
}

func TestMember(t *testing.T) {
	e := setup(t)
	viewer := e.role(t, "viewer")
	e.grant(t, e.bob, viewer, e.teamB.Id, 0)
	const r = `@member(team) = true`
	if !e.can(t, e.bob, r, e.teamB.Id) || e.can(t, e.bob, r, e.teamA.Id) || e.can(t, e.alice, r, e.teamB.Id) || e.can(t, nil, r, e.teamB.Id) {
		t.Fatal("member matrix")
	}
	if e.can(t, e.bob, r, "") {
		t.Fatal("empty scope is not a membership")
	}
	if !IsMember(e.app, e.bob, e.teamB.Id) || IsMember(e.app, e.bob, "") {
		t.Fatal("IsMember")
	}
}

func TestExpired(t *testing.T) {
	e := setup(t)
	admin := e.role(t, "admin")
	e.grant(t, e.alice, admin, "", -time.Hour)
	e.grant(t, e.bob, admin, "", time.Hour)
	if e.can(t, e.alice, `@role("admin") = true`, "") || Has(e.app, e.alice, "admin", "") {
		t.Fatal("expired membership allowed")
	}
	if !e.can(t, e.bob, `@role("admin") = true`, "") || !Has(e.app, e.bob, "admin", "") {
		t.Fatal("future expiry denied")
	}
}

func TestSuperuserNotRequired(t *testing.T) {
	e := setup(t)
	su, _ := e.app.FindAuthRecordByEmail(core.CollectionNameSuperusers, "test@example.com")
	admin := e.role(t, "admin")
	e.grant(t, su, admin, "", 0)
	if !e.can(t, su, `@role("admin") = true`, "") {
		t.Fatal("superuser")
	}
}

func TestCascade(t *testing.T) {
	e := setup(t)
	admin := e.role(t, "admin")
	editor := e.role(t, "editor")
	e.grant(t, e.alice, admin, "", 0)
	e.grant(t, e.bob, editor, e.teamA.Id, 0)
	count := func() int64 {
		n, _ := e.app.CountRecords(MembershipsName)
		return n
	}
	if count() != 2 {
		t.Fatalf("count %d", count())
	}
	// scope record deleted
	if err := e.app.Delete(e.teamA); err != nil {
		t.Fatal(err)
	}
	if count() != 1 || Has(e.app, e.bob, "editor", e.teamA.Id) {
		t.Fatalf("scope cascade: %d", count())
	}
	// role deleted
	if err := e.app.Delete(admin); err != nil {
		t.Fatal(err)
	}
	if count() != 0 || Has(e.app, e.alice, "admin", "") {
		t.Fatalf("role cascade: %d", count())
	}
	// user deleted
	e.grant(t, e.bob, editor, "", 0)
	bob, _ := e.app.FindRecordById("users", e.bob.Id)
	if err := e.app.Delete(bob); err != nil {
		t.Fatal(err)
	}
	if count() != 0 {
		t.Fatalf("user cascade: %d", count())
	}
}

func TestValidation(t *testing.T) {
	e := setup(t)
	admin := e.role(t, "admin")
	col, _ := e.app.FindCollectionByNameOrId(MembershipsName)
	m := core.NewRecord(col)
	m.Set("user_collection", "teams") // not an auth collection
	m.Set("user", "x")
	m.Set("role", admin.Id)
	if err := e.app.Save(m); err == nil {
		t.Fatal("non-auth collection accepted")
	}
	e.grant(t, e.alice, admin, "", 0)
	col2, _ := e.app.FindCollectionByNameOrId(MembershipsName)
	d := core.NewRecord(col2)
	d.Set("user_collection", "users")
	d.Set("user", e.alice.Id)
	d.Set("role", admin.Id)
	if err := e.app.Save(d); err == nil {
		t.Fatal("duplicate membership accepted")
	}
}

func TestLint(t *testing.T) {
	e := setup(t)
	e.role(t, "admin")
	r1, r2 := `@role("admin") = true`, `@role('ghost') = true || @role("admin", team) = true`
	e.docs.ListRule, e.docs.ViewRule = &r1, &r2
	if err := e.app.Save(e.docs); err != nil {
		t.Fatal(err)
	}
	fs, err := Lint(e.app)
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 1 || fs[0].Role != "ghost" || fs[0].Collection != "docs" || fs[0].Rule != "viewRule" {
		t.Fatalf("%+v", fs)
	}
}

var rePlaceholder = regexp.MustCompile(`\{:(\w+)\}`)

// normalize replaces the random placeholder and alias names by stable ones.
func normalize(sql string, params dbx.Params) string {
	names := map[string]string{}
	sql = rePlaceholder.ReplaceAllStringFunc(sql, func(s string) string {
		k := rePlaceholder.FindStringSubmatch(s)[1]
		if _, ok := names[k]; !ok {
			names[k] = "p" + string(rune('a'+len(names)))
		}
		return "{:" + names[k] + "}"
	})
	for _, p := range []string{"tkm", "tkr"} {
		re := regexp.MustCompile(p + `\w{6}`)
		seen := map[string]string{}
		sql = re.ReplaceAllStringFunc(sql, func(s string) string {
			if _, ok := seen[s]; !ok {
				seen[s] = p + string(rune('a'+len(seen)))
			}
			return seen[s]
		})
	}
	return sql
}

func TestLegacyASTParity(t *testing.T) {
	e := setup(t)
	info := &core.RequestInfo{Context: core.RequestInfoContextDefault, Method: "GET", Auth: e.alice,
		Query: map[string]string{}, Headers: map[string]string{}, Body: map[string]any{}}
	for _, raw := range []string{
		`@role("admin") = true`,
		`@role("editor", team) = true`,
		`@member(team) = true && team != ""`,
		`@role("x", "lit") = false`,
	} {
		ast, err := rule.Parse(raw)
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		r1 := core.NewRecordFieldResolver(e.app, e.docs, info, true)
		viaAST, err := search.EmitAST(ast, r1, 0)
		if err != nil {
			t.Fatalf("%s ast: %v", raw, err)
		}
		r2 := core.NewRecordFieldResolver(e.app, e.docs, info, true)
		legacy, err := search.FilterData(raw).BuildExpr(r2)
		if err != nil {
			t.Fatalf("%s legacy: %v", raw, err)
		}
		db := e.app.DB().(*dbx.DB)
		ps1, ps2 := dbx.Params{}, dbx.Params{}
		a := normalize(viaAST.Build(db, ps1), ps1)
		b := normalize(legacy.Build(db, ps2), ps2)
		if a != b {
			t.Fatalf("%s\nAST:    %s\nlegacy: %s", raw, a, b)
		}
		if strings.Contains(a, "'admin'") || strings.Contains(a, "lit") {
			t.Fatalf("value interpolated into SQL: %s", a)
		}
		if len(ps1) != len(ps2) {
			t.Fatalf("%s: param count %d vs %d", raw, len(ps1), len(ps2))
		}
	}
}

func TestBadArgs(t *testing.T) {
	e := setup(t)
	info := &core.RequestInfo{Context: core.RequestInfoContextDefault, Method: "GET",
		Query: map[string]string{}, Headers: map[string]string{}, Body: map[string]any{}}
	for _, raw := range []string{`@role() = true`, `@role(team) = true`, `@role("a","b","c") = true`, `@member() = true`, `@role("") = true`} {
		r := core.NewRecordFieldResolver(e.app, e.docs, info, true)
		if _, err := search.FilterData(raw).BuildExpr(r); err == nil {
			t.Fatalf("%s accepted", raw)
		}
	}
}
