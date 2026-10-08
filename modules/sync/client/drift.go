//go:build !no_sync

package client

import (
	"context"
	"fmt"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/sync/hlc"
	"github.com/tokibase/tokibase/modules/sync/proto"
)

// DefaultMaxDrift is the tolerance used when the hub does not state one.
const DefaultMaxDrift = 5 * time.Minute

// fixClock handles the clock verdict of a handshake (docs/SYNC_DESIGN.md §3.7).
// The offset was set by the handshake itself. Pending changes whose HLC is
// more than the tolerance ahead of the corrected clock are re-stamped (in
// origin order), whatever the verdict says: a device that was corrected by the
// signed 401 path never sees clock.ok=false but still holds the old stamps.
// When the hub said clock.ok=false the node handshakes once more with the
// corrected time. Changes stamped in the past are kept: offline edits are
// legitimately old.
func (c *Client) fixClock(ctx context.Context, hs *proto.HandshakeResponse) (*proto.HandshakeResponse, error) {
	maxd := time.Duration(hs.Clock.MaxDriftMs) * time.Millisecond
	if maxd <= 0 {
		maxd = DefaultMaxDrift
	}
	n, err := c.restampFuture(maxd, hs.PushFrom)
	if err != nil {
		return nil, err
	}
	if n > 0 && c.o.Logger != nil {
		c.o.Logger.Warn("sync: the clock of this device was ahead of the hub: pending changes were re-stamped", "changes", n, "offset", c.Offset())
	}
	if hs.Clock.Ok {
		return hs, nil
	}
	again, err := c.Handshake(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := c.restampFuture(maxd, again.PushFrom); err != nil {
		return nil, err
	}
	return again, nil
}

// restampFuture gives the pending local changes that are ahead of the corrected
// clock by more than maxDrift a fresh HLC from the corrected clock, in origin
// order. It also moves the record clocks (`_sync_meta`) and the `base_hlc` of
// later changes that referred to the old values, and lowers the clock and its
// persisted floor. It returns the number of re-stamped changes.
func (c *Client) restampFuture(maxDrift time.Duration, pushFrom int64) (int, error) {
	if c.o.App == nil || c.o.Clock == nil {
		return 0, nil
	}
	bound := int64(hlc.Make(c.wallNow().Add(maxDrift).UnixMilli(), 0xffff))
	var ahead int
	if err := c.o.App.DB().NewQuery("SELECT COUNT(*) FROM _changes WHERE node={:n} AND status IN ('local','pushed') AND origin_seq>={:f} AND hlc>{:b}").
		Bind(dbx.Params{"n": c.nodeID, "f": pushFrom, "b": bound}).Row(&ahead); err != nil || ahead == 0 {
		return 0, err
	}
	clock := c.o.Clock
	prev := clock.Last()
	restamped := 0
	err := c.o.App.RunInTransaction(func(tx kernel.App) error {
		restamped = 0
		db := tx.NonconcurrentDB()
		// the first statement is a write: it takes the write lock, so no capture
		// issues a stamp from the old clock while the rows are rewritten
		if _, err := db.NewQuery("UPDATE _sync_state SET value=value WHERE key='node_id'").Execute(); err != nil {
			return err
		}
		var rows []struct {
			Seq int64 `db:"origin_seq"`
			HLC int64 `db:"hlc"`
		}
		if err := db.NewQuery("SELECT origin_seq, hlc FROM _changes WHERE node={:n} AND status IN ('local','pushed') AND origin_seq>={:f} ORDER BY origin_seq").
			Bind(dbx.Params{"n": c.nodeID, "f": pushFrom}).All(&rows); err != nil {
			return err
		}
		first := -1
		for i, r := range rows {
			if r.HLC > bound {
				first = i
				break
			}
		}
		if first < 0 {
			return nil
		}
		firstSeq := rows[first].Seq
		// highest stamp that is still valid and stays
		var keepChanges, keepMeta int64
		if err := db.NewQuery("SELECT COALESCE(MAX(hlc),0) FROM _changes WHERE hlc<={:b} AND NOT (node={:n} AND origin_seq>={:f} AND status IN ('local','pushed'))").
			Bind(dbx.Params{"b": bound, "n": c.nodeID, "f": firstSeq}).Row(&keepChanges); err != nil {
			return err
		}
		if err := db.NewQuery("SELECT COALESCE(MAX(hlc),0) FROM _sync_meta WHERE hlc<={:b}").Bind(dbx.Params{"b": bound}).Row(&keepMeta); err != nil {
			return err
		}
		clock.ResetTo(hlc.HLC(max(keepChanges, keepMeta)))
		for _, r := range rows[first:] {
			nh := int64(clock.Now())
			if _, err := db.NewQuery("UPDATE _changes SET hlc={:h} WHERE node={:n} AND origin_seq={:s}").
				Bind(dbx.Params{"h": nh, "n": c.nodeID, "s": r.Seq}).Execute(); err != nil {
				return err
			}
			// later changes of the same record refer to the old stamp as their base
			if _, err := db.NewQuery("UPDATE _changes SET base_hlc={:h} WHERE node={:n} AND origin_seq>{:s} AND status IN ('local','pushed') AND base_hlc={:o}").
				Bind(dbx.Params{"h": nh, "n": c.nodeID, "s": r.Seq, "o": r.HLC}).Execute(); err != nil {
				return err
			}
			if _, err := db.NewQuery("UPDATE _sync_meta SET hlc={:h} WHERE node={:n} AND hlc={:o}").
				Bind(dbx.Params{"h": nh, "n": c.nodeID, "o": r.HLC}).Execute(); err != nil {
				return err
			}
			restamped++
		}
		// the persisted floor may hold the old stamp: a restart would resume from it
		_, err := db.NewQuery("UPDATE _sync_state SET value={:v} WHERE key={:k}").
			Bind(dbx.Params{"v": clock.Last().String(), "k": hlc.FloorKey}).Execute()
		return err
	})
	if err != nil {
		clock.Observe(prev) // the rows were not rewritten: never go below what was issued
		return 0, fmt.Errorf("sync: re-stamping the pending changes: %w", err)
	}
	return restamped, nil
}
