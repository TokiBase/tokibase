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
//
// The decision comes from the POLICY (`strip_fields`, persisted at save time and
// kept sticky, see stripFieldsFor), never from the live crypto registry alone:
// `toki crypto disable`, a hub whose crypto module is off or compiled out, or a
// registry that is not filled yet at boot must not turn a withheld field into a
// synced one (fail closed). The registry can only WIDEN the set.
func stripped(col *core.Collection, p *policy, name string) bool {
	if p == nil || p.Crypto != CryptoStrip {
		return false
	}
	if _, ok := p.StripFields[name]; ok {
		return true
	}
	return col != nil && kernel.IsSensitive(col.Id, name)
}

// CryptoStrip is the `crypto` policy value that withholds encrypted fields.
const CryptoStrip = "strip"

// keyCollections lists the ids of the collections a node needs keys for: an
// enabled policy that syncs the collection in a direction the node uses,
// `crypto` other than strip, encrypted fields configured, and a way for the
// node to use them:
//
//   - a push-only node still encrypts its own writes, so it gets the keys;
//   - a collection that the node pulls but never receives a row of (a null
//     view rule without `trusted` while pull_view_rule is on, or a partition the
//     node has no value for) gets none.
//
// The data key is per collection, not per partition: a partition-restricted
// node that pulls its own tenant still holds the key that opens the other
// tenants' ciphertext should it ever reach the device (docs/modules/sync.md).
func (m *Module) keyCollections(kp kernel.SyncKeyProvider, node *core.Record) ([]string, error) {
	rows, err := m.pol.load()
	if err != nil {
		return nil, err
	}
	var params map[string]any
	if node != nil {
		_ = node.UnmarshalJSONField("params", &params)
	}
	seen := map[string]struct{}{}
	for k, p := range rows {
		if p.ColID == "" || k != p.ColID || p.Direction == DirNone || p.Crypto == CryptoStrip {
			continue
		}
		if !kp.NeedsKeys(p.ColID) {
			continue
		}
		if p.Direction == DirPull && !m.nodeCanPull(p, node, params) {
			continue
		}
		seen[p.ColID] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

// nodeCanPull reports whether a node with these params can ever receive a row
// of the collection of p.
func (m *Module) nodeCanPull(p *policy, node *core.Record, params map[string]any) bool {
	if p.PartField != "" {
		if v, ok := params[p.PartParam]; !ok || partString(v) == "" {
			return false
		}
	}
	ruleOn := p.PullViewRule || (envFlag(EnvPullViewRule) && !p.SkipViewRule && node != nil && node.GetString("actor_record") != "")
	if ruleOn && !p.Trusted {
		if col := m.collectionOf(p.ColID); col != nil && col.ViewRule == nil {
			return false // view rule null: superusers only, never pulled unless trusted (§7.7)
		}
	}
	return true
}

// handshakeKeys builds the `keys` of a handshake answer: every data key version
// (retired ones as such) of the collections the node may read, wrapped to its
// X25519 key. A new version after a rotation shows in the next handshake.
//
// A collection whose keys cannot be exported does not fail the handshake: it
// is left out and reported in the second result (`errors` of the answer), so
// the node still syncs every other collection. The spoke then holds back only
// the affected collection (ErrSyncKeyMissing handling, client/keymissing.go).
func (m *Module) handshakeKeys(node *core.Record) ([]proto.Key, []proto.KeyError, map[string]int, error) {
	out := []proto.Key{}
	kp := kernel.SyncKeyProviderOf(m.app)
	if kp == nil {
		return out, nil, nil, nil
	}
	ids, err := m.keyCollections(kp, node)
	if err != nil || len(ids) == 0 {
		return out, nil, nil, err
	}
	// nothing below fails the handshake: a hub without master key, a node with an
	// unusable key or a key version that does not unwrap costs only the encrypted
	// collections, the node keeps syncing the others
	allFailed := func(why error) ([]proto.Key, []proto.KeyError, map[string]int, error) {
		m.app.Logger().Error("sync: cannot export the encryption keys for a handshake, the encrypted collections are left out",
			"node", node.Id, "collections", ids, "error", why)
		kerrs := make([]proto.KeyError, 0, len(ids))
		for _, id := range ids {
			kerrs = append(kerrs, proto.KeyError{Collection: id, Code: proto.CodeKeyExport})
		}
		return out, kerrs, nil, nil
	}
	kx, ok := decodeKey(node.GetString("kx_pubkey"), 32)
	if !ok {
		return allFailed(fmt.Errorf("node %s has no usable X25519 key", node.Id))
	}
	wks, failed, err := kp.ExportKeys(ids, kx)
	if err != nil {
		return allFailed(err)
	}
	have := map[string]int{}
	for _, k := range wks {
		pk := proto.Key{Collection: k.Collection, Version: k.Version, Retired: k.Retired}
		if !k.Retired {
			pk.Wrapped = b64(k.Wrapped)
			have[k.Collection] = max(have[k.Collection], k.Version)
		}
		out = append(out, pk)
	}
	var kerrs []proto.KeyError
	for _, f := range failed {
		kerrs = append(kerrs, proto.KeyError{Collection: f.Collection, Code: proto.CodeKeyExport})
		m.app.Logger().Error("sync: cannot export the encryption keys of a collection, it is left out of the handshake",
			"node", node.Id, "collection", f.Collection, "error", f.Err)
	}
	return out, kerrs, have, nil
}

// stripFieldsOfRow parses the persisted `strip_fields` of a policy row.
func stripFieldsOfRow(r *core.Record) []string {
	var out []string
	if raw := rawJSON(r, "strip_fields"); raw != nil {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}

// effectiveExclude is the `exclude` list of a policy row as nodes see it: the
// configured list plus, for `crypto: strip`, the withheld encrypted fields: the
// persisted `strip_fields` and whatever the live registry adds. That way a node
// hashes, captures and applies the same field set as the hub without knowing
// anything about encryption.
func (m *Module) effectiveExclude(r *core.Record) []string {
	var ex []string
	if raw := rawJSON(r, "exclude"); raw != nil {
		_ = json.Unmarshal(raw, &ex)
	}
	if r.GetString("crypto") == CryptoStrip {
		ex = append(ex, stripFieldsOfRow(r)...)
		if col := m.collectionOf(r.GetString("collection")); col != nil {
			ex = append(ex, kernel.SensitiveFieldsOf(col.Id)...)
		}
		sort.Strings(ex)
		ex = slices.Compact(ex)
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
