//go:build !no_webhooks

package webhooks

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/internal/netguard"
)

const (
	maxErrorBytes = 4 << 10
	userAgent     = "TokiBase-Webhooks/1"
)

// backoffFn returns the delay before the next try after the attempt-th failed
// attempt (attempt starts at 1). Replaceable for tests.
var backoffFn = defaultBackoff

// SetBackoff replaces the retry backoff (tests). It returns a restore func.
func SetBackoff(fn func(attempt int) time.Duration) (restore func()) {
	prev := backoffFn
	backoffFn = fn
	return func() { backoffFn = prev }
}

// SetClock replaces the clock (tests). It returns a restore func.
func SetClock(fn func() time.Time) (restore func()) {
	prev := nowFn
	nowFn = fn
	return func() { nowFn = prev }
}

// defaultBackoff is 10s * 2^(attempt-1) capped at 1h, with +-20% jitter.
func defaultBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := time.Hour
	if attempt <= 9 { // 10s*2^8 = 2560s < 1h; 2^9 would exceed the cap
		d = 10 * time.Second << (attempt - 1)
		if d > time.Hour {
			d = time.Hour
		}
	}
	jitter := 0.8 + rand.Float64()*0.4
	return time.Duration(float64(d) * jitter)
}

// Sign returns the X-Toki-Signature value for a body sent at timestamp ts.
func Sign(secret string, ts int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(ts, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return "v1=" + hex.EncodeToString(mac.Sum(nil))
}

// ---- SSRF guard (shared: internal/netguard) ----

// ErrBlockedAddress is returned when the target resolves to a non-public IP.
var ErrBlockedAddress = errors.New("webhook target resolves to a private, loopback or link-local address (set TOKI_WEBHOOK_ALLOW_PRIVATE=1 to allow)")

func blockedIP(ip netip.Addr) bool { return netguard.BlockedIP(ip) }

func newClient(timeout time.Duration) *http.Client {
	return netguard.NewClient(timeout, "TOKI_WEBHOOK_ALLOW_PRIVATE", ErrBlockedAddress)
}

// ---- delivery ----

// deliver performs one delivery attempt for the row deliveryID and updates its
// state (delivered, failed with a scheduled retry, or dead). It is the only
// place with delivery logic, so it can later be run as a kernel job handler.
func deliver(ctx context.Context, app core.App, deliveryID string) error {
	d, err := getDelivery(app, deliveryID)
	if err != nil {
		return fmt.Errorf("delivery %s: %w", deliveryID, err)
	}
	if d.State == StateDelivered || d.State == StateDead {
		return nil
	}

	rec, err := app.FindRecordById(ConfigCollection, d.Webhook)
	if err != nil {
		finish(app, d, 0, "webhook no longer exists", 0, 1)
		return nil
	}
	wh := webhookOf(rec)
	if !wh.Enabled && d.Event != EventPing {
		// stop retrying: park the row as dead (kept for retention, replayable
		// once the webhook is enabled again)
		drop(app, d, "webhook disabled: delivery dropped (replay it after enabling the webhook)")
		return nil
	}

	status, msg, ms := send(ctx, wh, d)
	if d.Event != EventPing && status == 0 && ctx.Err() != nil {
		// shutdown interrupted the attempt: give it back without charging an attempt
		setNext(app, d.Id, nowFn())
		return nil
	}
	finish(app, d, status, msg, ms, wh.MaxAttempts)
	return nil
}

// drop marks a delivery dead without counting an attempt and without audit.
func drop(app core.App, d *Delivery, reason string) {
	if _, err := app.AuxDB().Update(DeliveriesTable, dbx.Params{
		"state": StateDead, "last_error": reason, "updated": fmtTime(nowFn()),
	}, dbx.HashExp{"id": d.Id}).Execute(); err != nil {
		app.Logger().Error("webhooks: failed to update delivery", "delivery", d.Id, "error", err)
	}
}

// send does the HTTP call. msg is empty on success (2xx).
func send(ctx context.Context, wh *Webhook, d *Delivery) (status int, msg string, ms int) {
	body := []byte(d.Payload)
	ctx, cancel := context.WithTimeout(ctx, time.Duration(wh.TimeoutMs)*time.Millisecond)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, wh.URL, bytes.NewReader(body))
	if err != nil {
		return 0, "invalid request: " + err.Error(), 0
	}
	for k, v := range wh.Headers {
		req.Header.Set(k, v)
	}
	ts := nowFn().Unix()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("X-Toki-Event", d.Event)
	req.Header.Set("X-Toki-Delivery", d.Id)
	if d.Seq > 0 {
		req.Header.Set("X-Toki-Seq", strconv.FormatInt(d.Seq, 10))
	}
	req.Header.Set("X-Toki-Timestamp", strconv.FormatInt(ts, 10))
	req.Header.Set("X-Toki-Signature", Sign(wh.Secret, ts, body))

	start := time.Now()
	res, err := newClient(time.Duration(wh.TimeoutMs) * time.Millisecond).Do(req)
	ms = int(time.Since(start) / time.Millisecond)
	if err != nil {
		return 0, truncate(err.Error()), ms
	}
	defer res.Body.Close()
	snippet, _ := io.ReadAll(io.LimitReader(res.Body, maxErrorBytes))
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		return res.StatusCode, "", ms
	}
	if res.StatusCode >= 300 && res.StatusCode < 400 {
		return res.StatusCode, truncate(fmt.Sprintf("HTTP %d: redirects are not followed (not retried): %s", res.StatusCode, snippet)), ms
	}
	return res.StatusCode, truncate(fmt.Sprintf("HTTP %d: %s", res.StatusCode, snippet)), ms
}

func truncate(s string) string {
	if len(s) > maxErrorBytes {
		return s[:maxErrorBytes]
	}
	return s
}

// finish records the outcome of an attempt.
func finish(app core.App, d *Delivery, status int, errMsg string, ms, maxAttempts int) {
	now := nowFn()
	attempt := d.Attempt + 1
	set := dbx.Params{
		"attempt": attempt, "last_status": status, "last_error": errMsg,
		"response_ms": ms, "updated": fmtTime(now),
	}
	dead := false
	switch {
	case errMsg == "":
		set["state"] = StateDelivered
	case d.Event == EventPing || attempt >= maxAttempts || (status >= 300 && status < 400):
		set["state"] = StateDead
		dead = true
	default:
		set["state"] = StateFailed
		set["next_at"] = fmtTime(now.Add(backoffFn(attempt)))
	}
	if _, err := app.AuxDB().Update(DeliveriesTable, set, dbx.HashExp{"id": d.Id}).Execute(); err != nil {
		app.Logger().Error("webhooks: failed to update delivery", "delivery", d.Id, "error", err)
		return
	}
	if dead && d.Event != EventPing {
		app.Logger().Warn("webhooks: delivery is dead", "delivery", d.Id, "webhook", d.Webhook, "event", d.Event, "attempts", attempt)
		audit(AuditDead, d.Collection, d.Id, map[string]any{
			"webhook": d.Webhook, "event": d.Event, "record": d.Record, "attempts": attempt,
			"last_status": status, "last_error": errMsg,
		})
	}
}

func setNext(app core.App, id string, t time.Time) {
	_, _ = app.AuxDB().Update(DeliveriesTable, dbx.Params{"next_at": fmtTime(t)}, dbx.HashExp{"id": id}).Execute()
}

// Ping sends a synchronous `ping` event to wh and returns the finished row.
// A failed ping is never retried.
func Ping(ctx context.Context, app core.App, wh *Webhook) (*Delivery, error) {
	if err := initTable(app); err != nil {
		return nil, err
	}
	p := newPayload(EventPing, "", "", map[string]any{"message": "ping from TokiBase", "webhook": wh.Name})
	body, _ := jsonMarshal(p)
	// next_at far ahead so no worker claims it while we deliver synchronously
	id, err := insertDelivery(app, wh, EventPing, "", "", body, 0, nowFn().Add(claimLease))
	if err != nil {
		return nil, err
	}
	if err := deliver(ctx, app, id); err != nil {
		return nil, err
	}
	return getDelivery(app, id)
}
