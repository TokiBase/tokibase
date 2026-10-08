package paypal

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/tokibase/tokibase/modules/payments"
)

const orderCreated = `{"id":"5O190127TN364715T","status":"PAYER_ACTION_REQUIRED","links":[{"href":"https://api.sandbox.paypal.com/v2/checkout/orders/5O190127TN364715T","rel":"self","method":"GET"},{"href":"https://www.sandbox.paypal.com/checkoutnow?token=5O190127TN364715T","rel":"payer-action","method":"GET"}]}`
const orderCompleted = `{"id":"5O190127TN364715T","status":"COMPLETED","purchase_units":[{"payments":{"captures":[{"id":"3C679366HH908993F","status":"COMPLETED","amount":{"currency_code":"USD","value":"12.50"}}],"refunds":[{"id":"1JU08902781691411","status":"COMPLETED","amount":{"currency_code":"USD","value":"2.50"}}]}}]}`
const orderApproved = `{"id":"5O190127TN364715T","status":"APPROVED"}`

const evCaptureCompleted = `{"id":"WH-1","event_type":"PAYMENT.CAPTURE.COMPLETED","resource":{"id":"3C679366HH908993F","status":"COMPLETED","custom_id":"int1","amount":{"currency_code":"USD","value":"12.50"},"supplementary_data":{"related_ids":{"order_id":"5O190127TN364715T"}}}}`
const evApproved = `{"id":"WH-2","event_type":"CHECKOUT.ORDER.APPROVED","resource":{"id":"5O190127TN364715T","status":"APPROVED"}}`
const evRefunded = `{"id":"WH-3","event_type":"PAYMENT.CAPTURE.REFUNDED","resource":{"id":"1JU08902781691411","status":"COMPLETED","amount":{"currency_code":"USD","value":"2.50"},"links":[{"rel":"up","href":"https://api.sandbox.paypal.com/v2/payments/captures/3C679366HH908993F"}]}}`

type fakePayPal struct {
	mu       sync.Mutex
	calls    []string
	tokens   int
	refundSt string
}

func (f *fakePayPal) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		switch {
		case r.URL.Path == "/v1/oauth2/token":
			u, p, ok := r.BasicAuth()
			if !ok || u != "cid" || p != "sec" {
				w.WriteHeader(401)
				return
			}
			f.mu.Lock()
			f.tokens++
			f.mu.Unlock()
			_, _ = w.Write([]byte(`{"access_token":"tok-abc","expires_in":3600}`))
			return
		case r.Header.Get("Authorization") != "Bearer tok-abc":
			w.WriteHeader(401)
		case r.URL.Path == "/v2/checkout/orders" && r.Method == "POST":
			var o map[string]any
			_ = json.Unmarshal(b, &o)
			if o["intent"] != "CAPTURE" || r.Header.Get("PayPal-Request-Id") != "int1" {
				w.WriteHeader(400)
				return
			}
			_, _ = w.Write([]byte(orderCreated))
		case strings.HasSuffix(r.URL.Path, "/capture"):
			_, _ = w.Write([]byte(orderCompleted))
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v2/checkout/orders/"):
			if strings.Contains(r.URL.Path, "APPROVEDORDER") {
				_, _ = w.Write([]byte(orderApproved))
				return
			}
			_, _ = w.Write([]byte(orderCompleted))
		case strings.HasSuffix(r.URL.Path, "/refund"):
			st := f.refundSt
			if st == "" {
				st = "COMPLETED"
			}
			_, _ = w.Write([]byte(`{"id":"1JU08902781691411","status":"` + st + `"}`))
		case r.URL.Path == "/v1/notifications/verify-webhook-signature":
			var o struct {
				Sig string          `json:"transmission_sig"`
				ID  string          `json:"webhook_id"`
				Ev  json.RawMessage `json:"webhook_event"`
			}
			_ = json.Unmarshal(b, &o)
			st := "FAILURE"
			if o.Sig == "valid-sig" && o.ID == "WH-ID" && len(o.Ev) > 2 {
				st = "SUCCESS"
			}
			_, _ = w.Write([]byte(`{"verification_status":"` + st + `"}`))
		default:
			w.WriteHeader(404)
		}
	})
}

func newP(t *testing.T) (*Provider, *fakePayPal) {
	t.Helper()
	f := &fakePayPal{}
	ts := httptest.NewServer(f.handler())
	t.Cleanup(ts.Close)
	p := New(ts.URL, "cid", "sec", "WH-ID", ts.Client())
	p.SetURLs("https://app.example/ok", "https://app.example/cancel")
	return p, f
}

func hdr(sig string) map[string]string {
	return map[string]string{
		"paypal-auth-algo": "SHA256withRSA", "paypal-cert-url": "https://api.sandbox.paypal.com/v1/notifications/certs/CERT-1",
		"paypal-transmission-id": "tid", "paypal-transmission-sig": sig, "paypal-transmission-time": "2026-10-08T10:00:00Z",
	}
}

func TestCreateIntentAndTokenCache(t *testing.T) {
	p, f := newP(t)
	url, ref, err := p.CreateIntent(context.Background(), &payments.IntentSpec{ID: "int1", Amount: 1250, Currency: "usd", Description: "Pro"})
	if err != nil || ref != "5O190127TN364715T" || !strings.Contains(url, "checkoutnow") {
		t.Fatalf("%q %q %v", url, ref, err)
	}
	if _, err := p.FetchStatus(context.Background(), ref, nil); err != nil {
		t.Fatal(err)
	}
	if f.tokens != 1 {
		t.Fatalf("access token must be cached, fetched %d times", f.tokens)
	}
}

func TestVerifyWebhook(t *testing.T) {
	p, _ := newP(t)
	ctx := context.Background()
	if _, err := p.VerifyWebhook(ctx, hdr("forged"), []byte(evCaptureCompleted)); !errors.Is(err, payments.ErrInvalidSignature) {
		t.Fatalf("forged: %v", err)
	}
	bad := hdr("valid-sig")
	bad["paypal-cert-url"] = "https://evil.example/cert"
	if _, err := p.VerifyWebhook(ctx, bad, []byte(evCaptureCompleted)); !errors.Is(err, payments.ErrInvalidSignature) {
		t.Fatalf("foreign cert url must be refused: %v", err)
	}
	missing := hdr("valid-sig")
	delete(missing, "paypal-transmission-id")
	if _, err := p.VerifyWebhook(ctx, missing, []byte(evCaptureCompleted)); !errors.Is(err, payments.ErrInvalidSignature) {
		t.Fatalf("missing header: %v", err)
	}
	noID := New(p.base, "cid", "sec", "", p.client)
	if _, err := noID.VerifyWebhook(ctx, hdr("valid-sig"), []byte(evCaptureCompleted)); !errors.Is(err, payments.ErrInvalidSignature) {
		t.Fatal("unset webhook id must fail closed")
	}

	evs, err := p.VerifyWebhook(ctx, hdr("valid-sig"), []byte(evCaptureCompleted))
	if err != nil || len(evs) != 1 {
		t.Fatal(err)
	}
	e := evs[0]
	if e.Type != payments.EventPaid || e.EventID != "WH-1" || e.ProviderRef != "5O190127TN364715T" || e.Amount != 1250 || e.Currency != "USD" || e.IntentID != "int1" {
		t.Fatalf("%+v", e)
	}
	if e.Data["ref_add"] != "3C679366HH908993F" {
		t.Fatalf("capture id must be kept: %v", e.Data)
	}
	evs, _ = p.VerifyWebhook(ctx, hdr("valid-sig"), []byte(evApproved))
	if evs[0].Type != payments.EventApproved || evs[0].ProviderRef != "5O190127TN364715T" {
		t.Fatalf("%+v", evs[0])
	}
	evs, _ = p.VerifyWebhook(ctx, hdr("valid-sig"), []byte(evRefunded))
	if evs[0].Type != payments.EventRefunded || evs[0].Amount != 250 || evs[0].ProviderRef != "3C679366HH908993F" || evs[0].Data["refund_ref"] != "1JU08902781691411" {
		t.Fatalf("%+v", evs[0])
	}
	evs, _ = p.VerifyWebhook(ctx, hdr("valid-sig"), []byte(`{"id":"WH-9","event_type":"BILLING.PLAN.CREATED","resource":{"id":"x"}}`))
	if evs[0].Type != payments.EventIgnored {
		t.Fatal("unrelated events are ignored")
	}
}

func TestCaptureAndStatus(t *testing.T) {
	p, _ := newP(t)
	st, err := p.Capture(context.Background(), "5O190127TN364715T", nil)
	if err != nil {
		t.Fatal(err)
	}
	// 2.50 of 12.50 already refunded in the fixture
	if st.State != payments.StatusPartiallyRefunded || st.Amount != 1250 || st.RefundedAmount != 250 || st.Data["capture_id"] != "3C679366HH908993F" {
		t.Fatalf("%+v", st)
	}
	st, _ = p.FetchStatus(context.Background(), "APPROVEDORDER", nil)
	if st.State != "approved" {
		t.Fatalf("%+v", st)
	}
	if _, err := p.FetchStatus(context.Background(), "a/b", nil); err == nil {
		t.Fatal("bad ref")
	}
}

func TestRefund(t *testing.T) {
	p, f := newP(t)
	r, err := p.Refund(context.Background(), &payments.RefundSpec{RefundID: "rf1", IntentID: "int1", Amount: 250, Currency: "USD", Data: map[string]any{"capture_id": "3C679366HH908993F"}})
	if err != nil || !r.Done || r.ProviderRef != "1JU08902781691411" {
		t.Fatalf("%+v %v", r, err)
	}
	f.refundSt = "PENDING"
	r, _ = p.Refund(context.Background(), &payments.RefundSpec{RefundID: "rf2", Amount: 250, Currency: "USD", Data: map[string]any{"capture_id": "C1"}})
	if r.Done {
		t.Fatal("pending refund is not done")
	}
	f.refundSt = "FAILED"
	if _, err := p.Refund(context.Background(), &payments.RefundSpec{RefundID: "rf3", Amount: 250, Currency: "USD", Data: map[string]any{"capture_id": "C1"}}); err == nil {
		t.Fatal("failed refund must error")
	}
	if _, err := p.Refund(context.Background(), &payments.RefundSpec{Amount: 1, Currency: "USD"}); err == nil {
		t.Fatal("no capture id")
	}
}

func TestSecretNeverInErrors(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte("sec leaked tok-abc"))
	}))
	defer ts.Close()
	p := New(ts.URL, "cid", "sec", "W", ts.Client())
	p.token, p.expire = "tok-abc", p.now().Add(3600e9)
	_, _, err := p.CreateIntent(context.Background(), &payments.IntentSpec{ID: "x", Amount: 1, Currency: "USD"})
	if err == nil || strings.Contains(err.Error(), "tok-abc") || strings.Contains(err.Error(), " sec ") {
		t.Fatalf("secret leaked: %v", err)
	}
}

func TestFactory(t *testing.T) {
	f := map[string]string{}
	get := func(k string) string { return f[k] }
	if p, err := payments.NewFromFactory("paypal", get); p != nil || err != nil {
		t.Fatal("unconfigured")
	}
	f["TOKI_PAYMENTS_PAYPAL_CLIENT_ID"] = "id"
	if _, err := payments.NewFromFactory("paypal", get); err == nil {
		t.Fatal("secret required")
	}
	f["TOKI_PAYMENTS_PAYPAL_CLIENT_SECRET"] = "s"
	p, err := payments.NewFromFactory("paypal", get)
	if err != nil || p.(*Provider).base != sandboxBase {
		t.Fatalf("default is sandbox: %v", err)
	}
	f["TOKI_PAYMENTS_PAYPAL_MODE"] = "live"
	p, _ = payments.NewFromFactory("paypal", get)
	if p.(*Provider).base != liveBase {
		t.Fatal("live")
	}
}
