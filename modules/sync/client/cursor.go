//go:build !no_sync

package client

import (
	"database/sql"
	"errors"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/modules/sync/proto"
)

// Cursor is a row of `_sync_cursors` (one per hub).
type Cursor struct {
	HubID         string         `db:"hub_id"`
	HubURL        string         `db:"hub_url"`
	HubPub        string         `db:"hub_pub"`
	HubEpoch      string         `db:"hub_epoch"`
	NodeID        string         `db:"node_id"`
	Cert          string         `db:"cert"`
	PullAfter     int64          `db:"pull_after"`
	AckedOrigin   int64          `db:"acked_origin"`
	SchemaVersion int64          `db:"schema_version"`
	ClockOffsetMs int64          `db:"clock_offset_ms"`
	LastOK        sql.NullString `db:"last_ok"`
	LastError     string         `db:"last_error"`
	State         string         `db:"state"`
}

const cursorCols = "hub_id, hub_url, hub_pub, hub_epoch, node_id, cert, pull_after, acked_origin, schema_version, clock_offset_ms, last_ok, last_error, state"

// LoadCursor returns the first cursor row, or nil when the node is not enrolled.
func LoadCursor(app core.App) (*Cursor, error) {
	if !app.HasTable("_sync_cursors") {
		return nil, nil
	}
	var c Cursor
	err := app.DB().NewQuery("SELECT " + cursorCols + " FROM _sync_cursors ORDER BY rowid LIMIT 1").One(&c)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// StoreEnrollment saves the enroll answer (one row per hub).
func StoreEnrollment(app core.App, hubURL string, r *proto.EnrollResponse) error {
	_, err := app.NonconcurrentDB().NewQuery(`INSERT INTO _sync_cursors (hub_id, hub_url, hub_pub, node_id, cert)
VALUES ({:h}, {:u}, {:p}, {:n}, {:c})
ON CONFLICT(hub_id) DO UPDATE SET hub_url=excluded.hub_url, hub_pub=excluded.hub_pub, node_id=excluded.node_id, cert=excluded.cert, last_error=''`).
		Bind(dbx.Params{"h": r.HubID, "u": hubURL, "p": r.HubPub, "n": r.NodeID, "c": r.Cert}).Execute()
	return err
}

func (c *Client) recordOK(hubID, epoch string, offset time.Duration) {
	if c.o.App == nil {
		return
	}
	_, _ = c.o.App.NonconcurrentDB().NewQuery("UPDATE _sync_cursors SET hub_epoch=CASE WHEN hub_epoch='' THEN {:e} ELSE hub_epoch END, clock_offset_ms={:o}, last_ok={:t}, last_error='' WHERE hub_id={:h}").
		Bind(dbx.Params{"e": epoch, "o": offset.Milliseconds(), "t": time.Now().UTC().Format("2006-01-02 15:04:05.000Z"), "h": hubID}).Execute()
}

// storeCert keeps a renewed device certificate.
func (c *Client) storeCert(hubID, cert string) {
	if c.o.App == nil {
		return
	}
	_, _ = c.o.App.NonconcurrentDB().NewQuery("UPDATE _sync_cursors SET cert={:c} WHERE hub_id={:h}").
		Bind(dbx.Params{"c": cert, "h": hubID}).Execute()
}

const maxLastError = 512

// cleanError truncates hub-controlled text and strips control characters.
func cleanError(s string) string {
	b := make([]rune, 0, len(s))
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			r = ' '
		}
		b = append(b, r)
		if len(b) >= maxLastError {
			break
		}
	}
	return string(b)
}

func (c *Client) recordError(err error) {
	if c.o.App == nil || err == nil {
		return
	}
	q := "UPDATE _sync_cursors SET last_error={:e}"
	p := dbx.Params{"e": cleanError(err.Error())}
	if c.wantHub != "" {
		q += " WHERE hub_id={:h}"
		p["h"] = c.wantHub
	}
	_, _ = c.o.App.NonconcurrentDB().NewQuery(q).Bind(p).Execute()
}
