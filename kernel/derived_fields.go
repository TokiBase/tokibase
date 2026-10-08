package kernel

import (
	"sort"
	"sync"
)

// derivedFields is a process-wide registry of "collection id -> field names"
// whose value is derived from other records (computed rollups). Such fields
// are recomputed locally on every node, so modules/sync never captures them.
// It mirrors the sensitive field registry: modules must not import each
// other, this is the shared seam between modules/computed and modules/sync.
var derivedFields = struct {
	mu sync.RWMutex
	m  map[string]map[string]struct{}
}{m: map[string]map[string]struct{}{}}

// RegisterDerivedField marks collectionId.field as derived.
func RegisterDerivedField(collectionId, field string) {
	if collectionId == "" || field == "" {
		return
	}
	derivedFields.mu.Lock()
	defer derivedFields.mu.Unlock()
	if derivedFields.m[collectionId] == nil {
		derivedFields.m[collectionId] = map[string]struct{}{}
	}
	derivedFields.m[collectionId][field] = struct{}{}
}

// UnregisterDerivedField removes a registration.
func UnregisterDerivedField(collectionId, field string) {
	derivedFields.mu.Lock()
	defer derivedFields.mu.Unlock()
	delete(derivedFields.m[collectionId], field)
	if len(derivedFields.m[collectionId]) == 0 {
		delete(derivedFields.m, collectionId)
	}
}

// IsDerived reports whether collectionId.field is registered as derived.
func IsDerived(collectionId, field string) bool {
	derivedFields.mu.RLock()
	defer derivedFields.mu.RUnlock()
	_, ok := derivedFields.m[collectionId][field]
	return ok
}

// DerivedFieldsOf returns the sorted derived field names of a collection.
func DerivedFieldsOf(collectionId string) []string {
	derivedFields.mu.RLock()
	defer derivedFields.mu.RUnlock()
	set := derivedFields.m[collectionId]
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
