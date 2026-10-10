//go:build !no_sync

package client

import (
	"encoding/json"
	"time"
)

// JSON views for the embed and mobile facade (docs/SYNC_DESIGN.md §6.3). The
// shapes are part of the host contract: keys are snake_case and only grow.

// StatusDoc is the JSON of Status.
type StatusDoc struct {
	State          string   `json:"state"`
	Online         bool     `json:"online"`
	Metered        bool     `json:"metered"`
	LowPower       bool     `json:"low_power"`
	Background     bool     `json:"background"`
	BackgroundDone bool     `json:"background_done"`
	Paused         bool     `json:"paused"`
	Running        bool     `json:"running"`
	Pending        int64    `json:"pending"`
	Conflicts      int64    `json:"conflicts"`
	PullAfter      int64    `json:"pull_after"`
	AckedOrigin    int64    `json:"acked_origin"`
	LastOK         string   `json:"last_ok,omitempty"`
	LastError      string   `json:"last_error,omitempty"`
	OffsetMs       int64    `json:"offset_ms"`
	Failures       int      `json:"failures"`
	NextAttempt    string   `json:"next_attempt,omitempty"`
	ApplyErrors    int64    `json:"apply_errors"`
	Heal           string   `json:"heal,omitempty"`
	DigestMismatch []string `json:"digest_mismatch,omitempty"`
}

func rfc(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// Doc converts a Status. conflicts is the number of open conflicts mirrored on
// this node (see Client.StatusDoc).
func (s Status) Doc(conflicts int64) StatusDoc {
	return StatusDoc{
		State: s.State, Online: s.Online, Metered: s.Conditions.Metered, LowPower: s.Conditions.LowPower,
		Background: s.Conditions.Background, BackgroundDone: s.BackgroundDone, Paused: s.Paused, Running: s.Running,
		Pending: s.Pending, Conflicts: conflicts, PullAfter: s.PullAfter, AckedOrigin: s.AckedOrigin,
		LastOK: rfc(s.LastOK), LastError: s.LastError, OffsetMs: s.OffsetMs, Failures: s.Failures,
		NextAttempt: rfc(s.NextAttempt), ApplyErrors: s.ApplyErrors, Heal: s.Heal, DigestMismatch: s.DigestMismatch,
	}
}

// StatusDoc is Status plus the open conflict count, ready for JSON.
func (c *Client) StatusDoc() StatusDoc {
	var n int64
	if c.o.App != nil && c.o.App.HasTable("_sync_conflicts") {
		_ = c.o.App.DB().NewQuery("SELECT COUNT(*) FROM _sync_conflicts WHERE status='open'").Row(&n)
	}
	return c.Status().Doc(n)
}

// StatusJSON is StatusDoc encoded.
func (c *Client) StatusJSON() ([]byte, error) { return json.Marshal(c.StatusDoc()) }

// EventDoc is the JSON of an Event.
type EventDoc struct {
	Type       string `json:"type"`
	Time       string `json:"time"`
	ID         string `json:"id,omitempty"`
	Collection string `json:"collection,omitempty"`
	Record     string `json:"record,omitempty"`
	Code       string `json:"code,omitempty"`
	Message    string `json:"message,omitempty"`
}

// JSON encodes the event.
func (e Event) JSON() []byte {
	b, _ := json.Marshal(EventDoc{
		Type: e.Type, Time: e.Time.UTC().Format(time.RFC3339Nano), ID: e.ID, Collection: e.Collection,
		Record: e.Record, Code: e.Code, Message: e.Message,
	})
	return b
}

// Subscribe registers fn for every event of the loop (in addition to the
// Events channel). fn runs on the emitting goroutine and must not block. The
// returned function removes it.
func (c *Client) Subscribe(fn func(Event)) (cancel func()) {
	c.lsnMu.Lock()
	c.lsnSeq++
	id := c.lsnSeq
	if c.lsn == nil {
		c.lsn = map[int]func(Event){}
	}
	c.lsn[id] = fn
	c.lsnMu.Unlock()
	return func() {
		c.lsnMu.Lock()
		delete(c.lsn, id)
		c.lsnMu.Unlock()
	}
}

func (c *Client) fanOut(ev Event) {
	c.lsnMu.Lock()
	fns := make([]func(Event), 0, len(c.lsn))
	for _, f := range c.lsn {
		fns = append(fns, f)
	}
	c.lsnMu.Unlock()
	for _, f := range fns {
		func() {
			defer func() { _ = recover() }()
			f(ev)
		}()
	}
}

// Rebootstrap schedules a fresh snapshot bootstrap and wakes the loop.
func (c *Client) Rebootstrap(reason string) error {
	if c.o.App == nil {
		return ErrStopped
	}
	if reason == "" {
		reason = "rebootstrap requested by the app"
	}
	if err := ScheduleRebootstrap(c.o.App, reason); err != nil {
		return err
	}
	c.Kick()
	return nil
}
