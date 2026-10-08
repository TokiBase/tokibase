//go:build !no_crypto

package crypto

import (
	"bytes"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/hook"
	"github.com/tokibase/tokibase/tools/types"
)

// Key sync (docs/SYNC_DESIGN.md §7.6, docs/modules/crypto.md "Sync").
//
// The hub wraps every DEK version of a collection to the X25519 key of a
// device: wrapped = ephPub(32) || nonce || AES-256-GCM(HKDF(X25519(eph, node)),
// dek) with AAD = the DEK AAD of the version. The device re-wraps the DEK under
// its OWN master key into `_crypto_keys`; it never sees the master key of the hub.

// StateStripped is the `state` a schema bundle gives to the `_crypto_fields`
// row of a collection whose sync policy says `crypto: strip`: the node does not
// receive the field, so it is a plain local column there and the module leaves
// it alone (no key needed).
const StateStripped = "stripped"

const (
	syncKeySalt = "tokibase-sync-keys-v1"
	ephLen      = 32
)

// syncKeyAAD binds the transport wrap to collection and version.
func syncKeyAAD(collId string, ver int) []byte {
	return append([]byte("tkc-sync-dek\x00"), dekAAD(collId, ver)...)
}

func syncWrapKey(shared []byte, collId string, ver int) ([]byte, error) {
	return hkdf.Key(sha256.New, shared, []byte(syncKeySalt), fmt.Sprintf("%s\x00%d", collId, ver), 32)
}

type syncProvider struct{ m *Module }

// bindSync registers the key provider and releases it on terminate.
func (m *Module) bindSync() {
	kernel.SetSyncKeyProvider(m.app, syncProvider{m})
	m.app.OnTerminate().Bind(&hook.Handler[*core.TerminateEvent]{
		Id: hookId + "syncrel",
		Func: func(e *core.TerminateEvent) error {
			kernel.ReleaseSyncKeys(m.app)
			return e.Next()
		},
	})
}

// NeedsKeys reports whether the collection has encrypted fields that this node
// reads or writes (stripped fields do not count).
func (p syncProvider) NeedsKeys(collectionId string) bool {
	cfg, err := p.m.fieldsFor(collectionId)
	return err == nil && len(cfg) > 0
}

// RefreshSyncConfig reloads the field configuration (and with it the kernel
// sensitive-field registry) from the database.
func (p syncProvider) RefreshSyncConfig() {
	p.m.Invalidate()
	_, _ = p.m.fieldsFor("")
}

// ExportKeys wraps every DEK version of the collections to recipient. One
// ephemeral key serves the whole call (one handshake).
func (p syncProvider) ExportKeys(collectionIds []string, recipient []byte) ([]kernel.WrappedKey, error) {
	m := p.m
	if !m.Active() {
		return nil, ErrNoMasterKey
	}
	pub, err := ecdh.X25519().NewPublicKey(recipient)
	if err != nil {
		return nil, fmt.Errorf("crypto: invalid recipient key: %w", err)
	}
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	shared, err := eph.ECDH(pub)
	if err != nil {
		return nil, err
	}
	ids := append([]string(nil), collectionIds...)
	sort.Strings(ids)
	var out []kernel.WrappedKey
	for _, id := range ids {
		recs, err := m.keyRecords(id)
		if err != nil {
			return nil, err
		}
		for _, r := range recs {
			ver := r.GetInt("version")
			wk := kernel.WrappedKey{Collection: id, Version: ver}
			if !r.GetDateTime("retired_at").IsZero() || r.GetString("wrapped_dek") == "" {
				wk.Retired = true
				out = append(out, wk)
				continue
			}
			dek, err := m.unwrap(id, ver, r.GetString("wrapped_dek"))
			if err != nil {
				return nil, fmt.Errorf("crypto: cannot unwrap key v%d of %s (wrong master key?): %w", ver, id, err)
			}
			k, err := syncWrapKey(shared, id, ver)
			if err != nil {
				return nil, err
			}
			b64, err := sealRaw(k, syncKeyAAD(id, ver), dek)
			if err != nil {
				return nil, err
			}
			raw, err := b64Decode(b64)
			if err != nil {
				return nil, err
			}
			wk.Wrapped = append(append([]byte{}, eph.PublicKey().Bytes()...), raw...)
			out = append(out, wk)
		}
	}
	return out, nil
}

// ImportKeys unwraps keys with the X25519 private key of the node and stores
// them wrapped under the local master key. A version that already exists must
// hold the same DEK (a node that created its own key for a synced collection
// cannot join it). A retired version is dropped locally unless a local row
// still holds a ciphertext of it (the hub rotation does not emit sync changes,
// so such a row keeps its old ciphertext until it is written again).
func (p syncProvider) ImportKeys(keys []kernel.WrappedKey, localPriv []byte) error {
	m := p.m
	if len(keys) == 0 {
		return nil
	}
	if !m.Active() {
		return ErrNoMasterKey
	}
	priv, err := ecdh.X25519().NewPrivateKey(localPriv)
	if err != nil {
		return fmt.Errorf("crypto: invalid node key: %w", err)
	}
	coll, err := m.app.FindCollectionByNameOrId(KeysCollection)
	if err != nil {
		return err
	}
	defer m.Invalidate()
	for _, wk := range keys {
		if wk.Collection == "" || wk.Version <= 0 {
			return errors.New("crypto: invalid key in the sync answer")
		}
		existing, _ := m.app.FindFirstRecordByFilter(KeysCollection, "collection={:c} && version={:v}",
			dbx.Params{"c": wk.Collection, "v": wk.Version})
		if wk.Retired {
			if existing == nil {
				r := core.NewRecord(coll)
				r.Set("collection", wk.Collection)
				r.Set("version", wk.Version)
				r.Set("wrapped_dek", "")
				dt, _ := types.ParseDateTime(timeNow())
				r.Set("retired_at", dt)
				if err := m.app.Save(r); err != nil {
					return err
				}
				continue
			}
			if existing.GetString("wrapped_dek") == "" {
				continue
			}
			if m.versionInUse(wk.Collection, wk.Version) {
				m.app.Logger().Warn("crypto: the hub retired a key version that local rows still use; keeping it until they are rewritten",
					"collection", wk.Collection, "version", wk.Version)
				continue
			}
			if err := m.retireKeys(wk.Collection, []int{wk.Version}); err != nil {
				return err
			}
			continue
		}
		if len(wk.Wrapped) <= ephLen {
			return fmt.Errorf("crypto: key v%d of %s is truncated", wk.Version, wk.Collection)
		}
		ephPub, err := ecdh.X25519().NewPublicKey(wk.Wrapped[:ephLen])
		if err != nil {
			return fmt.Errorf("crypto: key v%d of %s: %w", wk.Version, wk.Collection, err)
		}
		shared, err := priv.ECDH(ephPub)
		if err != nil {
			return err
		}
		k, err := syncWrapKey(shared, wk.Collection, wk.Version)
		if err != nil {
			return err
		}
		dek, err := openRaw(k, syncKeyAAD(wk.Collection, wk.Version), b64Encode(wk.Wrapped[ephLen:]))
		if err != nil {
			return fmt.Errorf("crypto: cannot unwrap key v%d of %s (not wrapped for this node?): %w", wk.Version, wk.Collection, err)
		}
		if len(dek) != 32 {
			return fmt.Errorf("crypto: key v%d of %s has a wrong length", wk.Version, wk.Collection)
		}
		if existing != nil {
			if existing.GetString("wrapped_dek") == "" {
				continue // retired here: stays retired
			}
			cur, err := m.unwrap(wk.Collection, wk.Version, existing.GetString("wrapped_dek"))
			if err != nil {
				return fmt.Errorf("crypto: cannot unwrap the local key v%d of %s (wrong master key?): %w", wk.Version, wk.Collection, err)
			}
			if !bytes.Equal(cur, dek) {
				return fmt.Errorf("crypto: this node already has a different key v%d for collection %s; it cannot join the hub's encrypted collection", wk.Version, wk.Collection)
			}
			continue
		}
		w, err := m.wrap(wk.Collection, wk.Version, dek)
		if err != nil {
			return err
		}
		r := core.NewRecord(coll)
		r.Set("collection", wk.Collection)
		r.Set("version", wk.Version)
		r.Set("wrapped_dek", w)
		if err := m.app.Save(r); err != nil {
			return fmt.Errorf("crypto: cannot store key v%d of %s: %w", wk.Version, wk.Collection, err)
		}
	}
	return nil
}

// versionInUse reports whether any text-like column of the collection still
// holds a ciphertext of the version.
func (m *Module) versionInUse(collId string, ver int) bool {
	col, err := m.app.FindCachedCollectionByNameOrId(collId)
	if err != nil || col == nil {
		return false
	}
	for _, f := range col.Fields {
		if !allowedTypes[f.Type()] {
			continue
		}
		if n, err := m.countCiphertext(col, f.GetName(), ver); err != nil || n > 0 {
			return true
		}
	}
	return false
}
