//go:build !no_payments

package payments

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
)

const (
	maxWebhookBody  = 1 << 20
	maxInvalidStore = 16 << 10
)

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

// Ingest verifies a webhook with the provider adapter, stores the raw verified
// payload (or the rejected attempt) in _payment_events and dispatches the
// processing jobs. Verification happens before anything is stored as
// verified, and storage before processing.
func (m *Module) Ingest(ctx context.Context, providerName string, headers map[string]string, body []byte) (IngestResult, error) {
	var res IngestResult
	prov, err := m.Provider(providerName)
	if err != nil {
		return res, err
	}
	evs, verr := prov.VerifyWebhook(ctx, headers, body)
	if verr != nil {
		m.storeInvalid(providerName, headers, body, verr)
		audit(ActionWebhookBad, EventsCollection, "", map[string]any{"provider": providerName, "error": truncate(verr.Error(), 300)})
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
		if ev.Type == EventIgnored {
			rec.Set("status", "ignored")
		} else {
			rec.Set("status", "received")
		}
		if err := m.app.Save(rec); err != nil {
			if isUnique(err) {
				res.Duplicates++
				continue
			}
			return res, err
		}
		if ev.Type == EventIgnored {
			res.Ignored++
			continue
		}
		res.Accepted++
		m.dispatch(ctx, rec.Id)
	}
	return res, nil
}

func (m *Module) storeInvalid(provider string, headers map[string]string, body []byte, verr error) {
	col, err := m.app.FindCachedCollectionByNameOrId(EventsCollection)
	if err != nil {
		return
	}
	rec := core.NewRecord(col)
	rec.Set("provider", strings.ToLower(provider))
	rec.Set("verified", false)
	rec.Set("status", "invalid")
	rec.Set("verify_error", truncate(verr.Error(), 1900))
	rec.Set("raw", truncate(string(body), maxInvalidStore))
	rec.Set("headers", redactHeaders(headers))
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
	if !ipAllowed(name, ip) {
		audit(ActionWebhookBad, EventsCollection, "", map[string]any{"provider": name, "ip": ip, "error": "ip not allowed"})
		return e.ForbiddenError("", nil)
	}
	if !m.rl.allow(ip, time.Now()) {
		return e.TooManyRequestsError("", nil)
	}
	body, err := io.ReadAll(http.MaxBytesReader(e.Response, e.Request.Body, maxWebhookBody))
	if err != nil {
		return e.BadRequestError("Invalid body.", nil)
	}
	res, err := m.Ingest(e.Request.Context(), name, lowerHeaders(e.Request.Header), body)
	if err != nil {
		if errors.Is(err, ErrInvalidSignature) {
			return e.UnauthorizedError("", nil)
		}
		return e.BadRequestError("Invalid webhook.", nil)
	}
	return e.JSON(http.StatusOK, map[string]any{"received": res.Accepted, "duplicates": res.Duplicates, "ignored": res.Ignored})
}
