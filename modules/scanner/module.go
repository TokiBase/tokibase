//go:build !no_scanner

package scanner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/subscriptions"
)

const (
	dedupeCapacity = 1024
	seqCapacity    = 2048
	seqTTL         = 10 * time.Minute
)

// ErrReplica is returned when a scan is ingested from a sync replica apply:
// scan events are local to the node and never come from the hub.
var ErrReplica = errors.New("scanner: scan events are local to the node and cannot be created by sync")

// RejectedError reports a code dropped by the scanner's filters.
type RejectedError struct{ Reason string }

func (e *RejectedError) Error() string { return "scan rejected: " + e.Reason }

// Event is one stored scan.
type Event struct {
	ID        string `json:"id" db:"id"`
	Scanner   string `json:"scanner" db:"scanner"`
	Code      string `json:"code" db:"code"`
	Symbology string `json:"symbology" db:"symbology"`
	Source    string `json:"source" db:"source"`
	DupCount  int    `json:"dup_count" db:"dup_count"`
	TS        string `json:"ts" db:"created"`
}

// Result is the outcome of [Module.Ingest].
type Result struct {
	Event
	// Duplicate is true when the code repeated inside the dedupe window (or
	// the client_seq was seen before): no new event was created.
	Duplicate bool `json:"duplicate"`
}

type dedupeEntry struct {
	id   string
	at   time.Time
	dups int
	ev   Event
}

type seqEntry struct {
	res Result
	at  time.Time
}

// Module is the registered scanner module.
type Module struct {
	app core.App
	now func() time.Time

	ingestMu sync.Mutex
	dedupe   *lru[*dedupeEntry]
	seqs     *lru[seqEntry]

	// open opens the device of a serial or evdev scanner; tests replace it.
	open func(*Scanner) (closer, error)

	supMu   sync.Mutex
	running map[string]*reader
	wake    chan struct{}
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	started bool
}

func newModule(app core.App) *Module {
	return &Module{
		app: app, now: func() time.Time { return time.Now().UTC() },
		dedupe: newLRU[*dedupeEntry](dedupeCapacity), seqs: newLRU[seqEntry](seqCapacity),
		open: openDevice, running: map[string]*reader{}, wake: make(chan struct{}, 1),
	}
}

// IngestOptions carries the request context of a scan.
type IngestOptions struct {
	Source    string
	Actor     string // "collection/id" of the authenticated caller, empty for readers
	ClientSeq string // optional idempotency key of the caller
	Symbology string // optional hint from the caller
}

// Ingest cleans, de-duplicates, stores and publishes one scan.
func (m *Module) Ingest(ctx context.Context, sc *Scanner, raw string, o IngestOptions) (*Result, error) {
	if kernel.IsSyncReplica(ctx) {
		return nil, ErrReplica
	}
	code, reason := sc.Clean(raw)
	if reason != "" {
		return nil, &RejectedError{Reason: reason}
	}
	if o.Source == "" {
		o.Source = sc.Kind
	}
	now := m.now()

	m.ingestMu.Lock()
	defer m.ingestMu.Unlock()

	seqKey := ""
	if o.ClientSeq != "" {
		seqKey = o.Actor + "|" + sc.Name + "|" + o.ClientSeq
		if e, ok := m.seqs.get(seqKey); ok && now.Sub(e.at) < seqTTL {
			r := e.res
			r.Duplicate = true
			return &r, nil
		}
	}

	key := sc.Name + "|" + code
	if sc.DedupeMs > 0 {
		if d, ok := m.dedupe.get(key); ok && now.Sub(d.at) < time.Duration(sc.DedupeMs)*time.Millisecond {
			d.dups++
			d.ev.DupCount = d.dups
			_, err := m.app.DB().Update(EventsCollection, dbx.Params{"dup_count": d.dups, "updated": fmtTime(now)},
				dbx.HashExp{"id": d.id}).Execute()
			if err != nil {
				m.app.Logger().Warn("scanner: failed to count a duplicate", "error", err)
			}
			res := &Result{Event: d.ev, Duplicate: true}
			if seqKey != "" {
				m.seqs.put(seqKey, seqEntry{res: *res, at: now})
			}
			return res, nil
		}
	}

	ev := Event{
		ID: core.GenerateDefaultRandomId(), Scanner: sc.Name, Code: code,
		Symbology: symbologyOf(code, o.Symbology), Source: o.Source, TS: fmtTime(now),
	}
	_, err := m.app.DB().Insert(EventsCollection, dbx.Params{
		"id": ev.ID, "scanner": ev.Scanner, "code": ev.Code, "symbology": ev.Symbology,
		"source": ev.Source, "actor": o.Actor, "dup_count": 0, "created": ev.TS, "updated": ev.TS,
	}).Execute()
	if err != nil {
		return nil, fmt.Errorf("scanner: failed to store the scan: %w", err)
	}
	if sc.DedupeMs > 0 {
		m.dedupe.put(key, &dedupeEntry{id: ev.ID, at: now, ev: ev})
	}
	res := &Result{Event: ev}
	if seqKey != "" {
		m.seqs.put(seqKey, seqEntry{res: *res, at: now})
	}
	m.publish(ev)
	return res, nil
}

// publish sends the event on "@scan" to the authenticated subscribers allowed
// to read scans.
func (m *Module) publish(ev Event) {
	b, _ := json.Marshal(map[string]any{
		"id": ev.ID, "scanner": ev.Scanner, "code": ev.Code, "symbology": ev.Symbology, "ts": ev.TS,
	})
	for _, c := range m.app.SubscriptionsBroker().Clients() {
		if !allowedAuth(c.Get(apis.RealtimeClientAuthKey)) {
			continue
		}
		// "@scan" and "@scan?options=..." are both subscriptions to the topic;
		// the event carries the name the client subscribed with
		for key := range c.Subscriptions(Topic) {
			if topicName(key) == Topic {
				go c.Send(subscriptions.Message{Name: key, Data: b})
			}
		}
	}
}

// allowedAuth applies TOKI_SCAN_TOPIC_AUTH to the auth record of a client.
func allowedAuth(v any) bool {
	rec, _ := v.(*core.Record)
	if rec == nil {
		return false
	}
	if topicSuperuserOnly() {
		return rec.Collection().Name == core.CollectionNameSuperusers
	}
	return true
}

// Events returns scans after the event `since` (oldest first). Without a known
// `since` it returns the newest `limit` scans and gap reports whether `since`
// was given but is no longer retained.
func (m *Module) Events(since, scanner string, limit int) (items []Event, gap bool, err error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	items = []Event{}
	var cursor int64 = -1
	if since != "" {
		var rid int64
		if e := m.app.DB().NewQuery("SELECT rowid FROM {{" + EventsCollection + "}} WHERE id={:id}").
			Bind(dbx.Params{"id": since}).Row(&rid); e == nil {
			cursor = rid
		} else {
			gap = true
		}
	}
	cols := "[[id]],[[scanner]],[[code]],[[symbology]],[[source]],[[dup_count]],[[created]]"
	where := "1=1"
	p := dbx.Params{"lim": limit}
	if scanner != "" {
		where += " AND [[scanner]]={:sc}"
		p["sc"] = scanner
	}
	if cursor >= 0 {
		p["cur"] = cursor
		err = m.app.DB().NewQuery("SELECT " + cols + " FROM {{" + EventsCollection + "}} WHERE " + where +
			" AND rowid>{:cur} ORDER BY rowid ASC LIMIT {:lim}").Bind(p).All(&items)
		return items, false, err
	}
	err = m.app.DB().NewQuery("SELECT " + cols + " FROM {{" + EventsCollection + "}} WHERE " + where +
		" ORDER BY rowid DESC LIMIT {:lim}").Bind(p).All(&items)
	for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
		items[i], items[j] = items[j], items[i]
	}
	return items, gap, err
}

// Prune deletes events older than the retention and returns how many.
func (m *Module) Prune() (int64, error) {
	cut := fmtTime(m.now().Add(-retention()))
	res, err := m.app.DB().NewQuery("DELETE FROM {{" + EventsCollection + "}} WHERE [[created]] < {:c}").
		Bind(dbx.Params{"c": cut}).Execute()
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// seqString renders a JSON client_seq (string or integer) as a key.
func seqString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return strings.TrimSpace(fmt.Sprint(v))
}
