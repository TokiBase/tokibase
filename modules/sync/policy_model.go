//go:build !no_sync

package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	stdsync "sync"

	validation "github.com/pocketbase/ozzo-validation/v4"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/dbutils"
	"github.com/tokibase/tokibase/tools/hook"
)

// The policy model of docs/SYNC_DESIGN.md §2.6: schema, partition syntax and the
// validation that runs on save (hub) and in `toki sync policies lint`.

// Lists of allowed values (the select fields use them too).
var (
	policyDirections  = []string{DirBoth, DirPush, DirPull, DirNone}
	policyStrategies  = []string{"lww", "hub-wins", "field-merge", "hook"}
	policyCryptoModes = []string{"ciphertext", "strip"}
)

var (
	partitionRe = regexp.MustCompile(`^\s*([A-Za-z_][A-Za-z0-9_]*)\s*=\s*@node\.([A-Za-z_][A-Za-z0-9_]*)\s*$`)
	reserveRe   = regexp.MustCompile(`^reserve:[A-Za-z][A-Za-z0-9_]{0,63}$`)
)

// parsePartition parses "<field> = @node.<param>". An empty string means "no
// partition" and returns empty names without error.
func parsePartition(s string) (field, param string, err error) {
	if strings.TrimSpace(s) == "" {
		return "", "", nil
	}
	m := partitionRe.FindStringSubmatch(s)
	if m == nil {
		return "", "", fmt.Errorf("invalid partition %q (want \"<field> = @node.<param>\")", s)
	}
	return m[1], m[2], nil
}

// policyFullFields is the complete field list of `_sync_policies`. A collection
// that an older build created gets the missing ones added.
func policyFullFields() []core.Field {
	return []core.Field{
		&core.TextField{Name: "collection", Required: true, Max: 100},
		&core.SelectField{Name: "direction", MaxSelect: 1, Values: policyDirections},
		&core.SelectField{Name: "strategy", MaxSelect: 1, Values: policyStrategies},
		&core.TextField{Name: "partition", Max: 200},
		&core.JSONField{Name: "field_types", MaxSize: 16384},
		&core.JSONField{Name: "exclude", MaxSize: 16384},
		&core.TextField{Name: "hook", Max: 100},
		// pull_view_rule defaults to true when the record is created through the
		// API or the CLI (see bindDefaults); a bool field has no default of its own.
		&core.BoolField{Name: "pull_view_rule"},
		&core.SelectField{Name: "crypto", MaxSelect: 1, Values: policyCryptoModes},
		&core.NumberField{Name: "order", Min: floatPtr(0), OnlyInt: true},
		&core.BoolField{Name: "enabled"},
		&core.BoolField{Name: "review"},
		&core.BoolField{Name: "trusted"},
	}
}

func floatPtr(f float64) *float64 { return &f }

// policyIssue is one finding about a policy row.
type policyIssue struct {
	Collection string `json:"collection"`
	Level      string `json:"level"` // error | warning
	Field      string `json:"field,omitempty"`
	Message    string `json:"message"`
}

func (i policyIssue) String() string {
	f := ""
	if i.Field != "" {
		f = i.Field + ": "
	}
	return fmt.Sprintf("%s %s: %s%s", i.Level, i.Collection, f, i.Message)
}

// partitionable reports whether a field of this type can carry a partition key.
func partitionable(f core.Field) bool {
	switch f.Type() {
	case kernel.FieldTypeText, kernel.FieldTypeEmail, kernel.FieldTypeURL, kernel.FieldTypeNumber, kernel.FieldTypeBool:
		return true
	case kernel.FieldTypeSelect, kernel.FieldTypeRelation:
		m, ok := f.(interface{ IsMultiple() bool })
		return ok && !m.IsMultiple()
	}
	return false
}

func isMultiple(f core.Field) bool {
	m, ok := f.(interface{ IsMultiple() bool })
	return ok && m.IsMultiple()
}

func indexedFirst(col *core.Collection, field string) bool {
	if field == kernel.FieldNameId {
		return true
	}
	for _, raw := range col.Indexes {
		idx := dbutils.ParseIndex(raw)
		if len(idx.Columns) > 0 && strings.Trim(idx.Columns[0].Name, "`\"'[] ") == field && idx.Where == "" {
			return true
		}
	}
	return false
}

// checkPolicy inspects one `_sync_policies` row. Findings with level "error"
// make the save fail; the rest are shown by `toki sync policies lint`.
func checkPolicy(app kernel.App, rec *core.Record) []policyIssue {
	ref := strings.TrimSpace(rec.GetString("collection"))
	var out []policyIssue
	add := func(level, field, format string, a ...any) {
		out = append(out, policyIssue{Collection: ref, Level: level, Field: field, Message: fmt.Sprintf(format, a...)})
	}
	if ref == "" {
		add("error", "collection", "collection is required")
		return out
	}
	enabled := rec.GetBool("enabled")
	col, err := app.FindCachedCollectionByNameOrId(ref)
	if err != nil || col == nil {
		lvl := "warning"
		if enabled {
			lvl = "error"
		}
		add(lvl, "collection", "collection %q does not exist", ref)
		return out
	}
	if !eligible(col) {
		add("error", "collection", "system collections (names starting with \"_\") and views are never synced")
		return out
	}
	// one policy per collection, whether named by name or by id
	if others, err := app.FindAllRecords(PoliciesCollection); err == nil {
		for _, o := range others {
			if o.Id == rec.Id {
				continue
			}
			if oc, err := app.FindCachedCollectionByNameOrId(o.GetString("collection")); err == nil && oc != nil && oc.Id == col.Id {
				add("error", "collection", "collection %q already has a policy (%s)", col.Name, o.Id)
				break
			}
		}
	}

	dir := rec.GetString("direction")
	if dir == "" {
		dir = DirBoth
	}
	if col.IsAuth() && dir != DirPull && dir != DirNone {
		add("error", "direction", "auth collections can only be pull or none in v1 (got %q)", dir)
	}
	if systemAllowed(col) && dir != DirPull && dir != DirNone {
		add("error", "direction", "system collection %q is pull-only (got %q); use --direction pull", col.Name, dir)
	}
	strategy := rec.GetString("strategy")
	if strategy == "" {
		strategy = "lww"
	}
	if (dir == DirPull || dir == DirNone) && strategy != "lww" {
		add("warning", "strategy", "strategy %q has no effect when direction is %q (nothing is pushed)", strategy, dir)
	}
	if strategy != "hook" && rec.GetString("hook") != "" {
		add("warning", "hook", "hook is only used by strategy \"hook\"")
	}

	// exclude
	exclude := map[string]struct{}{}
	if raw := rawJSON(rec, "exclude"); raw != nil {
		var ex []string
		if err := json.Unmarshal(raw, &ex); err != nil {
			add("error", "exclude", "exclude must be a JSON array of field names")
		}
		for _, n := range ex {
			exclude[n] = struct{}{}
			if col.Fields.GetByName(n) == nil {
				add("warning", "exclude", "unknown field %q", n)
			}
		}
	}

	// field_types
	types := map[string]string{}
	if raw := rawJSON(rec, "field_types"); raw != nil {
		var ft map[string]string
		if err := json.Unmarshal(raw, &ft); err != nil {
			add("error", "field_types", "field_types must be a JSON object of field name to type")
		}
		names := make([]string, 0, len(ft))
		for n := range ft {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			typ := ft[n]
			types[n] = typ
			f := col.Fields.GetByName(n)
			if f == nil {
				add("error", "field_types", "unknown field %q", n)
				continue
			}
			switch {
			case typ == TypeCounter:
				if f.Type() != kernel.FieldTypeNumber {
					add("error", "field_types", "counter %q needs a number field (it is %s)", n, f.Type())
				}
			case typ == TypeSet:
				if (f.Type() != kernel.FieldTypeSelect && f.Type() != kernel.FieldTypeRelation) || !isMultiple(f) {
					add("error", "field_types", "set %q needs a multi select or multi relation field (it is %s)", n, f.Type())
				}
			case strings.HasPrefix(typ, "reserve:"):
				// the type is accepted now; the reservation machinery arrives with PR8
				if !reserveRe.MatchString(typ) {
					add("error", "field_types", "%q: reserve needs a sequence name (reserve:<name>)", n)
				}
				if f.Type() != kernel.FieldTypeText && f.Type() != kernel.FieldTypeNumber {
					add("error", "field_types", "reserve %q needs a text or number field (it is %s)", n, f.Type())
				}
			case typ == TypeInclude:
				if !col.IsAuth() || !authSystemFields[n] {
					add("warning", "field_types", "include only has an effect on email, emailVisibility and verified of auth collections")
				}
			default:
				add("error", "field_types", "unknown type %q for %q (counter, set, reserve:<sequence> or include)", typ, n)
			}
			if (typ == TypeCounter || typ == TypeSet) && strategy == "hub-wins" {
				add("warning", "field_types", "%s %q is ignored under strategy hub-wins", typ, n)
			}
			if _, ex := exclude[n]; ex {
				add("warning", "field_types", "%q is both typed and excluded", n)
			}
		}
	}

	// partition
	pf, pp, perr := parsePartition(rec.GetString("partition"))
	switch {
	case perr != nil:
		add("error", "partition", "%s", perr.Error())
	case pf != "":
		f := col.Fields.GetByName(pf)
		switch {
		case f == nil:
			add("error", "partition", "partition field %q does not exist", pf)
		case !partitionable(f):
			add("error", "partition", "partition field %q (%s) must be a single value text, number, bool, email, url, select or relation", pf, f.Type())
		default:
			if _, ex := exclude[pf]; ex {
				add("error", "partition", "partition field %q must not be excluded (the node has to see it)", pf)
			}
			if t := types[pf]; t == TypeCounter || t == TypeSet {
				add("error", "partition", "partition field %q must not be a counter or set", pf)
			}
			if f.GetHidden() || kernel.IsDerived(col.Id, pf) || (col.IsAuth() && isAuthSystemName(pf)) {
				// hidden, derived and auth-system fields are never synced: the node
				// could neither see nor set the key, and changes of it would not
				// produce a change row
				add("error", "partition", "partition field %q is hidden, derived or an auth system field and never travels to the spokes", pf)
			}
			if !indexedFirst(col, pf) {
				add("warning", "partition", "partition field %q is not indexed (pull and push check it on every change)", pf)
			}
		}
		if pp == "" {
			add("error", "partition", "missing node parameter")
		}
	}

	// file fields never travel in v1
	for _, f := range col.Fields {
		if f.Type() == kernel.FieldTypeFile {
			if _, ex := exclude[f.GetName()]; !ex {
				add("warning", "exclude", "file field %q is not synced in v1 (add it to exclude to silence this)", f.GetName())
			}
		}
	}
	if col.ViewRule == nil && rec.GetBool("pull_view_rule") && !rec.GetBool("trusted") && dir != DirPush && dir != DirNone {
		add("warning", "trusted", "viewRule is null (superusers only): the collection is never pulled unless trusted=true or pull_view_rule=false")
	}
	if !rec.GetBool("pull_view_rule") && col.ViewRule != nil && *col.ViewRule != "" && dir != DirPush && dir != DirNone {
		add("warning", "pull_view_rule", "pull_view_rule is off but viewRule is set: every record of the collection is pulled whatever the viewRule says")
	}
	if rec.GetBool("trusted") && col.ViewRule != nil {
		add("warning", "trusted", "trusted only matters for a collection whose viewRule is null")
	}
	return out
}

// policyValidationErrors turns the "error" findings into validation errors.
func policyValidationErrors(issues []policyIssue) error {
	errs := validation.Errors{}
	for _, i := range issues {
		if i.Level != "error" {
			continue
		}
		f := i.Field
		if f == "" {
			f = "collection"
		}
		if _, dup := errs[f]; dup {
			continue
		}
		errs[f] = validation.NewError("validation_sync_policy", i.Message)
	}
	if len(errs) == 0 {
		return nil
	}
	return errs
}

// bindPolicyModel validates policy rows on save (hub only: a spoke receives the
// hub's rows and must accept them even before its collections exist) and gives a
// policy created through the API or the CLI its defaults.
func (c *policyCache) bindPolicyModel() {
	app := c.m.app
	if c.m.role != RoleHub {
		return
	}
	app.OnRecordValidate(PoliciesCollection).Bind(&hook.Handler[*core.RecordEvent]{
		Id: hookId + "polval", Func: func(e *core.RecordEvent) error {
			if err := policyValidationErrors(checkPolicy(e.App, e.Record)); err != nil {
				return err
			}
			return e.Next()
		},
	})
	// pull_view_rule defaults to true on EVERY creation path. A bool field cannot
	// tell "absent" from "false", so a creator that really wants false states it:
	// the REST body carries the key, SetPolicy (CLI) saves with
	// [withExplicitViewRule]; a Go caller does the same or updates the record
	// after creating it.
	app.OnRecordCreateRequest(PoliciesCollection).Bind(&hook.Handler[*core.RecordRequestEvent]{
		Id: hookId + "poldef", Func: func(e *core.RecordRequestEvent) error {
			if ri, err := e.RequestInfo(); err == nil {
				if _, has := ri.Body["pull_view_rule"]; has {
					explicitViewRule.Store(e.Record, struct{}{})
				}
			}
			return e.Next()
		},
	})
	app.OnRecordCreate(PoliciesCollection).Bind(&hook.Handler[*core.RecordEvent]{
		Id: hookId + "poldefm", Priority: -1000, Func: func(e *core.RecordEvent) error {
			_, explicit := explicitViewRule.LoadAndDelete(e.Record)
			if !explicit && (e.Context == nil || e.Context.Value(explicitViewRuleKey{}) == nil) {
				e.Record.Set("pull_view_rule", true)
			}
			return e.Next()
		},
	})
}

type explicitViewRuleKey struct{}

var explicitViewRule stdsync.Map

// withExplicitViewRule marks a save whose pull_view_rule value is deliberate.
func withExplicitViewRule(ctx context.Context) context.Context {
	return context.WithValue(ctx, explicitViewRuleKey{}, true)
}

func isAuthSystemName(n string) bool {
	switch n {
	case kernel.FieldNameId, kernel.FieldNameTokenKey, kernel.FieldNamePassword, "email", "emailVisibility", "verified":
		return true
	}
	return false
}
