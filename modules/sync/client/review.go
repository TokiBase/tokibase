//go:build !no_sync

package client

import (
	"context"
	"strings"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/sync/hlc"
	"github.com/tokibase/tokibase/modules/sync/proto"
)

// A record whose change the hub parked, or whose revert the hub could not
// send because the actor may not view it, is "pending review": the node keeps
// serving its local value and lists the record in PendingReview until the hub
// decides (a normal revert or an applied row clears the mark).

const reviewPrefix = "review/"

func reviewKey(colID, rec string) string { return reviewPrefix + colID + "/" + rec }

// ReviewItem is a record that waits for a decision of the hub.
type ReviewItem struct {
	Collection string // collection id
	Record     string
	Notice     string // proto.NoticeParked or proto.NoticeInvisible
	Code       string // reason, e.g. actor_revoked
}

func (c *Client) markReview(tx kernel.App, col *core.Collection, ch *proto.PullChange) error {
	_, err := tx.NonconcurrentDB().NewQuery("INSERT INTO _sync_state (key, value) VALUES ({:k}, {:v}) ON CONFLICT(key) DO UPDATE SET value=excluded.value").
		Bind(dbx.Params{"k": reviewKey(col.Id, ch.Record), "v": ch.Notice + ":" + ch.Code}).Execute()
	if err != nil {
		return err
	}
	c.emit(Event{Type: EventParked, ID: ch.ID, Collection: col.Id, Record: ch.Record, Code: ch.Code, Message: "pending review (" + ch.Notice + ")"})
	return nil
}

func clearReview(db dbx.Builder, colID, rec string) error {
	_, err := db.NewQuery("DELETE FROM _sync_state WHERE key={:k}").Bind(dbx.Params{"k": reviewKey(colID, rec)}).Execute()
	return err
}

// PendingReview lists the records that wait for a decision of the hub.
func PendingReview(app core.App) ([]ReviewItem, error) {
	var rows []struct {
		Key   string `db:"key"`
		Value string `db:"value"`
	}
	if err := app.DB().NewQuery("SELECT key, value FROM _sync_state WHERE key LIKE 'review/%' ORDER BY key").All(&rows); err != nil {
		return nil, err
	}
	out := make([]ReviewItem, 0, len(rows))
	for _, r := range rows {
		parts := strings.SplitN(strings.TrimPrefix(r.Key, reviewPrefix), "/", 2)
		if len(parts) != 2 {
			continue
		}
		notice, code, _ := strings.Cut(r.Value, ":")
		out = append(out, ReviewItem{Collection: parts[0], Record: parts[1], Notice: notice, Code: code})
	}
	return out, nil
}

// applyEvict removes the local copy of a record that left the view of this
// node, WITHOUT a tombstone (the record still exists on the hub, so a later
// create must not be blocked and nothing is pushed back).
func (c *Client) applyEvict(tx kernel.App, col *core.Collection, ch *proto.PullChange) (bool, error) {
	if err := clearReview(tx.NonconcurrentDB(), col.Id, ch.Record); err != nil {
		return false, err
	}
	rec, _ := tx.FindRecordById(col.Id, ch.Record)
	if rec == nil {
		return false, nil
	}
	h, _ := hlc.Parse(ch.HLC)
	ctx := kernel.WithSyncOrigin(context.Background(), &kernel.SyncOrigin{Mode: kernel.SyncModePull, Node: ch.Node, HLC: uint64(h), ChangeID: ch.ID})
	if err := tx.DeleteWithContext(ctx, rec); err != nil {
		return false, err
	}
	_, err := tx.NonconcurrentDB().NewQuery("DELETE FROM _sync_tombstones WHERE collection={:c} AND record={:r} AND kind='delete'").
		Bind(dbx.Params{"c": col.Id, "r": ch.Record}).Execute()
	return true, err
}
