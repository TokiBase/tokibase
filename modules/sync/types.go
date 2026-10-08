//go:build !no_sync

package sync

import (
	"encoding/json"
	"math"
	"sort"
	"strings"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/sync/proto"
)

// Counter and set operations (docs/SYNC_DESIGN.md §4.5).
//
// A spoke captures a counter as {"$inc": delta} and a set as
// {"$add": [...], "$rm": [...]}. The hub replays them through PocketBase's own
// modifiers, so update rules and hooks see the same body a REST client would
// send (`fee+`, `tags+`, `tags-`), and it never needs a conflict decision:
// increments commute, and set operations are applied in hub arrival order.
//
// Known limit of the set type (no OR-set metadata in v1): "remove x" on one
// node and "add x" on another leave x present only when the add arrives last.

// Patch operation keys.
const (
	opInc = "$inc"
	opAdd = "$add"
	opRm  = "$rm"
)

// opOf returns v as a counter/set operation when name is declared `counter` or
// `set` in the policy field types (types) and v is an object carrying a
// `$inc`/`$add`/`$rm` key. On any other field an object is a plain value (a JSON
// field may legitimately hold such keys); validateTyped refuses the rest.
func opOf(types map[string]string, name string, v any) (map[string]any, bool) {
	if t := types[name]; t != TypeCounter && t != TypeSet {
		return nil, false
	}
	mp, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	_, inc := mp[opInc]
	_, add := mp[opAdd]
	_, rm := mp[opRm]
	return mp, inc || add || rm
}

// applyTyped replays a counter/set operation through PocketBase's modifiers.
func applyTyped(rec *core.Record, name string, op map[string]any) {
	for mk, mv := range typedModifiers(name, op) {
		rec.Set(mk, mv)
	}
}

// validateTyped enforces the policy `field_types` on a pushed patch
// (docs/SYNC_DESIGN.md §4.5): `$inc` only on a declared counter (finite
// number), `$add`/`$rm` only on a declared set (arrays), no other `$` key, and
// no absolute value for a counter or set unless the record is being created.
// A spoke could otherwise wrap a plain field in an operation to dodge the
// conflict strategy, or overwrite other nodes' increments with an absolute value.
func validateTyped(types map[string]string, fields map[string]core.Field, patch map[string]any, isNew bool) *rejection {
	for _, name := range sortedKeys(patch) {
		v := patch[name]
		f, ok := fields[name]
		if !ok {
			continue // not synced: ignored by the apply loop
		}
		mp, isMap := v.(map[string]any)
		dollar := false
		for k := range mp {
			if strings.HasPrefix(k, "$") {
				dollar = true
			}
		}
		switch types[name] {
		case TypeCounter:
			if !isMap {
				if isNew {
					continue
				}
				return reject(proto.CodeValidationFailed, "counter field "+name+": send {\"$inc\": n}, not an absolute value")
			}
			d, ok := mp[opInc].(float64)
			if len(mp) != 1 || !ok || math.IsNaN(d) || math.IsInf(d, 0) {
				return reject(proto.CodeValidationFailed, "counter field "+name+": want exactly {\"$inc\": <finite number>}")
			}
		case TypeSet:
			if !isMap {
				if _, isList := v.([]any); isList && isNew {
					continue
				}
				return reject(proto.CodeValidationFailed, "set field "+name+": send {\"$add\": [...], \"$rm\": [...]}, not an absolute value")
			}
			if len(mp) == 0 {
				return reject(proto.CodeValidationFailed, "set field "+name+": empty operation")
			}
			for k, x := range mp {
				if _, isList := x.([]any); (k != opAdd && k != opRm) || !isList {
					return reject(proto.CodeValidationFailed, "set field "+name+": unknown operation "+k)
				}
			}
		default:
			if dollar && f.Type() != kernel.FieldTypeJSON {
				return reject(proto.CodeValidationFailed, "field "+name+" is not a counter or set: operations are not accepted")
			}
		}
	}
	return nil
}

// splitTyped separates a patch into plain field values and typed operations.
func splitTyped(types map[string]string, patch map[string]any) (plain, typed map[string]any) {
	plain, typed = map[string]any{}, map[string]any{}
	for k, v := range patch {
		if _, ok := opOf(types, k, v); ok {
			typed[k] = v
		} else {
			plain[k] = v
		}
	}
	return plain, typed
}

// typedModifiers maps one typed operation to the PocketBase body modifiers:
// {"$inc": 5} -> {"fee+": 5}, {"$add": [...]} -> {"tags+": [...]} and
// {"$rm": [...]} -> {"tags-": [...]}. A negative increment is sent as a plain
// `field+` with a negative number (PocketBase adds it), which keeps one code
// path for both signs.
func typedModifiers(field string, op map[string]any) map[string]any {
	out := map[string]any{}
	if d, ok := op[opInc]; ok {
		out[field+"+"] = d
	}
	if add, ok := op[opAdd]; ok {
		out[field+"+"] = add
	}
	if rm, ok := op[opRm]; ok {
		out[field+"-"] = rm
	}
	return out
}

// bodyModifiers maps every typed operation of patch (see typedModifiers).
func bodyModifiers(types map[string]string, patch map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range patch {
		if op, ok := opOf(types, k, v); ok {
			for mk, mv := range typedModifiers(k, op) {
				out[mk] = mv
			}
		}
	}
	return out
}

// setApply returns list with the $add elements appended (when absent) and the
// $rm elements removed, in that order. It is the model of what PocketBase's
// `tags+` / `tags-` modifiers do to a multi select.
func setApply(list []any, add, rm []any) []any {
	key := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	out := make([]any, 0, len(list)+len(add))
	seen := map[string]bool{}
	for _, v := range list {
		if k := key(v); !seen[k] {
			seen[k] = true
			out = append(out, v)
		}
	}
	for _, v := range add {
		if k := key(v); !seen[k] {
			seen[k] = true
			out = append(out, v)
		}
	}
	if len(rm) > 0 {
		drop := map[string]bool{}
		for _, v := range rm {
			drop[key(v)] = true
		}
		kept := out[:0]
		for _, v := range out {
			if !drop[key(v)] {
				kept = append(kept, v)
			}
		}
		out = kept
	}
	return out
}

// opLists returns the $add and $rm lists of a typed operation.
func opLists(op map[string]any) (add, rm []any) {
	add, _ = op[opAdd].([]any)
	rm, _ = op[opRm].([]any)
	return add, rm
}

// opDelta returns the numeric $inc of a typed operation.
func opDelta(op map[string]any) float64 {
	d, _ := op[opInc].(float64)
	return d
}

// sortedKeys returns the keys of m in order (stable conflict rows and tests).
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// applyTypedOps applies the typed operations of patch to rec through the
// PocketBase modifiers. Names that are not in allowed are skipped.
func applyTypedOps(rec *core.Record, types map[string]string, patch map[string]any, allowed func(string) bool) {
	for name, v := range patch {
		op, ok := opOf(types, name, v)
		if !ok || !allowed(name) {
			continue
		}
		applyTyped(rec, name, op)
	}
}

// plainFields lists the plain (non counter/set operation) fields of a patch.
func plainFields(types map[string]string, patch map[string]any) []string {
	var out []string
	for k, v := range patch {
		if t := types[k]; t == TypeCounter || t == TypeSet {
			continue // typed fields never have a clock (a create carries them as absolute values)
		}
		if _, ok := opOf(types, k, v); !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
