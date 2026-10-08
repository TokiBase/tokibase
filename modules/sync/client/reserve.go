//go:build !no_sync

package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	stdsync "sync"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/modules/sync/proto"
)

// Reservation ranges on the spoke (docs/SYNC_DESIGN.md §3.10): the handshake
// lists the active ranges of the hub, the loop keeps `_sync_reserved` equal to
// them and asks for a new range when the remaining values drop below 20 %.

// reserveState is the part of the Client that belongs to reservations.
type reserveState struct {
	mu   stdsync.Mutex
	want map[string]bool
}

// WantReserve asks the loop to get a new range of the sequence as soon as it
// can (prefetch, or after the ranges ran dry). It never blocks.
func (c *Client) WantReserve(sequence string) {
	c.rsv.mu.Lock()
	if c.rsv.want == nil {
		c.rsv.want = map[string]bool{}
	}
	c.rsv.want[sequence] = true
	c.rsv.mu.Unlock()
	c.Kick()
}

func (c *Client) takeWanted() []string {
	c.rsv.mu.Lock()
	defer c.rsv.mu.Unlock()
	var out []string
	for s := range c.rsv.want {
		out = append(out, s)
	}
	c.rsv.want = nil
	sort.Strings(out)
	return out
}

// reserveSequences lists the sequences named by the local policies
// (`field_types` entries `reserve:<sequence>`).
func (c *Client) reserveSequences() []string {
	if c.o.App == nil || !c.o.App.HasTable("_sync_policies") {
		return nil
	}
	recs, err := c.o.App.FindAllRecords("_sync_policies")
	if err != nil {
		return nil
	}
	set := map[string]bool{}
	for _, r := range recs {
		if !r.GetBool("enabled") {
			continue
		}
		raw, _ := json.Marshal(r.Get("field_types"))
		var ft map[string]string
		if json.Unmarshal(raw, &ft) != nil {
			continue
		}
		for _, t := range ft {
			if s, ok := strings.CutPrefix(t, "reserve:"); ok && s != "" {
				set[s] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// reconcileReservations makes the local ranges equal to the active ranges the
// hub listed: new ones are added (next = start), a range the hub no longer
// lists was retired there (revoke, release) and stops being used. `next` of a
// known range is never moved.
func (c *Client) reconcileReservations(list []proto.Reservation) error {
	if c.o.App == nil {
		return nil
	}
	db := c.o.App.NonconcurrentDB()
	listed := map[string]bool{}
	for _, r := range list {
		if r.ID == "" {
			continue
		}
		listed[r.ID] = true
		if _, err := db.NewQuery(`INSERT OR IGNORE INTO _sync_reserved (id, sequence, start, "end", next, status, expires)
VALUES ({:id}, {:s}, {:a}, {:b}, {:a}, 'active', {:x})`).
			Bind(dbx.Params{"id": r.ID, "s": r.Sequence, "a": r.Start, "b": r.End, "x": r.Expires}).Execute(); err != nil {
			return err
		}
		// a spoke restored from an older backup must not reissue numbers the hub saw
		if _, err := db.NewQuery(`UPDATE _sync_reserved SET next={:n} WHERE id={:id} AND status='active' AND next<{:n}`).
			Bind(dbx.Params{"id": r.ID, "n": r.End - r.RemainingHint + 1}).Execute(); err != nil {
			return err
		}
	}
	var active []string
	if err := db.NewQuery("SELECT id FROM _sync_reserved WHERE status='active'").Column(&active); err != nil {
		return err
	}
	for _, id := range active {
		if !listed[id] {
			if _, err := db.NewQuery("UPDATE _sync_reserved SET status='retired' WHERE id={:id} AND status='active'").Bind(dbx.Params{"id": id}).Execute(); err != nil {
				return err
			}
		}
	}
	return nil
}

// NeedsRange reports whether the active ranges of a sequence are missing or below
// the prefetch share (20 %).
func (c *Client) NeedsRange(seq string) bool { return c.needsRange(seq) }

// needsRange reports whether the active ranges of a sequence are missing or
// below the prefetch share.
func (c *Client) needsRange(seq string) bool {
	var remaining, capacity int64
	if err := c.o.App.DB().NewQuery(`SELECT COALESCE(SUM("end"-next+1),0), COALESCE(MAX("end"-start+1),0) FROM _sync_reserved WHERE sequence={:s} AND status='active'`).
		Bind(dbx.Params{"s": seq}).Row(&remaining, &capacity); err != nil {
		return false
	}
	return capacity == 0 || remaining*5 < capacity
}

// usedRanges reports the highest value taken from each range of the sequence.
func (c *Client) usedRanges(seq string) map[string]int64 {
	var rows []struct {
		ID    string `db:"id"`
		Start int64  `db:"start"`
		Next  int64  `db:"next"`
	}
	if err := c.o.App.DB().NewQuery("SELECT id, start, next FROM _sync_reserved WHERE sequence={:s} AND next>start").Bind(dbx.Params{"s": seq}).All(&rows); err != nil {
		return nil
	}
	out := make(map[string]int64, len(rows))
	for _, r := range rows {
		out[r.ID] = r.Next - 1
	}
	return out
}

// Reserve asks the hub for a range of the sequence (count 0 = the block size
// of the sequence) and stores it locally.
func (c *Client) Reserve(ctx context.Context, sequence string, count int64) error {
	body, _ := json.Marshal(proto.ReserveRequest{Sequence: sequence, Count: count, Used: c.usedRanges(sequence)})
	b, err := c.authed(ctx, http.MethodPost, proto.PathReserve, nil, body)
	if err != nil {
		return err
	}
	var resp proto.ReserveResponse
	if err := json.Unmarshal(b, &resp); err != nil {
		return err
	}
	list := make([]proto.Reservation, 0, len(resp.Ranges))
	for _, r := range resp.Ranges {
		list = append(list, proto.Reservation{ID: r.ID, Sequence: sequence, Start: r.Start, End: r.End, Expires: r.Expires})
	}
	db := c.o.App.NonconcurrentDB()
	for _, r := range list {
		if _, err := db.NewQuery(`INSERT OR IGNORE INTO _sync_reserved (id, sequence, start, "end", next, status, expires)
VALUES ({:id}, {:s}, {:a}, {:b}, {:a}, 'active', {:x})`).
			Bind(dbx.Params{"id": r.ID, "s": r.Sequence, "a": r.Start, "b": r.End, "x": r.Expires}).Execute(); err != nil {
			return err
		}
	}
	return nil
}

// ReleaseRange gives an unused range back (design §3.10): the hub retires it,
// values are never reissued. The values taken so far stay valid.
func (c *Client) ReleaseRange(ctx context.Context, id string) error {
	var row struct {
		Start int64 `db:"start"`
		Next  int64 `db:"next"`
	}
	if err := c.o.App.DB().NewQuery("SELECT start, next FROM _sync_reserved WHERE id={:id}").Bind(dbx.Params{"id": id}).One(&row); err != nil {
		return err
	}
	used := int64(0)
	if row.Next > row.Start {
		used = row.Next - 1
	}
	body, _ := json.Marshal(proto.ReleaseRequest{ID: id, Used: used})
	if _, err := c.authed(ctx, http.MethodPost, proto.PathReserveRelease, nil, body); err != nil {
		return err
	}
	_, err := c.o.App.NonconcurrentDB().NewQuery("UPDATE _sync_reserved SET status='retired' WHERE id={:id}").Bind(dbx.Params{"id": id}).Execute()
	return err
}

// reservePass tops up the ranges of every sequence that is wanted or low. A
// refusal (limit, unknown sequence) is not an error of the cycle: the values
// that are left keep being used, and creates fail closed when they run out.
func (c *Client) reservePass(ctx context.Context) {
	if c.o.App == nil || !c.o.App.HasTable("_sync_reserved") {
		return
	}
	seqs := c.reserveSequences()
	set := map[string]bool{}
	for _, s := range seqs {
		set[s] = true
	}
	for _, s := range c.takeWanted() {
		set[s] = true
	}
	names := make([]string, 0, len(set))
	for s := range set {
		names = append(names, s)
	}
	sort.Strings(names)
	for _, s := range names {
		if ctx.Err() != nil {
			return
		}
		if !c.needsRange(s) {
			continue
		}
		if err := c.Reserve(ctx, s, 0); err != nil {
			var he *Error
			if errors.As(err, &he) && c.o.Logger != nil {
				c.o.Logger.Warn("sync: reservation refused", "sequence", s, "code", he.Code, "message", he.Message)
			} else if c.o.Logger != nil {
				c.o.Logger.Warn("sync: reservation failed", "sequence", s, "error", err)
			}
		}
	}
}
