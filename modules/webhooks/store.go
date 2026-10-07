// Package webhooks delivers record, collection and auth events to external
// HTTP endpoints with HMAC signatures, retries, dead-lettering and replay
// (see docs/modules/webhooks.md).
package webhooks

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/tools/security"
)

const (
	// ConfigCollection is the superuser-only config collection (main db).
	ConfigCollection = "_webhooks"
	// DeliveriesTable is the delivery queue/log table (auxiliary db).
	DeliveriesTable = "_webhook_deliveries"

	timeLayout = "2006-01-02 15:04:05.000Z"

	// Delivery states. `failed` means an attempt failed and a retry is
	// scheduled at next_at; `dead` means attempts are exhausted.
	StateQueued    = "queued"
	StateDelivered = "delivered"
	StateFailed    = "failed"
	StateDead      = "dead"

	// AuditDead is the action passed to the audit sink when a delivery dies.
	AuditDead = "webhook.dead"

	createTableSQL = `CREATE TABLE IF NOT EXISTS {{_webhook_deliveries}} (
		[[id]]          TEXT PRIMARY KEY NOT NULL,
		[[webhook]]     TEXT NOT NULL,
		[[event]]       TEXT NOT NULL,
		[[collection]]  TEXT NOT NULL DEFAULT '',
		[[record]]      TEXT NOT NULL DEFAULT '',
		[[payload]]     JSON,
		[[attempt]]     INTEGER NOT NULL DEFAULT 0,
		[[state]]       TEXT NOT NULL DEFAULT 'queued',
		[[next_at]]     TEXT NOT NULL DEFAULT '',
		[[last_status]] INTEGER NOT NULL DEFAULT 0,
		[[last_error]]  TEXT NOT NULL DEFAULT '',
		[[response_ms]] INTEGER NOT NULL DEFAULT 0,
		[[created]]     TEXT NOT NULL DEFAULT '',
		[[updated]]     TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_due ON {{_webhook_deliveries}} ([[state]], [[next_at]]);
	CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_webhook ON {{_webhook_deliveries}} ([[webhook]]);`
)

// Delivery is one row of _webhook_deliveries.
type Delivery struct {
	Id         string `db:"id" json:"id"`
	Webhook    string `db:"webhook" json:"webhook"`
	Event      string `db:"event" json:"event"`
	Collection string `db:"collection" json:"collection"`
	Record     string `db:"record" json:"record"`
	Payload    string `db:"payload" json:"-"`
	Attempt    int    `db:"attempt" json:"attempt"`
	State      string `db:"state" json:"state"`
	NextAt     string `db:"next_at" json:"next_at"`
	LastStatus int    `db:"last_status" json:"last_status"`
	LastError  string `db:"last_error" json:"last_error"`
	ResponseMs int    `db:"response_ms" json:"response_ms"`
	Created    string `db:"created" json:"created"`
	Updated    string `db:"updated" json:"updated"`
}

var nowFn = func() time.Time { return time.Now().UTC() }

func fmtTime(t time.Time) string { return t.UTC().Format(timeLayout) }

// initTable creates the deliveries table in the auxiliary db.
func initTable(app core.App) error {
	_, err := app.AuxDB().NewQuery(createTableSQL).Execute()
	return err
}

func insertDelivery(app core.App, wh *Webhook, event, collection, record string, payload []byte, nextAt time.Time) (string, error) {
	now := fmtTime(nowFn())
	id := security.RandomStringWithAlphabet(15, "abcdefghijklmnopqrstuvwxyz0123456789")
	_, err := app.AuxDB().Insert(DeliveriesTable, dbx.Params{
		"id": id, "webhook": wh.ID, "event": event, "collection": collection, "record": record,
		"payload": string(payload), "attempt": 0, "state": StateQueued,
		"next_at": fmtTime(nextAt), "created": now, "updated": now,
	}).Execute()
	return id, err
}

func getDelivery(app core.App, id string) (*Delivery, error) {
	d := &Delivery{}
	err := app.AuxDB().Select("*").From(DeliveriesTable).Where(dbx.HashExp{"id": id}).One(d)
	if err != nil {
		return nil, err
	}
	return d, nil
}

// ListDeliveries returns deliveries newest first, optionally filtered by state.
func ListDeliveries(app core.App, state string, limit int) ([]Delivery, error) {
	if !app.AuxHasTable(DeliveriesTable) {
		return nil, nil
	}
	q := app.AuxDB().Select("*").From(DeliveriesTable).OrderBy("created DESC", "id DESC")
	if state != "" {
		q.AndWhere(dbx.HashExp{"state": state})
	}
	if limit > 0 {
		q.Limit(int64(limit))
	}
	var out []Delivery
	return out, q.All(&out)
}

// Replay re-queues one delivery (by id) or, with id == "", every dead delivery.
// With hold the rows are hidden from workers for claimLease. It returns the number of re-queued rows. The attempt counter restarts at 0.
func Replay(app core.App, id string, hold bool) (int64, error) {
	now := fmtTime(nowFn())
	next := now
	if hold { // caller delivers it itself right away: hide it from workers
		next = fmtTime(nowFn().Add(claimLease))
	}
	set := dbx.Params{"state": StateQueued, "attempt": 0, "next_at": next, "updated": now}
	var exp dbx.Expression
	if id == "" {
		exp = dbx.HashExp{"state": StateDead}
	} else {
		exp = dbx.HashExp{"id": id}
	}
	res, err := app.AuxDB().Update(DeliveriesTable, set, exp).Execute()
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if id != "" && n == 0 {
		return 0, fmt.Errorf("delivery %q not found", id)
	}
	return n, nil
}

// claimLease is how long a claimed delivery is hidden from other workers.
const claimLease = 2 * time.Minute

// claimNext atomically claims one due delivery (queued or failed with
// next_at <= now). The claim pushes next_at forward by claimLease, so a worker
// crash makes the row due again later.
func claimNext(app core.App, now time.Time) (string, bool) {
	var row struct {
		Id string `db:"id"`
	}
	nowS := fmtTime(now)
	err := app.AuxDB().Select("id").From(DeliveriesTable).
		Where(dbx.NewExp("state IN ({:a}, {:b}) AND next_at <= {:now}", dbx.Params{"a": StateQueued, "b": StateFailed, "now": nowS})).
		OrderBy("next_at ASC").Limit(1).One(&row)
	if err != nil {
		return "", false
	}
	res, err := app.AuxDB().NewQuery("UPDATE {{" + DeliveriesTable + "}} SET [[next_at]]={:lease} WHERE [[id]]={:id} AND [[state]] IN ({:a}, {:b}) AND [[next_at]] <= {:now}").
		Bind(dbx.Params{"lease": fmtTime(now.Add(claimLease)), "id": row.Id, "a": StateQueued, "b": StateFailed, "now": nowS}).Execute()
	if err != nil {
		return "", false
	}
	n, _ := res.RowsAffected()
	return row.Id, n == 1
}

func pruneDelivered(app core.App, olderThan time.Time) {
	_, _ = app.AuxDB().Delete(DeliveriesTable, dbx.NewExp("state={:s} AND updated < {:t}", dbx.Params{"s": StateDelivered, "t": fmtTime(olderThan)})).Execute()
}

var errNotFound = errors.New("webhook not found")

func normalizeEvents(csv string) []string {
	var out []string
	for _, s := range strings.Split(csv, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
