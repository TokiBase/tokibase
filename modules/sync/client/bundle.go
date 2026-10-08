//go:build !no_sync

package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/sync/proto"
)

// BundleBackend is the optional part of Backend that applies a schema bundle:
// it imports the collections, replaces the config rows and returns the
// (collection, field) pairs of computed definitions that changed. The loop calls
// it inside the transaction that also advances the schema version.
type BundleBackend interface {
	ApplyBundle(tx kernel.App, b proto.SchemaBundle) (backfill [][2]string, err error)
	// AfterBundles runs once after the bundles of a handshake were committed
	// (record hashes of changed collections are recomputed here).
	AfterBundles(ctx context.Context) error
}

// jobComputedBackfill is the name of the computed backfill job (modules/computed).
const jobComputedBackfill = "computed.backfill"

// schemaVersion is the schema version this node applied (the cursor).
func (c *Client) schemaVersion() int64 {
	if c.o.App == nil {
		return 0
	}
	cur, err := LoadCursor(c.o.App)
	if err != nil || cur == nil {
		return 0
	}
	return cur.SchemaVersion
}

// applyBundles applies the bundles of a handshake in order, each in one
// transaction (docs/SYNC_DESIGN.md §3.8). Every bundle is a full snapshot, so
// applying one again is harmless; the hash is checked before anything is
// touched (§7.10).
func (c *Client) applyBundles(ctx context.Context, hs *proto.HandshakeResponse) error {
	if c.o.App == nil || len(hs.Schema.Bundles) == 0 {
		return nil
	}
	ba, ok := c.o.Backend.(BundleBackend)
	if !ok {
		return errors.New("sync: this client cannot apply schema bundles (no bundle backend)")
	}
	cur := c.schemaVersion()
	bundles := append([]proto.SchemaBundle(nil), hs.Schema.Bundles...)
	sort.Slice(bundles, func(i, j int) bool { return bundles[i].Version < bundles[j].Version })
	var backfill [][2]string
	for _, b := range bundles {
		if b.Version <= cur {
			continue
		}
		canon, err := proto.CanonicalJSON(b.Bundle)
		if err != nil {
			return fmt.Errorf("sync: schema bundle %d is not valid JSON: %w", b.Version, err)
		}
		if sum := sha256.Sum256(canon); hex.EncodeToString(sum[:]) != b.Hash {
			return fmt.Errorf("sync: schema bundle %d does not match its hash: refused", b.Version)
		}
		err = c.o.App.RunInTransaction(func(tx kernel.App) error {
			bf, err := ba.ApplyBundle(tx, b)
			if err != nil {
				return err
			}
			backfill = append(backfill, bf...)
			db := tx.NonconcurrentDB()
			if _, err := db.NewQuery("UPDATE _sync_cursors SET schema_version={:v} WHERE hub_id={:h}").
				Bind(dbx.Params{"v": b.Version, "h": hs.HubID}).Execute(); err != nil {
				return err
			}
			_, err = db.NewQuery("INSERT INTO _sync_state (key, value) VALUES ('schema_version', {:v}) ON CONFLICT(key) DO UPDATE SET value=excluded.value").
				Bind(dbx.Params{"v": fmt.Sprint(b.Version)}).Execute()
			return err
		})
		if err != nil {
			return fmt.Errorf("sync: applying schema bundle %d: %w", b.Version, err)
		}
		cur = b.Version
		if c.o.Logger != nil {
			c.o.Logger.Info("sync: schema bundle applied", "version", b.Version)
		}
	}
	if err := ba.AfterBundles(ctx); err != nil {
		return fmt.Errorf("sync: after the schema bundles: %w", err)
	}
	// derived values of changed definitions are recomputed locally (never synced)
	seen := map[[2]string]bool{}
	for _, bf := range backfill {
		if bf[0] == "" || seen[bf] {
			continue
		}
		seen[bf] = true
		_, err := kernel.Jobs(c.o.App).Enqueue(ctx, jobComputedBackfill, map[string]string{"collection": bf[0], "field": bf[1]})
		if err != nil && !errors.Is(err, kernel.ErrNoJobQueue) && c.o.Logger != nil {
			c.o.Logger.Warn("sync: failed to queue a computed backfill", "collection", bf[0], "field", bf[1], "error", err)
		}
	}
	return nil
}
