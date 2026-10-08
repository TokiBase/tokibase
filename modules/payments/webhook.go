//go:build !no_payments

package payments

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
)

// maxWebhookBody caps a webhook body (PayPal and Mayar events are a few KiB).
const maxWebhookBody = 64 << 10

// hourCap counts events per key in a fixed one hour window and bounds its own memory.
type hourCap struct {
	mu    sync.Mutex
	max   int
	start time.Time
	hits  map[string]int
}

func newHourCap(max int) *hourCap { return &hourCap{max: max, hits: map[string]int{}} }

// take reports whether key is still under the cap (and counts the hit).
func (c *hourCap) take(key string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if now.Sub(c.start) >= time.Hour {
		c.start, c.hits = now, map[string]int{}
	}
	if _, ok := c.hits[key]; !ok && len(c.hits) >= 10000 {
		return false // too many distinct sources: fail closed on the store, never on memory
	}
	c.hits[key]++
	return c.hits[key] <= c.max
}

// ipLimiter is the webhook endpoint's own per-IP fixed window limiter.
type ipLimiter struct {
	mu    sync.Mutex
	max   int
	start time.Time
	hits  map[string]int
}

func newIPLimiter(perMinute int) *ipLimiter {
	return &ipLimiter{max: perMinute, hits: map[string]int{}}
}

func (l *ipLimiter) allow(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.start) >= time.Minute {
		l.start, l.hits = now, map[string]int{}
	}
	l.hits[ip]++
	return l.hits[ip] <= l.max
}

// ipAllowed checks the optional allowlists: TOKI_PAYMENTS_WEBHOOK_IPS and
// TOKI_PAYMENTS_<PROVIDER>_WEBHOOK_IPS (comma separated IPs/CIDRs). When the
// provider list is set it wins; when neither is set every IP is allowed.
func ipAllowed(provider, ip string) bool {
	list := Getenv(EnvKey(provider, "WEBHOOK_IPS"))
	if list == "" {
		list = Getenv("TOKI_PAYMENTS_WEBHOOK_IPS")
	}
	if list == "" {
		return true
	}
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	for _, it := range strings.Split(list, ",") {
		it = strings.TrimSpace(it)
		if p, err := netip.ParsePrefix(it); err == nil {
			if p.Contains(addr) {
				return true
			}
		} else if a, err := netip.ParseAddr(it); err == nil && a == addr {
			return true
		}
	}
	return false
}

// redactHeaders lower-cases the names and drops everything that may carry a
// credential, so the stored event never contains secrets.
func redactHeaders(h map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range h {
		lk := strings.ToLower(k)
		skip := false
		for _, bad := range []string{"auth", "token", "secret", "key", "cookie", "sig", "password"} {
			if strings.Contains(lk, bad) {
				skip = true
				break
			}
		}
		if !skip {
			out[lk] = truncate(v, 512)
		}
	}
	return out
}

// IngestResult is the outcome of a webhook delivery.
type IngestResult struct {
	Accepted   int
	Duplicates int
	Ignored    int
}

// Ingest is IngestFrom without a client address (Go callers, tests).
func (m *Module) Ingest(ctx context.Context, providerName string, headers map[string]string, body []byte) (IngestResult, error) {
	return m.IngestFrom(ctx, "", providerName, headers, body)
}

// IngestFrom verifies a webhook with the provider adapter, stores the raw
// verified payload in _payment_events and dispatches the processing jobs.
// Nothing unverified is stored with its body: a failed verification leaves at
// most one small row (body hash + length, header names), and only for the
// first few failures per source address and hour. Verification that cannot
// run (ErrVerifyUnavailable) stores nothing and is returned as is.
func (m *Module) IngestFrom(ctx context.Context, ip, providerName string, headers map[string]string, body []byte) (IngestResult, error) {
	var res IngestResult
	prov, err := m.Provider(providerName)
	if err != nil {
		return res, err
	}
	evs, verr := prov.VerifyWebhook(ctx, headers, body)
	if verr != nil {
		if errors.Is(verr, ErrVerifyUnavailable) {
			m.app.Logger().Warn("payments: webhook verification unavailable", "provider", providerName, "error", verr)
			return res, verr
		}
		if m.bad.take(ip, m.now()) {
			m.storeInvalid(providerName, headers, body, verr)
			audit(ActionWebhookBad, EventsCollection, "", map[string]any{"provider": providerName, "error": truncate(verr.Error(), 300)})
		}
		return res, verr
	}
	col, err := m.app.FindCachedCollectionByNameOrId(EventsCollection)
	if err != nil {
		return res, err
	}
	red := redactHeaders(headers)
	for i := range evs {
		ev := evs[i]
		rec := core.NewRecord(col)
		rec.Set("provider", strings.ToLower(providerName))
		rec.Set("event_id", ev.EventID)
		rec.Set("type", ev.Type)
		rec.Set("provider_ref", truncate(ev.ProviderRef, 1000))
		rec.Set("verified", true)
		rec.Set("raw", string(body))
		rec.Set("headers", red)
		rec.Set("event", ev)
		quiet := ev.Type == EventIgnored || ev.Type == EventUnknown
		if quiet {
			rec.Set("status", "ignored")
		} else {
			rec.Set("status", "received")
		}
		if err := m.app.Save(rec); err != nil {
			if isUnique(err) {
				res.Duplicates++
				// a redelivery of an event that was stored but never finished
				// (enqueue failed, process died): dispatch it again
				if ex, _ := m.app.FindFirstRecordByFilter(EventsCollection, "provider={:p} && event_id={:e}",
					map[string]any{"p": strings.ToLower(providerName), "e": ev.EventID}); ex != nil {
					if st := ex.GetString("status"); st == "received" || st == "failed" {
						m.dispatch(ctx, ex.Id)
					}
				}
				continue
			}
			return res, err
		}
		if quiet {
			res.Ignored++
			if ev.Type == EventUnknown {
				m.app.Logger().Warn("payments: webhook with an unrecognized status ignored (intent stays as is)",
					"provider", providerName, "event", rec.Id, "provider_ref", ev.ProviderRef)
			}
			continue
		}
		res.Accepted++
		m.dispatch(ctx, rec.Id)
	}
	return res, nil
}

// storeInvalid keeps a minimal trace of a rejected delivery: the SHA-256 and
// size of the body and the header names. Never the body, never header values.
func (m *Module) storeInvalid(provider string, headers map[string]string, body []byte, verr error) {
	col, err := m.app.FindCachedCollectionByNameOrId(EventsCollection)
	if err != nil {
		return
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, strings.ToLower(k))
	}
	sort.Strings(names)
	sum := sha256.Sum256(body)
	rec := core.NewRecord(col)
	rec.Set("provider", strings.ToLower(provider))
	rec.Set("verified", false)
	rec.Set("status", "invalid")
	rec.Set("verify_error", truncate(verr.Error(), 1900))
	rec.Set("raw", fmt.Sprintf("sha256:%s len=%d", hex.EncodeToString(sum[:]), len(body)))
	rec.Set("headers", map[string]any{"names": truncate(strings.Join(names, ","), 2000)})
	if err := m.app.Save(rec); err != nil {
		m.app.Logger().Warn("payments: failed to store rejected webhook", "error", err)
	}
}

// dispatch enqueues the processing job; without a job queue it processes inline.
func (m *Module) dispatch(ctx context.Context, eventID string) {
	_, err := kernel.Jobs(m.app).Enqueue(ctx, JobProcess, map[string]string{"event": eventID},
		kernel.Unique("payments.process:"+eventID), kernel.MaxAttempts(8))
	if errors.Is(err, kernel.ErrNoJobQueue) {
		if perr := m.ProcessEvent(ctx, eventID); perr != nil {
			m.app.Logger().Error("payments: inline event processing failed", "event", eventID, "error", perr)
		}
		return
	}
	if err != nil {
		m.app.Logger().Error("payments: failed to enqueue event", "event", eventID, "error", err)
	}
}

func lowerHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		if len(v) > 0 {
			out[strings.ToLower(k)] = v[0]
		}
	}
	return out
}

func (m *Module) webhookHandler(e *core.RequestEvent) error {
	name := strings.ToLower(e.Request.PathValue("provider"))
	if _, err := m.Provider(name); err != nil {
		return e.NotFoundError("Unknown payment provider.", nil)
	}
	ip := e.RealIP()
	// own limiter first: it is the cheapest check and bounds everything below
	if !m.rl.allow(ip, time.Now()) {
		return e.TooManyRequestsError("", nil)
	}
	if !ipAllowed(name, ip) {
		if m.bad.take(ip, m.now()) {
			audit(ActionWebhookBad, EventsCollection, "", map[string]any{"provider": name, "ip": ip, "error": "ip not allowed"})
		}
		return e.ForbiddenError("", nil)
	}
	body, err := io.ReadAll(http.MaxBytesReader(e.Response, e.Request.Body, maxWebhookBody))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return e.Error(http.StatusRequestEntityTooLarge, "Webhook body too large.", nil)
		}
		return e.BadRequestError("Invalid body.", nil)
	}
	res, err := m.IngestFrom(e.Request.Context(), ip, name, lowerHeaders(e.Request.Header), body)
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidSignature):
			return e.UnauthorizedError("", nil)
		case errors.Is(err, ErrVerifyUnavailable):
			return e.Error(http.StatusServiceUnavailable, "Webhook verification is temporarily unavailable.", nil)
		}
		return e.BadRequestError("Invalid webhook.", nil)
	}
	return e.JSON(http.StatusOK, map[string]any{"received": res.Accepted, "duplicates": res.Duplicates, "ignored": res.Ignored})
}
