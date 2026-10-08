package kernel

import (
	"errors"
	"sync"
)

// ErrSyncKeyMissing is returned (wrapped) by a write of synced ciphertext whose key
// version this node does not have yet. The hub sends new versions with the next
// handshake, so the sync client drops its session and retries the change.
var ErrSyncKeyMissing = errors.New("sync: the data key of this ciphertext has not arrived yet")

// WrappedKey is one data-encryption-key version of a collection, wrapped for
// one device (docs/SYNC_DESIGN.md §7.6).
type WrappedKey struct {
	// Collection is the collection id.
	Collection string
	// Version is the DEK version (the number in the `tkc1:<version>:` prefix of a ciphertext).
	Version int
	// Wrapped is the DEK sealed to the recipient X25519 key (opaque to the
	// caller). Empty for a retired version.
	Wrapped []byte
	// Retired marks a version the hub destroyed: the node must drop its copy
	// too (when no local row still uses it).
	Retired bool
}

// SyncKeyProvider is implemented by modules/crypto. modules/sync uses it
// (through [SyncKeyProviderOf]) to ship the collection DEKs to a device without
// importing the module. The device never learns the master key of the hub: it
// re-wraps every imported DEK under its own master key.
type SyncKeyProvider interface {
	// ExportKeys (hub) returns the DEK versions of the collections, wrapped to
	// recipientX25519 (the X25519 public key of the node).
	ExportKeys(collectionIds []string, recipientX25519 []byte) ([]WrappedKey, error)
	// ImportKeys (spoke) unwraps keys with the X25519 private key of the node and
	// stores them under the local master key. It fails when the node has no
	// master key.
	ImportKeys(keys []WrappedKey, localX25519Priv []byte) error
	// NeedsKeys reports whether the collection has encrypted fields whose
	// ciphertext this node must be able to read or write.
	NeedsKeys(collectionId string) bool
}

// SyncKeyRefresher is the optional part of a [SyncKeyProvider] that reloads its
// configuration from the database (called after a schema bundle was applied, so
// that [IsSensitive] is current before the record hashes are recomputed).
type SyncKeyRefresher interface {
	RefreshSyncConfig()
}

var syncKeyProviders sync.Map // App -> SyncKeyProvider

// SetSyncKeyProvider registers the provider of app (nil removes it).
func SetSyncKeyProvider(app App, p SyncKeyProvider) {
	if app == nil {
		return
	}
	if p == nil {
		syncKeyProviders.Delete(app)
		return
	}
	syncKeyProviders.Store(app, p)
}

// SyncKeyProviderOf returns the provider of app, or nil when the crypto module
// is off or compiled out.
func SyncKeyProviderOf(app App) SyncKeyProvider {
	if app == nil {
		return nil
	}
	v, _ := syncKeyProviders.Load(app)
	p, _ := v.(SyncKeyProvider)
	return p
}

// ReleaseSyncKeys drops the provider of app (called on terminate).
func ReleaseSyncKeys(app App) {
	if app != nil {
		syncKeyProviders.Delete(app)
	}
}
