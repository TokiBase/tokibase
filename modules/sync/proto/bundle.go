package proto

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"strconv"
)

// SchemaBundle is one schema version of the hub (docs/SYNC_DESIGN.md §3.8): a
// full snapshot of the synced collections plus the config rows, so applying
// any version is idempotent. Hash is the sha256 (hex) of the canonical JSON of
// Bundle (see CanonicalJSON).
type SchemaBundle struct {
	Version int64           `json:"version"`
	Hash    string          `json:"hash"`
	Bundle  json.RawMessage `json:"bundle"`
	// Sig is the hub signature over node|version|hash (SignBundle).
	Sig string `json:"sig,omitempty"`
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

func bundleDigest(node string, version int64, hash string) []byte {
	return []byte("toki-sync-bundle|" + node + "|" + strconv.FormatInt(version, 10) + "|" + hash)
}

// SignBundle signs a bundle for one node with the hub key, so a spoke does not
// depend on the transport alone to trust the schema it applies (§7.10).
func SignBundle(priv ed25519.PrivateKey, node string, version int64, hash string) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(priv, bundleDigest(node, version, hash)))
}

// VerifyBundle checks SignBundle.
func VerifyBundle(pub ed25519.PublicKey, node string, version int64, hash, sig string) bool {
	b, err := base64.StdEncoding.DecodeString(sig)
	return err == nil && len(pub) == ed25519.PublicKeySize && ed25519.Verify(pub, bundleDigest(node, version, hash), b)
}
