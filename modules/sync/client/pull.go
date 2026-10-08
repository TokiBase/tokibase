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
		if err != nil {
			if IsCode(err, proto.CodeRebootstrap) {
				c.setState("rebootstrap_required")
				c.emit(Event{Type: EventRebootstrap})
				return ErrRebootstrap
			}
			return err
		}
		applied, errs, err := c.applyPage(cur.HubID, pr)
		if err != nil {
			return err
		}
		res.Pulled += len(pr.Changes)
		res.Applied += applied
		through = max(through, pr.Next)
		pulled = pulled || len(pr.Changes) > 0 || pr.Next > cur.PullAfter
		if errs > 0 {
			c.loop.mu.Lock()
			c.loop.applyErr += int64(errs)
			c.loop.mu.Unlock()
		}
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
