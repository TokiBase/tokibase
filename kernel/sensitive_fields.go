package kernel

import (
	"sort"
	"sync"
)

// SensitiveMarker is the value substituted for the content of a sensitive
// field in places that must never persist or forward it (audit log, webhook
// payloads, MCP exports).
const SensitiveMarker = "[encrypted]"

// sensitiveFields is a process-wide registry of "collection id -> field names"
// whose values must not leave through side channels. It is filled by modules
// that protect field content (modules/crypto) and consulted by the modules that
// copy records elsewhere (audit, webhooks, mcp). Modules must not import each
// other, so this registry is the shared seam.
var sensitiveFields = struct {
	mu sync.RWMutex
	m  map[string]map[string]struct{}
}{m: map[string]map[string]struct{}{}}

// RegisterSensitiveField marks collectionId.field as sensitive.
func RegisterSensitiveField(collectionId, field string) {
	if collectionId == "" || field == "" {
		return
	}
	sensitiveFields.mu.Lock()
	defer sensitiveFields.mu.Unlock()
	if sensitiveFields.m[collectionId] == nil {
		sensitiveFields.m[collectionId] = map[string]struct{}{}
	}
	sensitiveFields.m[collectionId][field] = struct{}{}
}

// UnregisterSensitiveField removes a registration.
func UnregisterSensitiveField(collectionId, field string) {
	sensitiveFields.mu.Lock()
	defer sensitiveFields.mu.Unlock()
	delete(sensitiveFields.m[collectionId], field)
	if len(sensitiveFields.m[collectionId]) == 0 {
		delete(sensitiveFields.m, collectionId)
	}
}

// IsSensitive reports whether collectionId.field is registered as sensitive.
func IsSensitive(collectionId, field string) bool {
	sensitiveFields.mu.RLock()
	defer sensitiveFields.mu.RUnlock()
	_, ok := sensitiveFields.m[collectionId][field]
	return ok
}

// SensitiveFieldsOf returns the sorted sensitive field names of a collection.
func SensitiveFieldsOf(collectionId string) []string {
	sensitiveFields.mu.RLock()
	defer sensitiveFields.mu.RUnlock()
	set := sensitiveFields.m[collectionId]
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for f := range set {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// RedactExport replaces, in an export of rec (for example the result of
// [Record.PublicExport]), the value of every sensitive field that is present
// and non-empty by marker (SensitiveMarker when empty). Records found in the
// "expand" entry are exported and redacted recursively. The map is modified
// in place and returned.
func RedactExport(rec *Record, export map[string]any, marker string) map[string]any {
	if rec == nil || export == nil {
		return export
	}
	if marker == "" {
		marker = SensitiveMarker
	}
	for _, f := range SensitiveFieldsOf(rec.Collection().Id) {
		if v, ok := export[f]; ok && !isEmptyExport(v) {
			export[f] = marker
		}
	}
	if ex, ok := export[FieldNameExpand].(map[string]any); ok {
		out := make(map[string]any, len(ex))
		for k, v := range ex {
			switch t := v.(type) {
			case *Record:
				out[k] = RedactExport(t, t.PublicExport(), marker)
			case []*Record:
				list := make([]any, len(t))
				for i, r := range t {
					list[i] = RedactExport(r, r.PublicExport(), marker)
				}
				out[k] = list
			default:
				out[k] = v
			}
		}
		export[FieldNameExpand] = out
	}
	return export
}

func isEmptyExport(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	}
	return false
}
