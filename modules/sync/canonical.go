//go:build !no_sync

package sync

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"sort"
	"strconv"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
)

// normalize turns a field value (DB export form: ciphertext stays "tkc1:…")
// into plain JSON data: nil, bool, string, float64, json.Number, []any,
// map[string]any. Numbers inside JSON values stay json.Number (decoded with
// UseNumber) so integers above 2^53 survive; number fields are float64.
func normalize(v any) (any, error) {
	switch t := v.(type) {
	case nil, bool, string, float64, json.Number:
		return t, nil
	case int:
		return float64(t), nil
	case int64:
		return float64(t), nil
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			n, err := normalize(x)
			if err != nil {
				return nil, err
			}
			out[k] = n
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			n, err := normalize(x)
			if err != nil {
				return nil, err
			}
			out[i] = n
		}
		return out, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// canonicalJSON encodes v with sorted object keys, float64 as
// strconv.FormatFloat(v,'g',-1,64), json.Number verbatim and no HTML escaping.
// A value that cannot be normalized encodes as null (values reaching here come
// from fieldValues, which reports the error).
func canonicalJSON(v any) []byte {
	var buf bytes.Buffer
	n, _ := normalize(v)
	writeCanon(&buf, n)
	return buf.Bytes()
}

// canonicalSet sorts the elements of an array by canonical form and drops
// duplicates: the stored order of a `set` field is not significant.
func canonicalSet(v any) any {
	arr, ok := v.([]any)
	if !ok {
		return v
	}
	type el struct {
		key string
		v   any
	}
	els := make([]el, 0, len(arr))
	for _, x := range arr {
		els = append(els, el{string(canonicalJSON(x)), x})
	}
	sort.SliceStable(els, func(i, j int) bool { return els[i].key < els[j].key })
	out := make([]any, 0, len(els))
	for i, e := range els {
		if i > 0 && els[i-1].key == e.key {
			continue
		}
		out = append(out, e.v)
	}
	return out
}

func writeCanonString(buf *bytes.Buffer, s string) {
	var sb bytes.Buffer
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	buf.Write(bytes.TrimRight(sb.Bytes(), "\n"))
}

func writeCanon(buf *bytes.Buffer, v any) {
	switch t := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if t {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case string:
		writeCanonString(buf, t)
	case float64:
		buf.WriteString(strconv.FormatFloat(t, 'g', -1, 64))
	case json.Number:
		buf.WriteString(string(t))
	case []any:
		buf.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeCanon(buf, e)
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeCanonString(buf, k)
			buf.WriteByte(':')
			writeCanon(buf, t[k])
		}
		buf.WriteByte('}')
	default:
		n, _ := normalize(t)
		writeCanon(buf, n)
	}
}

// canonicalHash is sha256 of {"c": collectionId, "id": id, "f": {sorted synced fields}}.
func canonicalHash(collectionId, id string, fields map[string]any) []byte {
	var buf bytes.Buffer
	buf.WriteString(`{"c":`)
	writeCanonString(&buf, collectionId)
	buf.WriteString(`,"id":`)
	writeCanonString(&buf, id)
	buf.WriteString(`,"f":`)
	writeCanon(&buf, fields)
	buf.WriteByte('}')
	sum := sha256.Sum256(buf.Bytes())
	return sum[:]
}

// syncedFields lists the fields of col that travel: everything except id,
// file, password, tokenKey, derived (computed) fields and the policy
// `exclude` list.
func syncedFields(col *core.Collection, p *policy) []core.Field {
	out := make([]core.Field, 0, len(col.Fields))
	for _, f := range col.Fields {
		name := f.GetName()
		switch {
		case name == kernel.FieldNameId,
			name == kernel.FieldNameTokenKey,
			name == kernel.FieldNamePassword,
			f.Type() == kernel.FieldTypeFile,
			f.Type() == kernel.FieldTypePassword,
			kernel.IsDerived(col.Id, name):
			continue
		}
		if p != nil {
			if _, ex := p.Exclude[name]; ex {
				continue
			}
		}
		out = append(out, f)
	}
	return out
}

// fieldValues reads the normalized DB-export values of fields from rec. Fields
// typed `set` in types are canonicalised (sorted, unique) so that the hash and
// the $add/$rm delta do not depend on the stored order.
func fieldValues(rec *core.Record, fields []core.Field, types map[string]string) (map[string]any, error) {
	out := make(map[string]any, len(fields))
	for _, f := range fields {
		var v any
		if dv, ok := f.(core.DriverValuer); ok {
			x, err := dv.DriverValue(rec)
			if err != nil {
				return nil, err
			}
			v = x
		} else {
			v = rec.GetRaw(f.GetName())
		}
		n, err := normalize(v)
		if err != nil {
			return nil, errf("field %q: %w", f.GetName(), err)
		}
		if types[f.GetName()] == TypeSet {
			n = canonicalSet(n)
		}
		out[f.GetName()] = n
	}
	return out, nil
}

// RecordHash returns the canonical hash of rec for policy p (nil policy =
// no `exclude` list). It is what `_changes.hash` and `_sync_meta.hash` hold.
func RecordHash(rec *core.Record, p *policy) ([]byte, error) {
	col := rec.Collection()
	var types map[string]string
	if p != nil {
		types = p.Types
	}
	vals, err := fieldValues(rec, syncedFields(col, p), types)
	if err != nil {
		return nil, err
	}
	return canonicalHash(col.Id, rec.Id, vals), nil
}
