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

// Lint finds API rules and indexes of a collection that reference one of its
// encrypted fields. Rules compare against ciphertext and indexes cannot help,
// so both are almost always mistakes.
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
			for _, tok := range tokens(*r) {
				p := stripMod(strings.SplitN(tok, ".", 2)[0])
				if p == c.Field {
					out = append(out, Finding{col.Name, c.Field, name,
						fmt.Sprintf("rule references encrypted field %q: it compares against ciphertext", c.Field)})
					break
				}
			}
		}
		for _, ix := range col.Indexes {
			if strings.Contains(ix, "`"+c.Field+"`") || strings.Contains(ix, "["+c.Field+"]") ||
				strings.Contains(ix, `"`+c.Field+`"`) || strings.Contains(ix, "("+c.Field+")") || strings.Contains(ix, " "+c.Field+",") {
				out = append(out, Finding{col.Name, c.Field, "index",
					"index covers an encrypted field: ciphertext is random, the index is useless (and a unique index cannot work)"})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Collection < out[j].Collection })
	return out, nil
}

func (m *Module) bootWarn() {
	cfgs, err := List(m.app)
	if err != nil || len(cfgs) == 0 {
		return
	}
	if !m.Active() {
		m.app.Logger().Error("crypto: fields are configured as encrypted but no master key is set; writes to them are refused and reads return ciphertext",
			"env", EnvMasterKey)
	}
	fs, _ := Lint(m.app)
	for _, f := range fs {
		m.app.Logger().Warn("crypto: "+f.Message, "collection", f.Collection, "field", f.Field, "where", f.Where)
	}
}
