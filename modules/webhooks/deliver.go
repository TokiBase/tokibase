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
	"net"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
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

// ---- SSRF guard ----

var extraBlocked = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // CGNAT
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
}

func allowPrivate() bool {
	v := strings.TrimSpace(os.Getenv("TOKI_WEBHOOK_ALLOW_PRIVATE"))
	return v == "1" || strings.EqualFold(v, "true")
}

// blockedIP reports whether ip is a loopback, private, link-local,
// unspecified, multicast or otherwise non-public address.
func blockedIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	for _, p := range extraBlocked {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// ErrBlockedAddress is returned when the target resolves to a non-public IP.
var ErrBlockedAddress = errors.New("webhook target resolves to a private, loopback or link-local address (set TOKI_WEBHOOK_ALLOW_PRIVATE=1 to allow)")

// guardControl runs on every outgoing connection with the already resolved
// IP, so it also defeats DNS rebinding and redirects to internal hosts.
func guardControl(network, address string, _ syscall.RawConn) error {
	if allowPrivate() {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("webhook: cannot parse resolved address %q", host)
	}
	if blockedIP(ip) {
		return ErrBlockedAddress
	}
	return nil
}

func newClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: guardControl}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:               nil, // a proxy would bypass the address guard
			DialContext:         dialer.DialContext,
			DisableKeepAlives:   true,
			TLSHandshakeTimeout: 10 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
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
		// hold the delivery until the webhook is enabled again (or replayed)
		setNext(app, d.Id, nowFn().Add(time.Minute))
		return nil
	}

	status, msg, ms := send(ctx, wh, d)
	finish(app, d, status, msg, ms, wh.MaxAttempts)
	return nil
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
	case d.Event == EventPing || attempt >= maxAttempts:
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
	id, err := insertDelivery(app, wh, EventPing, "", "", body, nowFn().Add(claimLease))
	if err != nil {
		return nil, err
	}
	if err := deliver(ctx, app, id); err != nil {
		return nil, err
	}
	return getDelivery(app, id)
}
