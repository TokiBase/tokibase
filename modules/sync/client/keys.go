//go:build !no_sync

package client

import (
	"encoding/base64"
	"fmt"
	"sort"

	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/sync/proto"
)

// importKeys stores the collection data keys of a handshake answer under the
// local master key (docs/SYNC_DESIGN.md §7.6). It runs before the schema
// bundles and before anything is pulled, so the ciphertext that follows can be
// read. A node that cannot hold the keys (no crypto module, no master key)
// refuses to sync a hub that has encrypted collections for it, instead of
// storing ciphertext it can neither read nor validate.
func (c *Client) importKeys(hs *proto.HandshakeResponse) error {
	if c.o.App == nil {
		return nil
	}
	// collections whose keys the hub could not export: they are held back after a few
	// handshakes (keymissing.go), the others sync as usual
	db := c.o.App.NonconcurrentDB()
	for _, ke := range hs.KeyErrors {
		if c.o.Logger != nil {
			c.o.Logger.Warn("sync: the hub could not export the encryption keys of a collection", "collection", ke.Collection, "code", ke.Code)
		}
		c.noteKeyMissing(db, ke.Collection)
	}
	defer c.resolveKeyMissing(hs)
	if len(hs.Keys) == 0 {
		return nil
	}
	cols := map[string]struct{}{}
	keys := make([]kernel.WrappedKey, 0, len(hs.Keys))
	for _, k := range hs.Keys {
		cols[k.Collection] = struct{}{}
		wk := kernel.WrappedKey{Collection: k.Collection, Version: k.Version, Retired: k.Retired}
		if !k.Retired {
			b, err := base64.StdEncoding.DecodeString(k.Wrapped)
			if err != nil {
				return fmt.Errorf("sync: invalid encryption key v%d of %s in the handshake: %w", k.Version, k.Collection, err)
			}
			wk.Wrapped = b
		}
		keys = append(keys, wk)
	}
	names := make([]string, 0, len(cols))
	for id := range cols {
		names = append(names, id)
	}
	sort.Strings(names)
	kp := kernel.SyncKeyProviderOf(c.o.App)
	if kp == nil {
		return fmt.Errorf("sync: the hub has encrypted collections for this node %v, but this build has no crypto module: refusing to sync", names)
	}
	if err := kp.ImportKeys(keys, c.o.Identity.X.Bytes()); err != nil {
		return fmt.Errorf("sync: this node cannot import the encryption keys of the collections %v (set TOKI_CRYPTO_MASTER_KEY or TOKI_CRYPTO_MASTER_KEY_FILE, a key of its own: it never sees the hub's): refusing to sync: %w", names, err)
	}
	return nil
}
