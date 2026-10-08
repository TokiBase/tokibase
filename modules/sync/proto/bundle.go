package proto

import "encoding/json"

// SchemaBundle is one schema version of the hub (docs/SYNC_DESIGN.md §3.8): a
// full snapshot of the synced collections plus the config rows, so applying
// any version is idempotent. Hash is the sha256 (hex) of the canonical JSON of
// Bundle (see CanonicalJSON).
type SchemaBundle struct {
	Version int64           `json:"version"`
	Hash    string          `json:"hash"`
	Bundle  json.RawMessage `json:"bundle"`
}

// BundleBody is the content of SchemaBundle.Bundle.
type BundleBody struct {
	// Collections is the export of the synced collections, ids included.
	Collections []map[string]any `json:"collections"`
	// Config holds the rows of the config collections by collection name
	// (`_sync_policies`, `_field_rules`, `_batch_rules`, `_computed_fields`,
	// `_crypto_fields`).
	Config map[string][]map[string]any `json:"config"`
}

// CanonicalJSON re-encodes JSON text with sorted keys and without HTML
// escaping, keeping numbers verbatim. The bundle hash is computed over this
// form on both sides, so a transport that re-escapes the raw bytes does not
// change it.
func CanonicalJSON(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytesReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return encodeNoEscape(v)
}
