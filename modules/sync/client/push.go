//go:build !no_sync

package client

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/modules/sync/hlc"
	"github.com/tokibase/tokibase/modules/sync/proto"
)

type outRow struct {
	Seq    int64  `db:"origin_seq"`
	HLC    int64  `db:"hlc"`
	Base   int64  `db:"base_hlc"`
	Coll   string `db:"collection"`
	Record string `db:"record"`
	Op     string `db:"op"`
	Patch  string `db:"patch"`
	Hash   []byte `db:"hash"`
	SV     int64  `db:"schema_version"`
	Actor  string `db:"actor"`
	Tx     string `db:"tx"`
}

const pushBytesBudget = 4 << 20

// nextPage reads the next rows to push, in origin_seq order, keeping a tx
// group whole.
func (c *Client) nextPage(from int64, n int) ([]outRow, error) {
	var rows []outRow
	err := c.o.App.DB().NewQuery(`SELECT origin_seq, hlc, base_hlc, collection, record, op, patch, hash, schema_version, actor, tx
  FROM _changes WHERE node={:n} AND origin_seq>={:f} AND status IN ('local','pushed') ORDER BY origin_seq LIMIT {:l}`).
		Bind(dbx.Params{"n": c.nodeID, "f": from, "l": n}).All(&rows)
	if err != nil {
		return nil, err
	}
	// byte budget (never below one row)
	size := 0
	for i := range rows {
		size += len(rows[i].Patch) + 200
		if size > pushBytesBudget && i > 0 && (rows[i].Tx == "" || rows[i].Tx != rows[i-1].Tx) {
			rows = rows[:i]
			break
		}
	}
	if len(rows) > 0 {
		last := rows[len(rows)-1]
		if last.Tx != "" {
			var more []outRow
			if err := c.o.App.DB().NewQuery(`SELECT origin_seq, hlc, base_hlc, collection, record, op, patch, hash, schema_version, actor, tx
  FROM _changes WHERE node={:n} AND origin_seq>{:f} AND tx={:t} AND status IN ('local','pushed') ORDER BY origin_seq`).
				Bind(dbx.Params{"n": c.nodeID, "f": last.Seq, "t": last.Tx}).All(&more); err == nil {
				rows = append(rows, more...)
			}
		}
	}
	return rows, nil
}

func (c *Client) postPush(ctx context.Context, rows []outRow) (*proto.PushResponse, error) {
	req := proto.PushRequest{
		Batch:      c.nodeID + "-" + itoa(rows[0].Seq) + "-" + itoa(rows[len(rows)-1].Seq),
		ClientTime: c.wallNow().UTC().Format(proto.TimeLayout),
		Changes:    make([]proto.PushChange, 0, len(rows)),
	}
	for _, r := range rows {
		pc := proto.PushChange{
			ID: c.nodeID + ":" + itoa(r.Seq), HLC: hlc.HLC(r.HLC).String(), Collection: r.Coll, Record: r.Record,
			Op: r.Op, Patch: json.RawMessage(r.Patch), Actor: r.Actor, Tx: r.Tx, SV: r.SV,
		}
		if r.Base != 0 {
			pc.Base = hlc.HLC(r.Base).String()
		}
		if len(r.Hash) > 0 {
			pc.Hash = hex.EncodeToString(r.Hash)
		}
		req.SchemaVersion = max(req.SchemaVersion, r.SV)
		req.Changes = append(req.Changes, pc)
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	hdr := map[string]string{}
	if len(body) > 1024 {
		var zb bytes.Buffer
		zw := gzip.NewWriter(&zb)
		_, _ = zw.Write(body)
		_ = zw.Close()
		body, hdr["Content-Encoding"] = zb.Bytes(), "gzip"
	}
	b, err := c.authed(ctx, http.MethodPost, proto.PathPush, hdr, body)
	if err != nil {
		return nil, err
	}
	var resp proto.PushResponse
	if err := json.Unmarshal(b, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) markPushed(rows []outRow) {
	_, _ = c.o.App.NonconcurrentDB().NewQuery("UPDATE _changes SET status='pushed' WHERE node={:n} AND origin_seq>={:a} AND origin_seq<={:b} AND status='local'").
		Bind(dbx.Params{"n": c.nodeID, "a": rows[0].Seq, "b": rows[len(rows)-1].Seq}).Execute()
}

// pushAll pushes every pending change, page by page (docs/SYNC_DESIGN.md §3.4).
func (c *Client) pushAll(ctx context.Context, res *Result) error {
	if c.o.App == nil {
		return nil
	}
	gaps := 0
	for {
		c.loop.mu.Lock()
		from := c.loop.pushFrom
		c.loop.mu.Unlock()
		rows, err := c.nextPage(from, c.pageSize())
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		c.markPushed(rows)
		resp, err := c.postPush(ctx, rows)
		if err != nil {
			var he *Error
			if !errors.As(err, &he) {
				return err
			}
			switch he.Code {
			case proto.CodePushGap:
				pf, _ := he.Data["push_from"].(float64)
				if gaps++; gaps > 3 || pf <= 0 {
					return err
				}
				if err := c.reconcile(int64(pf)); err != nil {
					return err
				}
				c.loop.mu.Lock()
				c.loop.pushFrom = int64(pf)
				c.loop.mu.Unlock()
				continue
			case proto.CodeBatchTooLarge:
				c.loop.mu.Lock()
				c.loop.page = max(len(rows)/2, 1)
				c.loop.mu.Unlock()
				if len(rows) == 1 {
					return err
				}
				continue
			}
			return err
		}
		byID := make(map[string]outRow, len(rows))
		for _, row := range rows {
			byID[c.nodeID+":"+itoa(row.Seq)] = row
		}
		for _, r := range resp.Results {
			row := byID[r.ID]
			switch {
			case r.Status == proto.ResRejected:
				res.Rejected++
				c.emit(Event{Type: EventRejected, ID: r.ID, Code: r.Code})
				c.recordOutcome(row, r)
			case r.Status == proto.ResSuperseded || (r.Status == proto.ResDuplicate && r.Was == proto.ResSuperseded):
				res.Superseded++
				c.emit(Event{Type: EventSuperseded, ID: r.ID})
				c.recordOutcome(row, r)
			case r.Status == proto.ResParked || (r.Status == proto.ResDuplicate && r.Was == proto.ResParked):
				res.Parked++
				res.Rejected++ // PR4 counted a parked change as rejected; Parked is the finer count
				c.emit(Event{Type: EventParked, ID: r.ID, Code: r.Code, Message: "the change waits for an admin (toki sync conflicts)"})
				c.recordOutcome(row, r)
			default:
				res.Pushed++
				if r.Status == proto.ResMerged {
					c.recordOutcome(row, r)
				}
			}
		}
		if resp.AckedThrough < rows[len(rows)-1].Seq {
			return errors.New("sync: the hub did not acknowledge the whole push")
		}
		if _, err := c.o.App.NonconcurrentDB().NewQuery("UPDATE _changes SET status='acked' WHERE node={:n} AND origin_seq<={:a} AND status IN ('local','pushed')").
			Bind(dbx.Params{"n": c.nodeID, "a": resp.AckedThrough}).Execute(); err != nil {
			return err
		}
		_, _ = c.o.App.NonconcurrentDB().NewQuery("UPDATE _sync_cursors SET acked_origin={:a}").Bind(dbx.Params{"a": resp.AckedThrough}).Execute()
		c.loop.mu.Lock()
		c.loop.pushFrom = resp.AckedThrough + 1
		c.loop.mu.Unlock()
		if len(rows) < c.pageSize() {
			return nil
		}
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
