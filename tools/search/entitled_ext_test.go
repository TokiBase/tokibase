//go:build !no_payments

package search_test

import (
	"testing"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/modules/payments"
	"github.com/tokibase/tokibase/tests"
	"github.com/tokibase/tokibase/tools/search"
)

// The entitled() rule function is registered additively by modules/payments.
// It must allow/deny identically on the legacy compiler and on the AST path,
// and must not disturb the built-in functions.
func TestEntitledRuleBothPaths(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()
	m := payments.Register(app)

	mc := core.NewAuthCollection("members")
	if err := app.Save(mc); err != nil {
		t.Fatal(err)
	}
	mk := func(email string) *core.Record {
		r := core.NewRecord(mc)
		r.SetEmail(email)
		r.SetPassword("password12345")
		if err := app.Save(r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	pro, free := mk("pro@example.com"), mk("free@example.com")

	col := core.NewBaseCollection("posts")
	col.Fields.Add(&core.TextField{Name: "title"})
	if err := app.Save(col); err != nil {
		t.Fatal(err)
	}
	post := core.NewRecord(col)
	post.Set("title", "x")
	if err := app.Save(post); err != nil {
		t.Fatal(err)
	}

	can := func(rule string, auth *core.Record) bool {
		t.Helper()
		ok, err := app.CanAccessRecord(post, &core.RequestInfo{Auth: auth, Context: core.RequestInfoContextDefault}, &rule)
		if err != nil {
			t.Fatalf("%s: %v", rule, err)
		}
		return ok
	}

	for _, ast := range []bool{false, true} {
		restore := search.SetRuleASTForTest(ast)
		name := "legacy"
		if ast {
			name = "ast"
		}
		t.Run(name, func(t *testing.T) {
			defer restore()
			for _, rule := range []string{`entitled("pro") = true`, `@entitled("pro") = true`} {
				if can(rule, nil) || can(rule, pro) || can(rule, free) {
					t.Fatalf("%s: nobody is entitled yet", rule)
				}
			}
			// active, no end
			if _, err := m.GrantEntitlement(pro.Id, "members", payments.Grant{Key: "pro"}, payments.EntActive); err != nil {
				t.Fatal(err)
			}
			for _, rule := range []string{`entitled("pro") = true`, `@entitled("pro") = true`, `title = "x" && entitled("pro") = true`, `entitled("pro") = true || title = "nope"`} {
				if !can(rule, pro) {
					t.Errorf("%s: entitled user denied", rule)
				}
				if can(rule, free) || can(rule, nil) {
					t.Errorf("%s: unentitled user allowed", rule)
				}
			}
			if can(`entitled("team") = true`, pro) {
				t.Error("another key must not match")
			}
			if !can(`entitled("team") = false`, pro) {
				t.Error("negation")
			}
			// lapse
			if _, err := m.RevokeEntitlement(pro.Id, "members", "pro"); err != nil {
				t.Fatal(err)
			}
			if can(`entitled("pro") = true`, pro) {
				t.Error("lapsed entitlement must deny")
			}
			// expired `until` in a still "active" row denies without waiting for the sweeper
			e, _ := app.FindFirstRecordByFilter(payments.EntitlementsCollection, "subject={:s}", map[string]any{"s": pro.Id})
			e.Set("status", payments.EntActive)
			e.Set("until", "2000-01-01 00:00:00.000Z")
			if err := app.Save(e); err != nil {
				t.Fatal(err)
			}
			if can(`entitled("pro") = true`, pro) {
				t.Error("elapsed until must deny")
			}
			// grace with a future end allows
			e.Set("status", payments.EntGrace)
			e.Set("until", "2999-01-01 00:00:00.000Z")
			if err := app.Save(e); err != nil {
				t.Fatal(err)
			}
			if !can(`entitled("pro") = true`, pro) {
				t.Error("grace must allow")
			}
			e.Set("status", payments.EntTrial)
			if err := app.Save(e); err != nil {
				t.Fatal(err)
			}
			if !can(`entitled("pro") = true`, pro) {
				t.Error("trial must allow")
			}
			// invalid usage
			for _, bad := range []string{`entitled() = true`, `entitled(title) = true`, `entitled("a","b") = true`} {
				rule := bad
				if _, err := app.CanAccessRecord(post, &core.RequestInfo{Auth: pro, Context: core.RequestInfoContextDefault}, &rule); err == nil {
					t.Errorf("%s must be an error", bad)
				}
			}
			// built-ins untouched
			if !can(`strftime("%Y", "2026-01-02") = "2026"`, nil) {
				t.Error("strftime")
			}
			// reset for the second run
			_ = app.Delete(e)
		})
	}
}
