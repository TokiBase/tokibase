package kernel_test

import (
	"fmt"
	"testing"

	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tests"
	"github.com/tokibase/tokibase/tools/search"
)

func TestRequestAuthKind(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	users, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatal(err)
	}
	user, err := app.FindAuthRecordByEmail("users", "test@example.com")
	if err != nil {
		t.Fatal(err)
	}
	su, err := app.FindAuthRecordByEmail(kernel.CollectionNameSuperusers, "test@example.com")
	if err != nil {
		t.Fatal(err)
	}

	agents := kernel.NewBaseCollection(kernel.CollectionNameAgents)
	agents.Fields.Add(&kernel.TextField{Name: "role"})
	agent := kernel.NewRecord(agents)
	agent.Id = "agent0000000001"
	agent.Set("role", "writer")

	for _, tc := range []struct {
		name string
		auth *kernel.Record
		want string
	}{
		{"guest", nil, "guest"},
		{"user", user, "user"},
		{"superuser", su, "superuser"},
		{"agent", agent, "agent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := kernel.AuthKindOf(tc.auth); got != tc.want {
				t.Fatalf("AuthKindOf = %q, want %q", got, tc.want)
			}
			info := &kernel.RequestInfo{Auth: tc.auth, Query: map[string]string{}, Headers: map[string]string{}, Body: map[string]any{}}
			r := kernel.NewRecordFieldResolver(app, users, info, true)
			e, err := search.FilterData("@request.auth.kind = 'x'").BuildExpr(r)
			if err != nil {
				t.Fatal(err)
			}
			built := app.DB().Select("(1)").From("users").AndWhere(e).Build()
			sql, params := built.SQL(), built.Params()
			found := false
			for _, v := range params {
				if v == tc.want {
					found = true
				}
			}
			if !found {
				t.Fatalf("kind %q not in %s %v", tc.want, sql, fmt.Sprint(params))
			}
		})
	}
}
