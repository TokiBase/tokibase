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
// into plain JSON data: nil, bool, string, float64, []any, map[string]any.
func normalize(v any) any {
	switch t := v.(type) {
	case nil, bool, string, float64:
		return t
	case int:
		return float64(t)
	case int64:
		return float64(t)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil
	}
	return out
}

// canonicalJSON encodes v with sorted object keys, numbers as
// strconv.FormatFloat(v,'g',-1,64) and no HTML escaping.
func canonicalJSON(v any) []byte {
	var buf bytes.Buffer
	writeCanon(&buf, normalize(v))
	return buf.Bytes()
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
		writeCanon(buf, normalize(t))
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
	writeCanon(&buf, normalize(fields))
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

// fieldValues reads the normalized DB-export values of fields from rec.
func fieldValues(rec *core.Record, fields []core.Field) (map[string]any, error) {
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
		out[f.GetName()] = normalize(v)
	}
	return out, nil
}

// RecordHash returns the canonical hash of rec for policy p (nil policy =
// no `exclude` list). It is what `_changes.hash` and `_sync_meta.hash` hold.
func RecordHash(rec *core.Record, p *policy) ([]byte, error) {
	col := rec.Collection()
	vals, err := fieldValues(rec, syncedFields(col, p))
	if err != nil {
		return nil, err
	}
	return canonicalHash(col.Id, rec.Id, vals), nil
}
