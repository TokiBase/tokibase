package kernel

import (
	"errors"
	"sync"
)

// ErrSyncKeyMissing is returned (wrapped) by a write of synced ciphertext whose key
// version this node does not have yet. The hub sends new versions with the next
// handshake, so the sync client drops its session and retries the change.
var ErrSyncKeyMissing = errors.New("sync: the data key of this ciphertext has not arrived yet")

// ErrSyncKeyRetired is returned (wrapped) by the hub for pushed ciphertext whose
// key version was retired: the hub can no longer read it. The message starts
// with the sync code so that the push result carries it (`crypto_version_retired`).
var ErrSyncKeyRetired = errors.New("crypto_version_retired: the data key of this ciphertext was retired on the hub")

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
	// recipientX25519 (the X25519 public key of the node). A collection whose
	// keys cannot be exported (one version does not unwrap, for example) is
	// reported in failed and left out of keys: the others are still answered.
	// err is set when nothing can be exported (no master key, unusable
	// recipient key).
	ExportKeys(collectionIds []string, recipientX25519 []byte) (keys []WrappedKey, failed []KeyFailure, err error)
	// ImportKeys (spoke) unwraps keys with the X25519 private key of the node and
	// stores them under the local master key. It fails when the node has no
	// master key.
	ImportKeys(keys []WrappedKey, localX25519Priv []byte) error
	// NeedsKeys reports whether the collection has encrypted fields whose
	// ciphertext this node must be able to read or write.
	NeedsKeys(collectionId string) bool
}

// KeyFailure is a collection whose data keys could not be exported.
type KeyFailure struct {
	Collection string
	Err        error
}

// SyncSweeper is implemented by modules/sync. modules/crypto calls it to keep
// the change log of a synced collection in step with the bulk rewrites of
// `toki crypto enable|disable|rotate` (docs/SYNC_DESIGN.md §7.6), and to ask
// whether a key version may be retired.
type SyncSweeper interface {
	// SyncRole is "hub" or "spoke".
	SyncRole() string
	// IsSynced reports whether the collection has an enabled sync policy.
	IsSynced(collectionId string) bool
	// RecordSweep runs inside the transaction of a sweep batch, after the
	// stored values were rewritten: changed maps a record id to the fields that
	// were rewritten. The hub writes one `u` change per record (current stored
	// values, new HLC) so that every node converges on the new ciphertext.
	RecordSweep(tx App, collectionId string, changed map[string][]string) error
	// RetireBlockers lists the active nodes that may still hold or write
	// ciphertext of one of the versions ("" entries are never returned).
	RetireBlockers(collectionId string, versions []int) ([]string, error)
}

var syncSweepers sync.Map // App -> SyncSweeper

// SetSyncSweeper registers the sweeper of app (nil removes it).
func SetSyncSweeper(app App, s SyncSweeper) {
	if app == nil {
		return
	}
	if s == nil {
		syncSweepers.Delete(app)
		return
	}
	syncSweepers.Store(app, s)
}

// SyncSweeperOf returns the sweeper of app, or nil when sync is off or compiled out.
func SyncSweeperOf(app App) SyncSweeper {
	if app == nil {
		return nil
	}
	v, _ := syncSweepers.Load(app)
	s, _ := v.(SyncSweeper)
	return s
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
