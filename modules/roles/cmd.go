//go:build !no_roles

package roles

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/tools/types"
)

func findRole(app core.App, name string) (*core.Record, error) {
	return app.FindFirstRecordByData(RolesName, "name", name)
}

func splitUser(s string) (string, string, error) {
	i := strings.Index(s, "/")
	if i <= 0 || i == len(s)-1 {
		return "", "", errors.New("expected <collection>/<userId>")
	}
	return s[:i], s[i+1:], nil
}

func parseExpires(v string) (types.DateTime, error) {
	if d, err := time.ParseDuration(v); err == nil {
		return types.ParseDateTime(time.Now().UTC().Add(d))
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, v); err == nil {
			return types.ParseDateTime(t.UTC())
		}
	}
	return types.DateTime{}, fmt.Errorf("invalid --expires %q (use RFC3339, YYYY-MM-DD or a duration such as 72h)", v)
}

// NewCommand creates the `roles` command (list, create, rm, grant, revoke, who, lint).
func NewCommand(app core.App) *cobra.Command {
	root := &cobra.Command{
		Use:   "roles",
		Short: "Manage roles and memberships (_roles, _memberships)",
		Long: "Roles are named grants that rules test with @role(\"name\") = true, @role(\"name\", scope, \"collection\") = true and @member(scope, \"collection\") = true.\n" +
			"A membership gives a role to one auth record (any auth collection, or _agents), optionally limited to a scope\n" +
			"(a team, tenant or clan record id plus its collection) and optionally expiring. A running server picks up CLI changes within 5 seconds.",
	}
	out := func(c *cobra.Command) func(string, ...any) {
		return func(f string, a ...any) { fmt.Fprintf(c.OutOrStdout(), f, a...) }
	}

	list := &cobra.Command{
		Use: "list", Short: "List roles with their membership counts", Args: cobra.NoArgs, SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			rows := []struct {
				Name  string `db:"name"`
				Desc  string `db:"description"`
				Count int    `db:"n"`
			}{}
			err := app.DB().NewQuery("SELECT r.name, r.description, (SELECT COUNT(*) FROM {{" + MembershipsName +
				"}} m WHERE m.role = r.id) AS n FROM {{" + RolesName + "}} r ORDER BY r.name").All(&rows)
			if err != nil {
				return err
			}
			for _, r := range rows {
				out(c)("%s\t%d member(s)\t%s\n", r.Name, r.Count, r.Desc)
			}
			return nil
		},
	}

	var desc string
	create := &cobra.Command{
		Use: "create <name>", Short: "Create a role", Args: cobra.ExactArgs(1), SilenceUsage: true,
		RunE: func(c *cobra.Command, a []string) error {
			col, err := app.FindCollectionByNameOrId(RolesName)
			if err != nil {
				return err
			}
			r := core.NewRecord(col)
			r.Set("name", a[0])
			r.Set("description", desc)
			if err := app.Save(r); err != nil {
				return err
			}
			out(c)("created role %s\n", a[0])
			return nil
		},
	}
	create.Flags().StringVar(&desc, "description", "", "description")

	rm := &cobra.Command{
		Use: "rm <name>", Short: "Delete a role and ALL its memberships", Args: cobra.ExactArgs(1), SilenceUsage: true,
		RunE: func(c *cobra.Command, a []string) error {
			r, err := findRole(app, a[0])
			if err != nil {
				return fmt.Errorf("role %s not found", a[0])
			}
			n, _ := app.CountRecords(MembershipsName, dbxHashRole(r.Id))
			if err := app.Delete(r); err != nil {
				return err
			}
			out(c)("deleted role %s and %d membership(s)\n", a[0], n)
			return nil
		},
	}

	var scope, scopeCol, expires string
	grant := &cobra.Command{
		Use: "grant <collection>/<userId> <role>", Short: "Give a role to an auth record", Args: cobra.ExactArgs(2), SilenceUsage: true,
		Example: "roles grant users/abc123 editor --scope team42 --scope-collection teams --expires 720h",
		RunE: func(c *cobra.Command, a []string) error {
			colName, uid, err := splitUser(a[0])
			if err != nil {
				return err
			}
			uc, err := app.FindCollectionByNameOrId(colName)
			if err != nil || (!uc.IsAuth() && uc.Name != agentsCollection) {
				return fmt.Errorf("%s is not an auth collection or _agents", colName)
			}
			if scope != "" && scopeCol == "" {
				return fmt.Errorf("--scope requires --scope-collection (record ids are only unique per collection)")
			}
			if _, err := app.FindRecordById(uc, uid); err != nil {
				return fmt.Errorf("record %s/%s not found", colName, uid)
			}
			role, err := findRole(app, a[1])
			if err != nil {
				return fmt.Errorf("role %s not found (create it with: roles create %s)", a[1], a[1])
			}
			mc, err := app.FindCollectionByNameOrId(MembershipsName)
			if err != nil {
				return err
			}
			m := core.NewRecord(mc)
			m.Set("user_collection", uc.Id)
			m.Set("user", uid)
			m.Set("role", role.Id)
			m.Set("scope", scope)
			m.Set("scope_collection", scopeCol)
			if expires != "" {
				dt, err := parseExpires(expires)
				if err != nil {
					return err
				}
				m.Set("expires", dt)
			}
			if err := app.Save(m); err != nil {
				return err
			}
			out(c)("granted %s to %s/%s\n", a[1], colName, uid)
			return nil
		},
	}
	grant.Flags().StringVar(&scope, "scope", "", "limit the grant to this record id (team, tenant, clan)")
	grant.Flags().StringVar(&scopeCol, "scope-collection", "", "collection of the scope record (required with --scope)")
	grant.Flags().StringVar(&expires, "expires", "", "expiry: RFC3339, YYYY-MM-DD or a duration such as 72h")

	var rvScope string
	revoke := &cobra.Command{
		Use: "revoke <collection>/<userId> <role>", Short: "Remove a role from an auth record", Args: cobra.ExactArgs(2), SilenceUsage: true,
		RunE: func(c *cobra.Command, a []string) error {
			colName, uid, err := splitUser(a[0])
			if err != nil {
				return err
			}
			uc, err := app.FindCollectionByNameOrId(colName)
			if err != nil {
				return fmt.Errorf("unknown collection %s", colName)
			}
			role, err := findRole(app, a[1])
			if err != nil {
				return fmt.Errorf("role %s not found", a[1])
			}
			recs, err := app.FindAllRecords(MembershipsName, dbxMember(uc.Id, uid, role.Id, rvScope))
			if err != nil {
				return err
			}
			for _, r := range recs {
				if err := app.Delete(r); err != nil {
					return err
				}
			}
			out(c)("revoked %d membership(s)\n", len(recs))
			return nil
		},
	}
	revoke.Flags().StringVar(&rvScope, "scope", "", "scope of the grant to remove (empty = the global grant)")

	var whoJSON bool
	var whoScope string
	who := &cobra.Command{
		Use: "who <role>", Short: "List who holds a role", Args: cobra.ExactArgs(1), SilenceUsage: true,
		RunE: func(c *cobra.Command, a []string) error {
			rows := []struct {
				UserCollection string `db:"user_collection" json:"user_collection"`
				User           string `db:"user" json:"user"`
				Scope          string `db:"scope" json:"scope"`
				Expires        string `db:"expires" json:"expires"`
			}{}
			q := "SELECT m.user_collection, m.user, m.scope, m.expires FROM {{" + MembershipsName + "}} m INNER JOIN {{" +
				RolesName + "}} r ON r.id = m.role WHERE r.name = {:n}"
			p := map[string]any{"n": a[0]}
			if c.Flags().Changed("scope") {
				q += " AND m.scope = {:s}"
				p["s"] = whoScope
			}
			if err := app.DB().NewQuery(q + " ORDER BY m.scope, m.user").Bind(p).All(&rows); err != nil {
				return err
			}
			if whoJSON {
				enc := json.NewEncoder(c.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(rows)
			}
			for _, r := range rows {
				name := r.UserCollection
				if col, err := app.FindCachedCollectionByNameOrId(r.UserCollection); err == nil {
					name = col.Name
				}
				out(c)("%s/%s\tscope=%s\texpires=%s\n", name, r.User, r.Scope, r.Expires)
			}
			return nil
		},
	}
	who.Flags().StringVar(&whoScope, "scope", "", "only memberships of this scope")
	who.Flags().BoolVar(&whoJSON, "json", false, "output JSON")

	lint := &cobra.Command{
		Use: "lint", Short: "Warn about rules that use @role() for roles that do not exist", Args: cobra.NoArgs, SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			fs, err := Lint(app)
			if err != nil {
				return err
			}
			for _, f := range fs {
				out(c)("%s\t%s.%s\t%s\n", f.Severity, f.Collection, f.Rule, f.Message)
			}
			if len(fs) == 0 {
				out(c)("ok\n")
				return nil
			}
			return fmt.Errorf("%d finding(s)", len(fs))
		},
	}

	root.AddCommand(list, create, rm, grant, revoke, who, lint)
	return root
}
