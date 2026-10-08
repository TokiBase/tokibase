// Package paypal is the PayPal adapter: Orders v2 (create, capture, status),
// captures refunds and webhooks verified with PayPal's
// /v1/notifications/verify-webhook-signature API.
//
// Credentials come from the environment only:
//
//	TOKI_PAYMENTS_PAYPAL_CLIENT_ID      REST app client id (enables the adapter)
//	TOKI_PAYMENTS_PAYPAL_CLIENT_SECRET  REST app secret
//	TOKI_PAYMENTS_PAYPAL_WEBHOOK_ID     id of the webhook registered in the PayPal dashboard (required)
//	TOKI_PAYMENTS_PAYPAL_MODE           sandbox (default) | live
//	TOKI_PAYMENTS_PAYPAL_BASE_URL       override of the API base URL
//	TOKI_PAYMENTS_PAYPAL_RETURN_URL     where the buyer returns after approving
//	TOKI_PAYMENTS_PAYPAL_CANCEL_URL     where the buyer returns after canceling
//
// Flow: the buyer approves the order, PayPal sends CHECKOUT.ORDER.APPROVED, the
// framework calls Capture, PAYMENT.CAPTURE.COMPLETED marks the intent paid.
package paypal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/tokibase/tokibase/internal/netguard"
	"github.com/tokibase/tokibase/modules/payments"
)

const (
	liveBase    = "https://api-m.paypal.com"
	sandboxBase = "https://api-m.sandbox.paypal.com"
	// AllowPrivateEnv lifts the SSRF guard of the HTTP client (tests, private gateways).
	AllowPrivateEnv = "TOKI_PAYMENTS_ALLOW_PRIVATE"
)

var errBlocked = errors.New("paypal: blocked address")

func init() {
	payments.RegisterFactory("paypal", func(getenv func(string) string) (payments.Provider, error) {
		id := getenv(payments.EnvKey("paypal", "CLIENT_ID"))
		if id == "" {
			return nil, nil
		}
		secret := getenv(payments.EnvKey("paypal", "CLIENT_SECRET"))
		if secret == "" {
			return nil, errors.New("TOKI_PAYMENTS_PAYPAL_CLIENT_SECRET is not set")
		}
		base := getenv(payments.EnvKey("paypal", "BASE_URL"))
		switch strings.ToLower(getenv(payments.EnvKey("paypal", "MODE"))) {
		case "live", "production":
			if base == "" {
				base = liveBase
			}
		case "", "sandbox":
			if base == "" {
				base = sandboxBase
			}
		default:
			return nil, errors.New("TOKI_PAYMENTS_PAYPAL_MODE must be sandbox or live")
		}
		p := New(base, id, secret, getenv(payments.EnvKey("paypal", "WEBHOOK_ID")),
			netguard.NewClient(20*time.Second, AllowPrivateEnv, errBlocked))
		p.returnURL = getenv(payments.EnvKey("paypal", "RETURN_URL"))
		p.cancelURL = getenv(payments.EnvKey("paypal", "CANCEL_URL"))
		return p, nil
	})
}

// Provider implements payments.Provider (and payments.Capturer) for PayPal.
type Provider struct {
	base, clientID, secret, webhookID string
	returnURL, cancelURL              string
	client                            *http.Client
	now                               func() time.Time

	mu     sync.Mutex
	token  string
	expire time.Time
}

// New builds a provider by hand (tests, embedding).
func New(base, clientID, secret, webhookID string, client *http.Client) *Provider {
	return &Provider{base: strings.TrimRight(base, "/"), clientID: clientID, secret: secret, webhookID: webhookID, client: client, now: time.Now}
}

// SetURLs sets the buyer return and cancel URLs.
func (p *Provider) SetURLs(ret, cancel string) { p.returnURL, p.cancelURL = ret, cancel }

func (p *Provider) Name() string { return "paypal" }

func (p *Provider) scrub(s string) string {
	for _, sec := range []string{p.secret, p.token} {
		if sec != "" {
			s = strings.ReplaceAll(s, sec, "***")
		}
	}
	return s
}

func (p *Provider) accessToken(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.token != "" && p.now().Before(p.expire) {
		return p.token, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.base+"/v1/oauth2/token", strings.NewReader("grant_type=client_credentials"))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(p.clientID, p.secret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("paypal: token request failed: %s", p.scrub(err.Error()))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("paypal: token HTTP %d", resp.StatusCode)
	}
	var t struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &t); err != nil || t.AccessToken == "" {
		return "", errors.New("paypal: bad token response")
	}
	p.token = t.AccessToken
	p.expire = p.now().Add(time.Duration(t.ExpiresIn-60) * time.Second)
	return p.token, nil
}

func (p *Provider) do(ctx context.Context, method, path string, reqID string, body, out any) error {
	tok, err := p.accessToken(ctx)
	if err != nil {
		return err
	}
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
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if reqID != "" {
		req.Header.Set("PayPal-Request-Id", reqID)
	}
	req.Header.Set("Prefer", "return=representation")
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("paypal: request failed: %s", p.scrub(err.Error()))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		s := raw
		if len(s) > 300 {
			s = s[:300]
		}
		return fmt.Errorf("paypal: HTTP %d: %s", resp.StatusCode, p.scrub(string(s)))
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

type money struct {
	Currency string `json:"currency_code"`
	Value    string `json:"value"`
}

func (p *Provider) CreateIntent(ctx context.Context, in *payments.IntentSpec) (string, string, error) {
	str := func(k, def string) string {
		if v, ok := in.Metadata[k].(string); ok && v != "" {
			return v
		}
		return def
	}
	cur := strings.ToUpper(in.Currency)
	ctxp := map[string]any{"user_action": "PAY_NOW", "shipping_preference": "NO_SHIPPING"}
	if r := str("return_url", p.returnURL); r != "" {
		ctxp["return_url"] = r
	}
	if c := str("cancel_url", p.cancelURL); c != "" {
		ctxp["cancel_url"] = c
	}
	pu := map[string]any{
		"reference_id": in.ID, "custom_id": in.ID,
		"amount": money{Currency: cur, Value: payments.FormatAmount(in.Amount, cur)},
	}
	if in.Description != "" {
		d := in.Description
		if len(d) > 127 {
			d = d[:127]
		}
		pu["description"] = d
	}
	body := map[string]any{
		"intent":         "CAPTURE",
		"purchase_units": []any{pu},
		"payment_source": map[string]any{"paypal": map[string]any{"experience_context": ctxp}},
	}
	var o struct {
		ID    string `json:"id"`
		Links []struct {
			Rel  string `json:"rel"`
			Href string `json:"href"`
		} `json:"links"`
	}
	if err := p.do(ctx, http.MethodPost, "/v2/checkout/orders", in.ID, body, &o); err != nil {
		return "", "", err
	}
	link := ""
	for _, l := range o.Links {
		if l.Rel == "payer-action" || (l.Rel == "approve" && link == "") {
			link = l.Href
		}
	}
	if o.ID == "" || link == "" {
		return "", "", errors.New("paypal: order response without id or approval link")
	}
	return link, o.ID, nil
}

type event struct {
	ID        string          `json:"id"`
	EventType string          `json:"event_type"`
	Resource  json.RawMessage `json:"resource"`
}

type resource struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	CustomID string `json:"custom_id"`
	Amount   *money `json:"amount"`
	Links    []struct {
		Rel  string `json:"rel"`
		Href string `json:"href"`
	} `json:"links"`
	Supp struct {
		Related struct {
			OrderID string `json:"order_id"`
		} `json:"related_ids"`
	} `json:"supplementary_data"`
}

func (p *Provider) VerifyWebhook(ctx context.Context, h map[string]string, body []byte) ([]payments.WebhookEvent, error) {
	if p.webhookID == "" {
		return nil, fmt.Errorf("%w: TOKI_PAYMENTS_PAYPAL_WEBHOOK_ID is not set", payments.ErrInvalidSignature)
	}
	cert := h["paypal-cert-url"]
	if u, err := url.Parse(cert); err != nil || u.Scheme != "https" || !(u.Hostname() == "paypal.com" || strings.HasSuffix(u.Hostname(), ".paypal.com")) {
		return nil, fmt.Errorf("%w: untrusted cert url", payments.ErrInvalidSignature)
	}
	for _, k := range []string{"paypal-transmission-id", "paypal-transmission-time", "paypal-transmission-sig", "paypal-auth-algo"} {
		if h[k] == "" {
			return nil, fmt.Errorf("%w: missing %s", payments.ErrInvalidSignature, k)
		}
	}
	if !json.Valid(body) {
		return nil, errors.New("paypal: malformed body")
	}
	req := map[string]any{
		"auth_algo": h["paypal-auth-algo"], "cert_url": cert, "transmission_id": h["paypal-transmission-id"],
		"transmission_sig": h["paypal-transmission-sig"], "transmission_time": h["paypal-transmission-time"],
		"webhook_id": p.webhookID, "webhook_event": json.RawMessage(body),
	}
	var out struct {
		Status string `json:"verification_status"`
	}
	if err := p.do(ctx, http.MethodPost, "/v1/notifications/verify-webhook-signature", "", req, &out); err != nil {
		return nil, err // verification could not run: not a verdict, the caller must see a server error
	}
	if out.Status != "SUCCESS" {
		return nil, payments.ErrInvalidSignature
	}
	var ev event
	if err := json.Unmarshal(body, &ev); err != nil {
		return nil, err
	}
	var r resource
	_ = json.Unmarshal(ev.Resource, &r)
	we := payments.WebhookEvent{EventID: ev.ID, Type: payments.EventIgnored, IntentID: r.CustomID}
	amt := func() {
		if r.Amount != nil {
			we.Currency = strings.ToUpper(r.Amount.Currency)
			if n, err := payments.ParseAmount(r.Amount.Value, we.Currency); err == nil {
				we.Amount = n
			}
		}
	}
	switch ev.EventType {
	case "CHECKOUT.ORDER.APPROVED":
		we.Type, we.ProviderRef = payments.EventApproved, r.ID
	case "PAYMENT.CAPTURE.COMPLETED":
		we.Type, we.ProviderRef = payments.EventPaid, r.Supp.Related.OrderID
		if we.ProviderRef == "" {
			we.ProviderRef = r.ID
		}
		we.AltRefs = []string{r.ID}
		we.Data = map[string]any{"capture_id": r.ID, "ref_add": r.ID}
		amt()
	case "PAYMENT.CAPTURE.DENIED":
		we.Type, we.ProviderRef, we.AltRefs = payments.EventFailed, r.Supp.Related.OrderID, []string{r.ID}
	case "PAYMENT.CAPTURE.REFUNDED":
		we.Type = payments.EventRefunded
		we.Data = map[string]any{"refund_ref": r.ID}
		for _, l := range r.Links {
			if l.Rel == "up" {
				if i := strings.LastIndex(l.Href, "/"); i >= 0 {
					we.AltRefs = append(we.AltRefs, l.Href[i+1:])
				}
			}
		}
		amt()
		if len(we.AltRefs) > 0 {
			we.ProviderRef = we.AltRefs[0]
		}
	}
	if we.ProviderRef == "" && len(we.AltRefs) == 0 && we.IntentID == "" && we.Type != payments.EventIgnored {
		we.Type = payments.EventIgnored
	}
	return []payments.WebhookEvent{we}, nil
}

func (p *Provider) Refund(ctx context.Context, r *payments.RefundSpec) (*payments.RefundResult, error) {
	cap, _ := r.Data["capture_id"].(string)
	if cap == "" || strings.ContainsAny(cap, "/?# ") {
		return nil, errors.New("paypal: the intent has no capture id yet")
	}
	body := map[string]any{
		"amount":    money{Currency: strings.ToUpper(r.Currency), Value: payments.FormatAmount(r.Amount, r.Currency)},
		"custom_id": r.IntentID,
	}
	if r.Reason != "" {
		n := r.Reason
		if len(n) > 255 {
			n = n[:255]
		}
		body["note_to_payer"] = n
	}
	var o struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := p.do(ctx, http.MethodPost, "/v2/payments/captures/"+cap+"/refund", r.RefundID, body, &o); err != nil {
		return nil, err
	}
	switch o.Status {
	case "COMPLETED":
		return &payments.RefundResult{ProviderRef: o.ID, Done: true}, nil
	case "PENDING":
		return &payments.RefundResult{ProviderRef: o.ID, Done: false}, nil
	}
	return nil, fmt.Errorf("paypal: refund status %s", o.Status)
}

type orderResp struct {
	ID            string `json:"id"`
	Status        string `json:"status"`
	PurchaseUnits []struct {
		Payments struct {
			Captures []struct {
				ID     string `json:"id"`
				Status string `json:"status"`
				Amount money  `json:"amount"`
			} `json:"captures"`
			Refunds []struct {
				Status string `json:"status"`
				Amount money  `json:"amount"`
			} `json:"refunds"`
		} `json:"payments"`
	} `json:"purchase_units"`
}

func orderStatus(o *orderResp) *payments.Status {
	st := &payments.Status{State: payments.StatusPending}
	switch o.Status {
	case "APPROVED":
		st.State = "approved"
	case "VOIDED":
		st.State = payments.StatusFailed
	case "COMPLETED":
		if len(o.PurchaseUnits) == 0 || len(o.PurchaseUnits[0].Payments.Captures) == 0 {
			return st
		}
		pay := o.PurchaseUnits[0].Payments
		c := pay.Captures[0]
		st.Currency = strings.ToUpper(c.Amount.Currency)
		st.Amount, _ = payments.ParseAmount(c.Amount.Value, st.Currency)
		st.Data = map[string]any{"capture_id": c.ID, "ref_add": c.ID}
		switch c.Status {
		case "COMPLETED", "PARTIALLY_REFUNDED", "REFUNDED":
			st.State = payments.StatusPaid
			for _, rf := range pay.Refunds {
				if rf.Status == "COMPLETED" {
					n, _ := payments.ParseAmount(rf.Amount.Value, st.Currency)
					st.RefundedAmount += n
				}
			}
			if st.RefundedAmount >= st.Amount && st.Amount > 0 {
				st.State = payments.StatusRefunded
			} else if st.RefundedAmount > 0 {
				st.State = payments.StatusPartiallyRefunded
			}
		case "DECLINED", "FAILED":
			st.State = payments.StatusFailed
		}
	}
	return st
}

func (p *Provider) FetchStatus(ctx context.Context, ref string, _ map[string]any) (*payments.Status, error) {
	id, _, _ := strings.Cut(ref, ",")
	if id == "" || strings.ContainsAny(id, "/?# ") {
		return nil, errors.New("paypal: bad provider reference")
	}
	var o orderResp
	if err := p.do(ctx, http.MethodGet, "/v2/checkout/orders/"+id, "", nil, &o); err != nil {
		return nil, err
	}
	return orderStatus(&o), nil
}

// Capture captures an approved order (idempotent through PayPal-Request-Id).
func (p *Provider) Capture(ctx context.Context, ref string, _ map[string]any) (*payments.Status, error) {
	id, _, _ := strings.Cut(ref, ",")
	if id == "" || strings.ContainsAny(id, "/?# ") {
		return nil, errors.New("paypal: bad provider reference")
	}
	var o orderResp
	if err := p.do(ctx, http.MethodPost, "/v2/checkout/orders/"+id+"/capture", "capture-"+id, map[string]any{}, &o); err != nil {
		return nil, err
	}
	return orderStatus(&o), nil
}

var _ payments.Capturer = (*Provider)(nil)
