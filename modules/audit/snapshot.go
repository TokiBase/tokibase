//go:build !no_audit

package audit

import (
	"encoding/json"
	"reflect"
	"strings"
	"unicode/utf8"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/filesystem"
)

const redacted = "[redacted]"

// isSecretKey reports whether a JSON/field key must never reach the log.
func isSecretKey(k string) bool {
	k = strings.ToLower(k)
	switch k {
	case "tokenkey", "token", "privatekey", "apikey", "otp":
		return true
	}
	return strings.Contains(k, "password") || strings.Contains(k, "secret")
}

// normalize converts any value to plain JSON types (numbers kept as json.Number).
func normalize(v any) any {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return decode(raw)
}

func decode(raw []byte) any {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var out any
	if dec.Decode(&out) != nil {
		return nil
	}
	return out
}

// strip removes secret keys recursively.
func strip(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if isSecretKey(k) {
				delete(t, k)
				continue
			}
			t[k] = strip(val)
		}
	case []any:
		for i := range t {
			t[i] = strip(t[i])
		}
	}
	return v
}

// fileNames reduces file field values to names only (never contents).
func fileNames(v any) any {
	switch t := v.(type) {
	case *filesystem.File:
		return t.Name
	case string:
		return t
	case []string:
		return t
	case []*filesystem.File:
		out := make([]string, len(t))
		for i, f := range t {
			out[i] = f.Name
		}
		return out
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Slice {
		out := make([]any, rv.Len())
		for i := range out {
			out[i] = fileNames(rv.Index(i).Interface())
		}
		return out
	}
	return nil
}

// snapshot is a normalized, NOT yet stripped, copy of an object.
// Stripping happens in finalize so secret changes can still show in a diff.
type snapshot map[string]any

func recordSnapshot(r *core.Record) snapshot {
	if r == nil {
		return nil
	}
	out := snapshot{}
	for _, f := range r.Collection().Fields {
		name := f.GetName()
		v := r.Get(name)
		if f.Type() == core.FieldTypeFile {
			v = fileNames(v)
		}
		if f.Type() == core.FieldTypePassword {
			// Get() returns the plain value; keep only the hash, solely to
			// detect changes (it is stripped from the stored snapshot)
			if pv, ok := r.GetRaw(name).(*core.PasswordFieldValue); ok && pv != nil {
				v = pv.Hash
			} else {
				v = nil
			}
		}
		if kernel.IsSensitive(r.Collection().Id, name) {
			// encrypted fields: neither plaintext nor ciphertext reaches the log
			if s, ok := v.(string); ok && s == "" || v == nil {
				v = nil
			} else {
				v = kernel.SensitiveMarker
			}
		}
		out[name] = normalize(v)
	}
	if r.Collection().IsAuth() {
		// secrets are tracked (to detect changes) even when the collection
		// field list does not declare them; stripped from stored snapshots
		if _, ok := out["tokenKey"]; !ok {
			out["tokenKey"] = r.GetString("tokenKey")
		}
		if _, ok := out["password"]; !ok {
			if pv, ok := r.GetRaw("password").(*core.PasswordFieldValue); ok && pv != nil {
				out["password"] = pv.Hash
			}
		}
	}
	return out
}

func objectSnapshot(v any) snapshot {
	m, _ := normalize(v).(map[string]any)
	return snapshot(m)
}

// diffOf returns changed top-level keys -> {old,new}, redacting secret values.
func diffOf(before, after snapshot) map[string]any {
	diff := map[string]any{}
	keys := map[string]struct{}{}
	for k := range before {
		keys[k] = struct{}{}
	}
	for k := range after {
		keys[k] = struct{}{}
	}
	for k := range keys {
		if reflect.DeepEqual(before[k], after[k]) {
			continue
		}
		if isSecretKey(k) {
			diff[k] = map[string]any{"old": redacted, "new": redacted}
			continue
		}
		diff[k] = map[string]any{"old": strip(before[k]), "new": strip(after[k])}
	}
	return diff
}

// finalize strips secrets and caps the size; returns nil for a nil input.
func finalize(s snapshot) *string {
	if s == nil {
		return nil
	}
	// strip works on a deep copy: the raw snapshot is still needed for the diff
	return capJSON(strip(normalize(map[string]any(s))))
}

func capJSON(v any) *string {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	if len(raw) > maxJSONBytes {
		n := truncatedPreviewBytes
		for n > 0 && !utf8.Valid(raw[:n]) {
			n--
		}
		raw, _ = json.Marshal(map[string]any{
			truncatedMarker:  true,
			"original_bytes": len(raw),
			"preview":        string(raw[:n]),
		})
	}
	s := string(raw)
	return &s
}
