//go:build !no_sync

package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/modules/sync/proto"
)

// Pull fetches one page after the given hub seq (wait is the long-poll in
// seconds, 0 for none). It does not apply anything.
func (c *Client) Pull(ctx context.Context, after int64, limit, wait int) (*proto.PullResponse, error) {
	path := proto.PathPull + "?after=" + strconv.FormatInt(after, 10) + "&limit=" + strconv.Itoa(limit)
	if wait > 0 {
		path += "&wait=" + strconv.Itoa(wait)
	}
	b, err := c.authed(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return nil, err
	}
	var pr proto.PullResponse
	if err := json.Unmarshal(b, &pr); err != nil {
		return nil, err
	}
	return &pr, nil
}

// Ack tells the hub how far this node has pulled and optionally sends the
// per-collection digests (docs/SYNC_DESIGN.md §3.6).
func (c *Client) Ack(ctx context.Context, through int64, digest map[string]string) (*proto.AckResponse, error) {
	body, _ := json.Marshal(proto.AckRequest{PulledThrough: through, Digest: digest})
	b, err := c.authed(ctx, http.MethodPost, proto.PathAck, nil, body)
	if err != nil {
		return nil, err
	}
	var ar proto.AckResponse
	if err := json.Unmarshal(b, &ar); err != nil {
		return nil, err
	}
	return &ar, nil
}

// pullAll pulls and applies pages until the hub has nothing more, then acks.
func (c *Client) pullAll(ctx context.Context, res *Result) error {
	if c.o.App == nil || c.o.Backend == nil {
		return nil
	}
	pulled := false
	var through int64
	for {
		cur, err := LoadCursor(c.o.App)
		if err != nil || cur == nil {
			return errors.New("sync: the node is not enrolled")
		}
		pr, err := c.Pull(ctx, cur.PullAfter, c.pullLimit(), 0)
		if err != nil && IsCode(err, proto.CodeResponseTooLarge) && c.pullLimit() > 1 {
			half := max(c.pullLimit()/2, 1)
			c.loop.mu.Lock()
			c.loop.page = half
			c.loop.mu.Unlock()
			continue
		}
		if err != nil {
			if IsCode(err, proto.CodeRebootstrap) {
				c.markRebootstrap(0)
				return ErrRebootstrap
			}
			return err
		}
		applied, errs, err := c.applyPage(cur.HubID, pr)
		var ae *ApplyError
		if err != nil && !errors.As(err, &ae) {
			return err
		}
		res.Pulled += len(pr.Changes)
		res.Applied += applied
		if errs > 0 {
			c.loop.mu.Lock()
			c.loop.applyErr += int64(errs)
			c.loop.mu.Unlock()
		}
		if ae != nil {
			return ae // the cursor stopped before the failing change
		}
		through = max(through, pr.Next)
		pulled = pulled || len(pr.Changes) > 0 || pr.Next > cur.PullAfter
		if !pr.More {
			break
		}
	}
	if pulled {
		if _, err := c.Ack(ctx, through, nil); err != nil {
			return err
		}
	}
	return nil
}

// setPullAfter moves the cursor (in the caller's transaction).
func setPullAfter(db dbx.Builder, hubID string, n int64) error {
	_, err := db.NewQuery("UPDATE _sync_cursors SET pull_after={:n} WHERE hub_id={:h} AND pull_after<{:n}").
		Bind(dbx.Params{"n": n, "h": hubID}).Execute()
	return err
}

// markRebootstrap records that the hub no longer holds the changes this node is
// missing (compaction, a stale node): the state is `rebootstrap_required`, shown
// by Status() and `_sync_cursors.state`, and the loop stops until the snapshot
// bootstrap (PR7) or an operator takes over.
func (c *Client) markRebootstrap(lowWater int64) {
	c.setState("rebootstrap_required")
	if c.o.App != nil {
		msg := "the hub requires a re-bootstrap: this node is behind the retained changes (low_water " + strconv.FormatInt(lowWater, 10) + ")"
		_, _ = c.o.App.NonconcurrentDB().NewQuery("UPDATE _sync_cursors SET state='rebootstrap_required', last_error={:e}").
			Bind(dbx.Params{"e": msg}).Execute()
	}
	if c.o.Logger != nil {
		c.o.Logger.Warn("sync: the hub requires a re-bootstrap; the client loop stops (snapshot bootstrap is not available yet)", "low_water", lowWater)
	}
	c.emit(Event{Type: EventRebootstrap})
}
