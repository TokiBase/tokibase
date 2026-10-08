//go:build !no_sync

package sync

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/sync/proto"
)

// Encrypted fields (docs/SYNC_DESIGN.md §7.6): the ciphertext syncs verbatim and
// the hub ships the collection data keys to each device, wrapped to the X25519
// key of the node. The wrapping lives in modules/crypto behind
// kernel.SyncKeyProvider; this file only decides WHICH collections a node gets
// keys for.

// stripped reports whether name is an encrypted field that the policy withholds
// from the nodes (`crypto: strip`). Such a field is not part of the synced field
// set at all: it is not captured, not hashed, not pulled and not accepted from a
// push, so the hash and the digest of a record leave it out on EVERY node.
func stripped(col *core.Collection, p *policy, name string) bool {
	return p != nil && p.Crypto == CryptoStrip && kernel.IsSensitive(col.Id, name)
}

// CryptoStrip is the `crypto` policy value that withholds encrypted fields.
const CryptoStrip = "strip"

// keyCollections lists the ids of the collections a node needs keys for: an
// enabled policy that syncs the collection in any direction (a push-only node
// still encrypts its own writes), `crypto` other than strip, and encrypted
// fields configured.
func (m *Module) keyCollections(kp kernel.SyncKeyProvider) ([]string, error) {
	rows, err := m.pol.load()
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	for k, p := range rows {
		if p.ColID == "" || k != p.ColID || p.Direction == DirNone || p.Crypto == CryptoStrip {
			continue
		}
		if kp.NeedsKeys(p.ColID) {
			seen[p.ColID] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

// handshakeKeys builds the `keys` of a handshake answer: every data key version
// (retired ones as such) of the collections the node may read, wrapped to its
// X25519 key. A new version after a rotation shows in the next handshake.
func (m *Module) handshakeKeys(node *core.Record) ([]proto.Key, error) {
	out := []proto.Key{}
	kp := kernel.SyncKeyProviderOf(m.app)
	if kp == nil {
		return out, nil
	}
	ids, err := m.keyCollections(kp)
	if err != nil || len(ids) == 0 {
		return out, err
	}
	kx, ok := decodeKey(node.GetString("kx_pubkey"), 32)
	if !ok {
		return nil, fmt.Errorf("sync: node %s has no X25519 key", node.Id)
	}
	wks, err := kp.ExportKeys(ids, kx)
	if err != nil {
		return nil, err
	}
	for _, k := range wks {
		pk := proto.Key{Collection: k.Collection, Version: k.Version, Retired: k.Retired}
		if !k.Retired {
			pk.Wrapped = b64(k.Wrapped)
		}
		out = append(out, pk)
	}
	return out, nil
}

// effectiveExclude is the `exclude` list of a policy row as nodes see it: the
// configured list plus, for `crypto: strip`, the encrypted fields of the
// collection. That way a node hashes, captures and applies the same field set
// as the hub without knowing anything about encryption.
func (m *Module) effectiveExclude(r *core.Record) []string {
	var ex []string
	if raw := rawJSON(r, "exclude"); raw != nil {
		_ = json.Unmarshal(raw, &ex)
	}
	if r.GetString("crypto") == CryptoStrip {
		if col := m.collectionOf(r.GetString("collection")); col != nil {
			ex = append(ex, kernel.SensitiveFieldsOf(col.Id)...)
			sort.Strings(ex)
			ex = slices.Compact(ex)
		}
	}
	if ex == nil {
		return []string{}
	}
	return ex
}

func (m *Module) collectionOf(ref string) *core.Collection {
	c, err := m.app.FindCachedCollectionByNameOrId(ref)
	if err != nil {
		return nil
	}
	return c
}
