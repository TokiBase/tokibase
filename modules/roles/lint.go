//go:build !no_roles

package roles

import (
	"regexp"
	"sort"

	"github.com/tokibase/tokibase/core"
)

// Finding is one lint result.
type Finding struct {
	Collection string `json:"collection"`
	Rule       string `json:"rule"`
	Role       string `json:"role"`
	Severity   string `json:"severity"`
	Message    string `json:"message"`
}

var reRoleCall = regexp.MustCompile(`@role\(\s*(?:"((?:[^"\\]|\\.)*)"|'((?:[^'\\]|\\.)*)')`)

// Lint warns about collection rules that use @role("x") for a role that does not exist.
func Lint(app core.App) ([]Finding, error) {
	rows := []struct {
		Name string `db:"name"`
	}{}
	if err := app.DB().NewQuery("SELECT name FROM {{" + RolesName + "}}").All(&rows); err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, r := range rows {
		known[r.Name] = true
	}
	cols, err := app.FindAllCollections()
	if err != nil {
		return nil, err
	}
	out := []Finding{}
	for _, c := range cols {
		rules := map[string]*string{
			"listRule": c.ListRule, "viewRule": c.ViewRule, "createRule": c.CreateRule,
			"updateRule": c.UpdateRule, "deleteRule": c.DeleteRule,
		}
		if c.IsAuth() {
			rules["authRule"], rules["manageRule"] = c.AuthRule, c.ManageRule
		}
		for kind, rule := range rules {
			if rule == nil {
				continue
			}
			for _, m := range reRoleCall.FindAllStringSubmatch(*rule, -1) {
				name := m[1] + m[2]
				if !known[name] {
					out = append(out, Finding{Collection: c.Name, Rule: kind, Role: name, Severity: "warning",
						Message: "role " + name + " does not exist in _roles"})
				}
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Collection != out[j].Collection {
			return out[i].Collection < out[j].Collection
		}
		return out[i].Rule < out[j].Rule
	})
	return out, nil
}
