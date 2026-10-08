//go:build !no_crypto

package crypto

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tokibase/tokibase/core"
)

// Finding is one lint result.
type Finding struct {
	Collection string `json:"collection"`
	Field      string `json:"field"`
	Where      string `json:"where"` // listRule, viewRule, ..., index
	Message    string `json:"message"`
}

func absPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

// Lint finds indexes covering an encrypted field and API rules (of any
// collection) that use an encrypted field in an unsupported way. Rules then
// compare against ciphertext and indexes cannot help: almost always mistakes.
func Lint(app core.App) ([]Finding, error) {
	cfgs, err := List(app)
	if err != nil {
		return nil, err
	}
	out := []Finding{}
	for _, c := range cfgs {
		col, err := app.FindCachedCollectionByNameOrId(c.Collection)
		if err != nil || col == nil {
			out = append(out, Finding{c.Collection, c.Field, "config", "unknown collection"})
			continue
		}
		if col.Fields.GetByName(c.Field) == nil {
			out = append(out, Finding{col.Name, c.Field, "config", "unknown field"})
			continue
		}
		for _, ix := range col.Indexes {
			if strings.Contains(ix, "`"+c.Field+"`") || strings.Contains(ix, "["+c.Field+"]") ||
				strings.Contains(ix, `"`+c.Field+`"`) || strings.Contains(ix, "("+c.Field+")") || strings.Contains(ix, " "+c.Field+",") {
				out = append(out, Finding{col.Name, c.Field, "index",
					"index covers an encrypted field: ciphertext is random, the index is useless (and a unique index cannot work)"})
			}
		}
	}
	out = append(out, lintRules(app)...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Collection < out[j].Collection })
	return out, nil
}

func (m *Module) bootWarn() {
	cfgs, err := List(m.app)
	if err != nil || len(cfgs) == 0 {
		return
	}
	live := 0
	for _, c := range cfgs {
		if c.State != StateStripped {
			live++
		}
	}
	if !m.Active() && live > 0 {
		m.app.Logger().Error("crypto: fields are configured as encrypted but no master key is set; writes to them are refused and reads return ciphertext",
			"env", EnvMasterKey)
	}
	fs, _ := Lint(m.app)
	for _, f := range fs {
		m.app.Logger().Warn("crypto: "+f.Message, "collection", f.Collection, "field", f.Field, "where", f.Where)
	}
}

// lintRules reports every API rule (of any collection) that uses an encrypted
// field in a way the equality rewrite does not support. Such a rule compares
// against ciphertext (or, for an incomplete blind index, is rejected).
func lintRules(app core.App) []Finding {
	m := From(app)
	if m == nil {
		return nil
	}
	cols, err := app.FindAllCollections()
	if err != nil {
		return nil
	}
	var out []Finding
	for _, col := range cols {
		rules := map[string]*string{
			"listRule": col.ListRule, "viewRule": col.ViewRule, "createRule": col.CreateRule,
			"updateRule": col.UpdateRule, "deleteRule": col.DeleteRule,
		}
		if col.IsAuth() {
			rules["authRule"], rules["manageRule"] = col.AuthRule, col.ManageRule
		}
		for name, r := range rules {
			if r == nil || *r == "" {
				continue
			}
			if hit := m.filterViolation(col, *r); hit != "" {
				f := hit
				if i := strings.IndexByte(hit, '.'); i >= 0 {
					f = hit[i+1:]
				}
				out = append(out, Finding{col.Name, f, name, fmt.Sprintf(
					"rule uses encrypted field %q in an unsupported way: only `=`, `!=`, `?=`, `?!=` against a non-empty string on a blind-index field are rewritten (and @request.auth.<encrypted field> is rejected); anything else compares against ciphertext", hit)})
			}
		}
	}
	return out
}
