package mayar

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tokibase/tokibase/modules/payments"
)

// Recorded shape of the public docs (https://docs.mayar.id/api-reference/reqpayment/create).
const createFixture = `{"statusCode":200,"messages":"success","data":{"id":"1f0a-pay","transactionId":"9c3d-trx","link":"https://example.myr.id/invoices/abc"}}`
const detailPaid = `{"statusCode":200,"messages":"success","data":{"id":"1f0a-pay","status":"paid","amount":50000}}`
const detailUnpaid = `{"statusCode":200,"messages":"success","data":{"id":"1f0a-pay","status":"unpaid","amount":50000}}`
const hookPaid = `{"event":"payment.received","data":{"id":"1f0a-pay","transactionId":"9c3d-trx","status":"SUCCESS","amount":50000,"customerEmail":"a@example.com","updatedAt":"2026-10-08T10:00:00Z"}}`

func server(t *testing.T, detail string) (*Provider, *[]string) {
	t.Helper()
	var seen []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer key-123" {
			w.WriteHeader(401)
			return
		}
		b, _ := io.ReadAll(r.Body)
		seen = append(seen, r.Method+" "+r.URL.Path+" "+string(b))
		switch {
		case r.Method == "POST" && r.URL.Path == "/hl/v1/payment/create":
			_, _ = w.Write([]byte(createFixture))
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/hl/v1/payment/"):
			_, _ = w.Write([]byte(detail))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(ts.Close)
	return New(ts.URL+"/hl/v1", "key-123", "hook-secret", ts.Client()), &seen
}

func TestCreateIntent(t *testing.T) {
	p, seen := server(t, detailPaid)
	url, ref, err := p.CreateIntent(context.Background(), &payments.IntentSpec{
		ID: "int1", Amount: 50000, Currency: "IDR", OrderRef: "o1", CustomerEmail: "a@example.com", CustomerName: "A",
		Metadata: map[string]any{"return_url": "https://app.example/back"},
	})
	if err != nil || url != "https://example.myr.id/invoices/abc" || ref != "1f0a-pay,9c3d-trx" {
		t.Fatalf("%q %q %v", url, ref, err)
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(strings.SplitN((*seen)[0], " ", 3)[2]), &body)
	if body["amount"].(float64) != 50000 || body["redirectUrl"] != "https://app.example/back" || body["email"] != "a@example.com" {
		t.Fatalf("body %v", body)
	}
	if _, _, err := p.CreateIntent(context.Background(), &payments.IntentSpec{Amount: 1, Currency: "USD", CustomerEmail: "a@b.c"}); err == nil {
		t.Fatal("non IDR must be refused")
	}
	if _, _, err := p.CreateIntent(context.Background(), &payments.IntentSpec{Amount: 1, Currency: "IDR"}); err == nil {
		t.Fatal("missing email must be refused")
	}
}

func TestAPIErrorDoesNotLeakKey(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte("oops key-123 oops"))
	}))
	defer ts.Close()
	p := New(ts.URL, "key-123", "t", ts.Client())
	_, _, err := p.CreateIntent(context.Background(), &payments.IntentSpec{Amount: 1, Currency: "IDR", CustomerEmail: "a@b.c"})
	if err == nil || strings.Contains(err.Error(), "key-123") {
		t.Fatalf("error must not contain the key: %v", err)
	}
}

func TestVerifyWebhook(t *testing.T) {
	p, _ := server(t, detailPaid)
	ctx := context.Background()
	for name, h := range map[string]map[string]string{
		"none": {}, "wrong": {"x-callback-token": "nope"}, "other header": {"x-other": "hook-secret"},
	} {
		if _, err := p.VerifyWebhook(ctx, h, []byte(hookPaid)); !errors.Is(err, payments.ErrInvalidSignature) {
			t.Errorf("%s: want invalid signature, got %v", name, err)
		}
	}
	evs, err := p.VerifyWebhook(ctx, map[string]string{"x-callback-token": "hook-secret"}, []byte(hookPaid))
	if err != nil || len(evs) != 1 {
		t.Fatal(err)
	}
	ev := evs[0]
	if ev.Type != payments.EventPaid || ev.ProviderRef != "1f0a-pay" || ev.Amount != 50000 || len(ev.AltRefs) != 1 || ev.EventID == "" {
		t.Fatalf("%+v", ev)
	}
	evs2, _ := p.VerifyWebhook(ctx, map[string]string{"x-callback-token": "hook-secret"}, []byte(hookPaid))
	if evs2[0].EventID != ev.EventID {
		t.Fatal("event id must be deterministic so redeliveries dedupe")
	}
	// other events are verified but ignored
	evs, _ = p.VerifyWebhook(ctx, map[string]string{"x-callback-token": "hook-secret"}, []byte(`{"event":"payment.reminder","data":{"id":"1f0a-pay"}}`))
	if evs[0].Type != payments.EventIgnored {
		t.Fatal("reminder must be ignored")
	}
	if _, err := p.VerifyWebhook(ctx, map[string]string{"x-callback-token": "hook-secret"}, []byte(`{`)); err == nil || errors.Is(err, payments.ErrInvalidSignature) {
		t.Fatalf("malformed body is a plain error: %v", err)
	}
	// no configured token fails closed
	open := New(p.base, "k", "", p.client)
	if _, err := open.VerifyWebhook(ctx, map[string]string{}, []byte(hookPaid)); !errors.Is(err, payments.ErrInvalidSignature) {
		t.Fatal("unset token must fail closed")
	}
}

func TestConfirmBeforePaid(t *testing.T) {
	p, _ := server(t, detailUnpaid)
	p.SetConfirm(true)
	evs, err := p.VerifyWebhook(context.Background(), map[string]string{"x-callback-token": "hook-secret"}, []byte(hookPaid))
	if err != nil || evs[0].Type != payments.EventIgnored {
		t.Fatalf("an unconfirmed payment must not be reported paid: %v %+v", err, evs)
	}
	p2, _ := server(t, detailPaid)
	p2.SetConfirm(true)
	evs, _ = p2.VerifyWebhook(context.Background(), map[string]string{"x-callback-token": "hook-secret"}, []byte(hookPaid))
	if evs[0].Type != payments.EventPaid {
		t.Fatal("confirmed payment must be paid")
	}
}

func TestFetchStatusAndRefund(t *testing.T) {
	p, _ := server(t, detailPaid)
	st, err := p.FetchStatus(context.Background(), "1f0a-pay,9c3d-trx", nil)
	if err != nil || st.State != payments.StatusPaid || st.Amount != 50000 {
		t.Fatalf("%+v %v", st, err)
	}
	p, _ = server(t, detailUnpaid)
	if st, _ := p.FetchStatus(context.Background(), "1f0a-pay", nil); st.State != payments.StatusPending {
		t.Fatal("unpaid is pending")
	}
	if _, err := p.FetchStatus(context.Background(), "../x", nil); err == nil {
		t.Fatal("path traversal in ref")
	}
	if _, err := p.Refund(context.Background(), &payments.RefundSpec{}); !errors.Is(err, payments.ErrUnsupported) {
		t.Fatal("refund unsupported")
	}
}

func TestFactoryFromEnv(t *testing.T) {
	f := map[string]string{}
	get := func(k string) string { return f[k] }
	p, err := payments.NewFromFactory("mayar", get)
	if err != nil || p != nil {
		t.Fatal("unconfigured provider must be nil")
	}
	f["TOKI_PAYMENTS_MAYAR_API_KEY"] = "k"
	f["TOKI_PAYMENTS_MAYAR_MODE"] = "bogus"
	if _, err := payments.NewFromFactory("mayar", get); err == nil {
		t.Fatal("bad mode")
	}
	f["TOKI_PAYMENTS_MAYAR_MODE"] = "live"
	p, err = payments.NewFromFactory("mayar", get)
	if err != nil || p.(*Provider).base != "https://api.mayar.id/hl/v1" {
		t.Fatalf("live base: %v", err)
	}
}

func hookBody(status, amount string) []byte {
	parts := []string{`"id":"1f0a-pay"`, `"transactionId":"9c3d-trx"`, `"updatedAt":"2026-10-08T10:00:00Z"`}
	if status != "-" {
		parts = append(parts, `"status":`+status)
	}
	if amount != "-" {
		parts = append(parts, `"amount":`+amount)
	}
	return []byte(`{"event":"payment.received","data":{` + strings.Join(parts, ",") + `}}`)
}

var hookHdr = map[string]string{"x-callback-token": "hook-secret"}

// Pm3: only known paid words pay; everything unrecognized changes nothing.
func TestUnknownStatusNeverPays(t *testing.T) {
	p, seen := server(t, detailPaid) // the API would say "paid": the webhook must not even get that far
	for _, st := range []string{`"PROCESSING"`, `"waiting"`, "-", "null", `""`, `"refunded"`, `"reversed"`, `"closed"`, "true", `"settled-ish"`} {
		evs, err := p.VerifyWebhook(context.Background(), hookHdr, hookBody(st, "50000"))
		if err != nil || evs[0].Type != payments.EventUnknown {
			t.Errorf("status %s: want unknown, got %v %v", st, evs, err)
		}
	}
	if len(*seen) != 0 {
		t.Fatal("an unknown status must not trigger API calls")
	}
	for _, st := range []string{`"unpaid"`, `"pending"`} {
		evs, _ := p.VerifyWebhook(context.Background(), hookHdr, hookBody(st, "50000"))
		if evs[0].Type != payments.EventIgnored {
			t.Errorf("status %s must be ignored", st)
		}
	}
	for _, st := range []string{`"paid"`, `"SUCCESS"`, `"Settled"`, `"completed"`} {
		evs, _ := p.VerifyWebhook(context.Background(), hookHdr, hookBody(st, "50000"))
		if evs[0].Type != payments.EventPaid {
			t.Errorf("status %s must pay", st)
		}
	}
}

// Pm4: a paid webhook without a usable amount is only believed after the API says so.
func TestMissingAmountNeedsTheAPI(t *testing.T) {
	ctx := context.Background()
	p, _ := server(t, detailPaid)
	for _, amt := range []string{"-", "0", "15000.50", `"abc"`} {
		evs, err := p.VerifyWebhook(ctx, hookHdr, hookBody(`"paid"`, amt))
		if err != nil || evs[0].Type != payments.EventPaid || evs[0].Amount != 50000 {
			t.Errorf("amount %s: the API amount must be used: %+v %v", amt, evs, err)
		}
	}
	unpaid, _ := server(t, detailUnpaid)
	if evs, _ := unpaid.VerifyWebhook(ctx, hookHdr, hookBody(`"paid"`, "-")); evs[0].Type != payments.EventIgnored {
		t.Fatal("API says unpaid: not paid")
	}
	noAmt, _ := server(t, `{"data":{"id":"1f0a-pay","status":"paid"}}`)
	if evs, _ := noAmt.VerifyWebhook(ctx, hookHdr, hookBody(`"paid"`, "-")); evs[0].Type != payments.EventUnknown {
		t.Fatal("no amount anywhere: cannot be verified, not paid")
	}
	down := New("http://127.0.0.1:1", "k", "hook-secret", &http.Client{})
	if _, err := down.VerifyWebhook(ctx, hookHdr, hookBody(`"paid"`, "-")); !errors.Is(err, payments.ErrVerifyUnavailable) {
		t.Fatalf("API outage is not a verdict: %v", err)
	}
	conf, _ := server(t, detailPaid)
	conf.SetConfirm(true)
	if evs, _ := conf.VerifyWebhook(ctx, hookHdr, hookBody(`"paid"`, "1")); evs[0].Amount != 50000 {
		t.Fatalf("confirm mode: the API amount wins over the webhook body, got %d", evs[0].Amount)
	}
}

// Pm5: a closed link is not a payment.
func TestClosedIsNotPaid(t *testing.T) {
	closed := `{"data":{"id":"1f0a-pay","status":"closed","amount":50000}}`
	p, _ := server(t, closed)
	st, err := p.FetchStatus(context.Background(), "1f0a-pay", nil)
	if err != nil || st.State != payments.StatusPending {
		t.Fatalf("closed must stay pending: %+v %v", st, err)
	}
	p.SetConfirm(true)
	if evs, _ := p.VerifyWebhook(context.Background(), hookHdr, hookBody(`"paid"`, "50000")); evs[0].Type != payments.EventIgnored {
		t.Fatal("a paid webhook for a closed link must not pay")
	}
	for _, s := range []string{"mystery", "", "refunded"} {
		q, _ := server(t, `{"data":{"id":"x","status":"`+s+`","amount":5}}`)
		if st, _ := q.FetchStatus(context.Background(), "x", nil); st.State != payments.StatusPending {
			t.Errorf("unknown API status %q must stay pending", s)
		}
	}
}

func TestLiveModeConfirmsByDefault(t *testing.T) {
	f := map[string]string{"TOKI_PAYMENTS_MAYAR_API_KEY": "k"}
	get := func(k string) string { return f[k] }
	build := func() *Provider {
		p, err := payments.NewFromFactory("mayar", get)
		if err != nil {
			t.Fatal(err)
		}
		return p.(*Provider)
	}
	if build().confirm {
		t.Fatal("sandbox: confirm is opt-in")
	}
	f["TOKI_PAYMENTS_MAYAR_MODE"] = "live"
	if !build().confirm {
		t.Fatal("live: confirm by default")
	}
	f["TOKI_PAYMENTS_MAYAR_CONFIRM"] = "0"
	if build().confirm {
		t.Fatal("explicit 0 turns it off")
	}
}
