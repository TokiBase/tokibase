package embed

import "errors"

// ErrSyncUnavailable is returned by Instance.Sync() methods in a build with the
// no_sync tag.
var ErrSyncUnavailable = errors.New("embed: sync is not available in this build (no_sync)")

// SyncOptions turns the instance into a sync spoke (docs/SYNC_DESIGN.md §6.3).
// With Profile nano or edge it defaults TOKI_SYNC_ROLE=spoke. Entries in
// Options.Env win over everything set here.
type SyncOptions struct {
	// HubURL is the hub base URL (TOKI_SYNC_HUB_URL); Sync().Enroll uses it when
	// its own hubURL argument is empty. Plain http needs TOKI_SYNC_INSECURE=1 in
	// Options.Env and is accepted only for loopback and private hosts.
	HubURL string
	// Interval is the idle sync interval (TOKI_SYNC_INTERVAL, default 30 s; the
	// loop uses 5 min on a metered or low power device).
	Interval string
	// NodeKey is the encoded node identity (TOKI_SYNC_NODE_KEY). Leave it empty
	// to let the instance create and keep its own key file in the data dir. Set
	// it when the host keeps the key in the platform keystore. The sync module reads
	// the key from the process environment (TOKI_SYNC_NODE_KEY), so while the
	// instance runs it is visible to child processes of the host; an instance
	// without NodeKey never inherits the key of another one.
	//
	// Precedence: the fields of SyncOptions win over Options.Env, which wins over
	// the profile defaults, which win over the process environment. An Env entry for
	// TOKI_SYNC_ROLE, _HUB_URL, _INTERVAL or _NODE_KEY that contradicts a set
	// SyncOptions field makes Start fail.
	NodeKey []byte
}
