// Package mayar is the Mayar (Indonesia) payment adapter: single payment
// requests (https://docs.mayar.id/api-reference/reqpayment/create) and the
// payment.received webhook authenticated by a shared token header.
//
// Credentials come from the environment only:
//
//	TOKI_PAYMENTS_MAYAR_API_KEY        Bearer key (enables the adapter)
//	TOKI_PAYMENTS_MAYAR_MODE           sandbox (default) | live
//	TOKI_PAYMENTS_MAYAR_BASE_URL       override of the API base URL
//	TOKI_PAYMENTS_MAYAR_WEBHOOK_TOKEN  shared secret the webhook must present (required)
//	TOKI_PAYMENTS_MAYAR_WEBHOOK_HEADER header carrying it (default x-callback-token)
//	TOKI_PAYMENTS_MAYAR_REDIRECT_URL   default redirect after payment
//	TOKI_PAYMENTS_MAYAR_CONFIRM        1 = re-read the payment from the API before reporting paid
//	                                   (default 1 in live mode, 0 = off explicitly; a paid webhook without
//	                                   a usable amount is always confirmed through the API)
//
// The adapter is pure over bytes/JSON (see payments.Provider) so it can move
// into a WASM guest unchanged.
package mayar

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tokibase/tokibase/internal/netguard"
	"github.com/tokibase/tokibase/modules/payments"
)

const (
	liveBase    = "https://api.mayar.id/hl/v1"
	sandboxBase = "https://api.mayar.io/hl/v1"
	// AllowPrivateEnv lifts the SSRF guard of the HTTP client (tests, private gateways).
	AllowPrivateEnv = "TOKI_PAYMENTS_ALLOW_PRIVATE"
)

var errBlocked = errors.New("mayar: blocked address")

func init() {
	payments.RegisterFactory("mayar", func(getenv func(string) string) (payments.Provider, error) {
		key := getenv(payments.EnvKey("mayar", "API_KEY"))
		if key == "" {
			return nil, nil
		}
		base := getenv(payments.EnvKey("mayar", "BASE_URL"))
		live := false
		switch strings.ToLower(getenv(payments.EnvKey("mayar", "MODE"))) {
		case "live", "production":
			live = true
			if base == "" {
				base = liveBase
			}
		case "", "sandbox":
			if base == "" {
				base = sandboxBase
			}
		default:
			return nil, errors.New("TOKI_PAYMENTS_MAYAR_MODE must be sandbox or live")
		}
		hdr := strings.ToLower(getenv(payments.EnvKey("mayar", "WEBHOOK_HEADER")))
		if hdr == "" {
			hdr = "x-callback-token"
		}
		return &Provider{
			base: strings.TrimRight(base, "/"), apiKey: key, webhookToken: getenv(payments.EnvKey("mayar", "WEBHOOK_TOKEN")),
			webhookHeader: hdr, redirect: getenv(payments.EnvKey("mayar", "REDIRECT_URL")),
			confirm: confirmDefault(getenv(payments.EnvKey("mayar", "CONFIRM")), live),
			client:  netguard.NewClient(15*time.Second, AllowPrivateEnv, errBlocked),
			now:     time.Now,
		}, nil
	})
}

// confirmDefault: an explicit CONFIRM value wins; otherwise live mode confirms.
func confirmDefault(v string, live bool) bool {
	switch v {
	case "1", "true":
		return true
	case "0", "false":
		return false
	}
	return live
}

// Provider implements payments.Provider for Mayar.
type Provider struct {
	base, apiKey, webhookToken, webhookHeader, redirect string
	confirm                                             bool
	client                                              *http.Client
	now                                                 func() time.Time
}

// New builds a provider by hand (tests, embedding).
func New(base, apiKey, webhookToken string, client *http.Client) *Provider {
	return &Provider{base: strings.TrimRight(base, "/"), apiKey: apiKey, webhookToken: webhookToken,
		webhookHeader: "x-callback-token", client: client, now: time.Now}
}

// SetConfirm toggles the paid confirmation call.
func (p *Provider) SetConfirm(b bool) { p.confirm = b }

// SetNow replaces the clock (tests).
func (p *Provider) SetNow(f func() time.Time) { p.now = f }

func (p *Provider) Name() string { return "mayar" }

func (p *Provider) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("mayar: request failed: %s", scrub(err.Error(), p.apiKey))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("mayar: HTTP %d: %s", resp.StatusCode, scrub(snippet(raw), p.apiKey))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("mayar: bad response: %w", err)
		}
	}
	return nil
}

func scrub(s, secret string) string {
	if secret != "" {
		s = strings.ReplaceAll(s, secret, "***")
	}
	return s
}

func snippet(b []byte) string {
	if len(b) > 300 {
		b = b[:300]
	}
	return string(b)
}

type createResp struct {
	StatusCode int `json:"statusCode"`
	Data       struct {
		ID            string `json:"id"`
		TransactionID string `json:"transactionId"`
		Link          string `json:"link"`
	} `json:"data"`
}

func (p *Provider) CreateIntent(ctx context.Context, in *payments.IntentSpec) (string, string, error) {
	if !strings.EqualFold(in.Currency, "IDR") {
		return "", "", fmt.Errorf("mayar: only IDR is supported, got %s", in.Currency)
	}
	str := func(k, def string) string {
		if v, ok := in.Metadata[k].(string); ok && v != "" {
			return v
		}
		return def
	}
	name := in.CustomerName
	if name == "" {
		name = "Customer"
	}
	email := in.CustomerEmail
	if email == "" {
		email = str("email", "")
	}
	if email == "" {
		return "", "", errors.New("mayar: the customer needs an email address")
	}
	redirect := str("return_url", p.redirect)
	desc := in.Description
	if desc == "" {
		desc = "Order " + in.OrderRef
	}
	body := map[string]any{
		"name": name, "email": email, "amount": in.Amount, "mobile": str("mobile", "081200000000"),
		"redirectUrl": redirect, "description": desc + " [" + in.ID + "]",
		"expiredAt": p.now().UTC().Add(24 * time.Hour).Format("2006-01-02T15:04:05.000Z"),
	}
	var r createResp
	if err := p.do(ctx, http.MethodPost, "/payment/create", body, &r); err != nil {
		return "", "", err
	}
	if r.Data.ID == "" || r.Data.Link == "" {
		return "", "", errors.New("mayar: response without id or link")
	}
	ref := r.Data.ID
	if r.Data.TransactionID != "" {
		ref += "," + r.Data.TransactionID
	}
	return r.Data.Link, ref, nil
}

type webhookBody struct {
	Event string `json:"event"`
	Data  struct {
		ID            string          `json:"id"`
		TransactionID string          `json:"transactionId"`
		PaymentLinkID string          `json:"paymentLinkId"`
		Status        json.RawMessage `json:"status"`
		Amount        json.RawMessage `json:"amount"`
		UpdatedAt     json.RawMessage `json:"updatedAt"`
	} `json:"data"`
}

func statusText(raw json.RawMessage) string {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	return strings.ToLower(s)
}

func (p *Provider) VerifyWebhook(ctx context.Context, headers map[string]string, body []byte) ([]payments.WebhookEvent, error) {
	if p.webhookToken == "" {
		return nil, fmt.Errorf("%w: TOKI_PAYMENTS_MAYAR_WEBHOOK_TOKEN is not set", payments.ErrInvalidSignature)
	}
	got := headers[p.webhookHeader]
	if got == "" {
		got = strings.TrimPrefix(headers["authorization"], "Bearer ")
	}
	if subtle.ConstantTimeCompare([]byte(got), []byte(p.webhookToken)) != 1 {
		return nil, payments.ErrInvalidSignature
	}
	var wb webhookBody
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&wb); err != nil {
		return nil, fmt.Errorf("mayar: malformed body: %w", err)
	}
	h := sha256.Sum256([]byte(strings.Join([]string{wb.Event, wb.Data.ID, wb.Data.TransactionID, statusText(wb.Data.Status), string(wb.Data.UpdatedAt)}, "|")))
	ev := payments.WebhookEvent{
		EventID: hex.EncodeToString(h[:16]), Type: payments.EventIgnored,
		ProviderRef: wb.Data.ID, Currency: "IDR",
	}
	for _, a := range []string{wb.Data.TransactionID, wb.Data.PaymentLinkID} {
		if a != "" {
			ev.AltRefs = append(ev.AltRefs, a)
		}
	}
	if n, err := strconv.ParseInt(strings.Trim(strings.TrimSpace(string(wb.Data.Amount)), `"`), 10, 64); err == nil && n > 0 {
		ev.Amount = n // anything else (absent, 0, 15000.50, "abc") stays 0 = unknown
	}
	if wb.Event == "payment.received" {
		switch s := statusText(wb.Data.Status); s {
		case "paid", "success", "settled", "completed":
			ev.Type = payments.EventPaid
		case "failed", "cancelled", "canceled":
			ev.Type = payments.EventFailed
		case "expired":
			ev.Type = payments.EventExpired
		case "unpaid", "pending", "false", "created":
			ev.Type = payments.EventIgnored
		default:
			// absent, null, refunded, closed, anything new: never a payment
			ev.Type = payments.EventUnknown
		}
	}
	if ev.Type == payments.EventPaid {
		if wb.Data.ID == "" {
			ev.Type = payments.EventUnknown
		} else if p.confirm || ev.Amount <= 0 {
			// The webhook body is only as trustworthy as the shared token. Without
			// a usable amount the API is the only source of the price, so the
			// re-read is mandatory; an API outage is not a verdict (503, retried).
			st, err := p.FetchStatus(ctx, wb.Data.ID, nil)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", payments.ErrVerifyUnavailable, err)
			}
			switch {
			case st.State != payments.StatusPaid:
				ev.Type = payments.EventIgnored
			case st.Amount <= 0:
				ev.Type = payments.EventUnknown
			default:
				ev.Amount = st.Amount // the API is authoritative over the webhook body
			}
		}
	}
	return []payments.WebhookEvent{ev}, nil
}

func (p *Provider) Refund(context.Context, *payments.RefundSpec) (*payments.RefundResult, error) {
	return nil, payments.ErrUnsupported // Mayar exposes no public refund API
}

type detailResp struct {
	Data struct {
		ID     string      `json:"id"`
		Status string      `json:"status"`
		Amount json.Number `json:"amount"`
	} `json:"data"`
}

func (p *Provider) FetchStatus(ctx context.Context, ref string, _ map[string]any) (*payments.Status, error) {
	id, _, _ := strings.Cut(ref, ",")
	if id == "" || strings.ContainsAny(id, "/?# ") {
		return nil, errors.New("mayar: bad provider reference")
	}
	var r detailResp
	if err := p.do(ctx, http.MethodGet, "/payment/"+id, nil, &r); err != nil {
		return nil, err
	}
	st := &payments.Status{State: payments.StatusPending, Currency: "IDR"}
	if n, err := r.Data.Amount.Int64(); err == nil {
		st.Amount = n
	}
	switch strings.ToLower(r.Data.Status) {
	case "paid", "success", "settled", "completed":
		st.State = payments.StatusPaid
	// "closed" (a link/invoice closed in the dashboard) is NOT proof of payment: pending
	case "expired":
		st.State = payments.StatusExpired
	case "failed", "cancelled", "canceled":
		st.State = payments.StatusFailed
	}
	return st, nil
}
