//go:build !no_payments

package payments

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tokibase/tokibase/core"
)

// ---- helpers ---------------------------------------------------------------

func (e *env) productFull(t *testing.T, slug string, amount int, cur string, days, grace int) {
	t.Helper()
	col, _ := e.app.FindCollectionByNameOrId(ProductsCollection)
	r := core.NewRecord(col)
	r.Set("slug", slug)
	r.Set("enabled", true)
	r.Set("amount", amount)
	r.Set("currency", cur)
	r.Set("entitlement_key", "pro")
	r.Set("duration_days", days)
	r.Set("grace_days", grace)
	r.Set("balance_add", 10)
	if err := e.app.Save(r); err != nil {
		t.Fatal(err)
	}
}

func (e *env) newIntentFor(t *testing.T, who *core.Record) *core.Record {
	t.Helper()
	r, _, err := e.m.CreateIntent(context.Background(), CreateParams{
		Provider: "fake", Currency: "IDR", Product: "pro-month", OrderRef: "o", Customer: who,
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (e *env) ref(in *core.Record) string { return in.GetString("provider_ref") }

func (e *env) ent(t *testing.T, who *core.Record) *core.Record {
	t.Helper()
	r, err := e.app.FindFirstRecordByFilter(EntitlementsCollection, "subject={:s}", map[string]any{"s": who.Id})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func eventStatus(t *testing.T, e *env, eventID string) string {
	t.Helper()
	r, err := e.app.FindFirstRecordByFilter(EventsCollection, "event_id={:i}", map[string]any{"i": eventID})
	if err != nil {
		t.Fatalf("event %s: %v", eventID, err)
	}
	return r.GetString("status")
}

func near(t *testing.T, got, want time.Time, what string) {
	t.Helper()
	if d := got.Sub(want); d < -time.Minute || d > time.Minute {
		t.Fatalf("%s: got %v want about %v", what, got, want)
	}
}

// ---- Pm1 / Pm13: the price always comes from the product -----------------------

func TestProductPriceIntegrity(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.productFull(t, "pro-year", 0, "USD", 365, 0) // "client may choose" used to mean 1 USD buys a year
	e.productFull(t, "no-cur", 5000, "", 30, 0)    // currency chosen by the client: USD vs IDR is a 16000x difference
	e.productFull(t, "fixed", 1000, "USD", 30, 0)

	for _, c := range []CreateParams{
		{Provider: "fake", Currency: "USD", Product: "pro-year", Amount: 1, Customer: e.user},
		{Provider: "fake", Currency: "USD", Product: "pro-year", Customer: e.user},
		{Provider: "fake", Currency: "USD", Product: "no-cur", Amount: 5000, Customer: e.user},
		{Provider: "fake", Currency: "IDR", Product: "fixed", Customer: e.user},
		{Provider: "fake", Currency: "USD", Product: "fixed", Amount: 1, Customer: e.user},
		{Provider: "fake", Currency: "USD", Amount: MaxAmount + 1, Customer: e.user},
	} {
		if _, _, err := e.m.CreateIntent(ctx, c); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%+v must be refused, got %v", c, err)
		}
	}
	if len(e.prov.created) != 0 {
		t.Fatal("a refused request must not reach the provider")
	}
	in, _, err := e.m.CreateIntent(ctx, CreateParams{Provider: "fake", Product: "fixed", Customer: e.user})
	if err != nil || in.GetInt("amount") != 1000 || in.GetString("currency") != "USD" {
		t.Fatalf("product price and currency are the product's: %v", err)
	}

	// a 1 USD payment (even a genuine one) can never match the 1000 USD intent
	_, _ = e.deliver(t, "good", WebhookEvent{EventID: "cheap", Type: EventPaid, ProviderRef: e.ref(in), Amount: 1, Currency: "USD"})
	e.q.run(t, e.app)
	if status(t, e, in.Id) != StatusPending || e.m.Entitled(e.user.Id, "members", "pro") {
		t.Fatal("underpayment must not grant")
	}
}

// ---- Pm4: amount and currency are mandatory on paid ------------------------

func TestPaidNeedsAmountAndCurrency(t *testing.T) {
	e := setup(t)
	e.product(t, "pro-month", 50000, 30, 0)
	in := e.newIntent(t, "")
	for i, ev := range []WebhookEvent{
		{Type: EventPaid, ProviderRef: e.ref(in), Amount: 0, Currency: "IDR"},
		{Type: EventPaid, ProviderRef: e.ref(in), Amount: 50000},
		{Type: EventPaid, ProviderRef: e.ref(in), Amount: 50000, Currency: "USD"},
	} {
		ev.EventID = fmt.Sprintf("bad%d", i)
		_, _ = e.deliver(t, "good", ev)
	}
	if f := e.q.run(t, e.app); f != 0 {
		t.Fatal("mismatches are final, not retried")
	}
	if status(t, e, in.Id) != StatusPending || !e.hasAudit(ActionMismatch) || eventStatus(t, e, "bad0") != "rejected" {
		t.Fatal("a paid event without a matching amount and currency must be rejected")
	}

	// the reconcile / capture path checks the same
	e.now = e.now.Add(30 * time.Minute)
	for _, st := range []*Status{
		{State: StatusPaid},
		{State: StatusPaid, Amount: 1, Currency: "IDR"},
		{State: StatusPaid, Amount: 50000, Currency: "USD"},
	} {
		e.prov.statuses[e.ref(in)] = st
		rep, _ := e.m.Reconcile(context.Background())
		if rep.Errors != 1 || status(t, e, in.Id) != StatusPending {
			t.Fatalf("%+v: %+v", st, rep)
		}
	}
	e.prov.statuses[e.ref(in)] = &Status{State: StatusPaid, Amount: 50000, Currency: "IDR"}
	_, _ = e.m.Reconcile(context.Background())
	if status(t, e, in.Id) != StatusPaid {
		t.Fatal("a matching status pays")
	}
}

// ---- Pm2: money for a failed/expired intent is never dropped ---------------------

func TestLatePayment(t *testing.T) {
	e := setup(t)
	e.product(t, "pro-month", 50000, 30, 0)
	var lateFlags []bool
	OnPaid(e.app).BindFunc(func(ev *PaidEvent) error { lateFlags = append(lateFlags, ev.Late); return ev.Next() })
	lateHook := 0
	OnLatePayment(e.app).BindFunc(func(ev *PaidEvent) error { lateHook++; return ev.Next() })
	paid := func(id string, in *core.Record, byIntentID bool) {
		ev := WebhookEvent{EventID: id, Type: EventPaid, ProviderRef: e.ref(in), Amount: 50000, Currency: "IDR"}
		if byIntentID {
			ev.ProviderRef, ev.IntentID = "unrelated-ref", in.Id
		}
		_, _ = e.deliver(t, "good", ev)
		if f := e.q.run(t, e.app); f != 0 {
			t.Fatalf("%s: late payments are never rejected nor retried", id)
		}
	}

	// A: the customer failed once, retried on the same link and paid (inside the window)
	a := e.newIntent(t, "")
	_, _ = e.deliver(t, "good", WebhookEvent{EventID: "fa", Type: EventFailed, ProviderRef: e.ref(a)})
	e.q.run(t, e.app)
	if status(t, e, a.Id) != StatusFailed {
		t.Fatal("failed first")
	}
	paid("pa", a, false)
	if status(t, e, a.Id) != StatusPaid || !e.m.Entitled(e.user.Id, "members", "pro") || len(lateFlags) != 1 || !lateFlags[0] {
		t.Fatalf("re-opened to paid with Late=true: %s %v", status(t, e, a.Id), lateFlags)
	}
	if !e.hasAudit(ActionLate) || !e.hasAudit(`"level":"error"`) {
		t.Fatalf("a late payment is audited at error level: %v", e.audit)
	}

	// C: the event names the intent only by the echoed id (no matching provider reference):
	// never re-opened automatically
	c := e.newIntentFor(t, e.other)
	_, _ = e.deliver(t, "good", WebhookEvent{EventID: "fc", Type: EventFailed, ProviderRef: e.ref(c)})
	e.q.run(t, e.app)
	paid("pc", c, true)
	if status(t, e, c.Id) != StatusPaidLate || e.m.Entitled(e.other.Id, "members", "pro") || lateHook != 1 {
		t.Fatalf("paid_late expected, is %s", status(t, e, c.Id))
	}
	// the operator refunds instead of granting
	if _, err := e.m.Refund(context.Background(), c.Id, 0, "late", ""); err != nil || status(t, e, c.Id) != StatusRefunded {
		t.Fatalf("paid_late can be refunded: %v %s", err, status(t, e, c.Id))
	}

	// B: expired long ago, outside the window
	b := e.newIntentFor(t, e.other)
	_, _ = e.deliver(t, "good", WebhookEvent{EventID: "xb", Type: EventExpired, ProviderRef: e.ref(b)})
	e.q.run(t, e.app)
	e.now = e.now.Add(48 * time.Hour)
	paid("pb", b, false)
	if status(t, e, b.Id) != StatusPaidLate || e.m.Entitled(e.other.Id, "members", "pro") || lateHook != 2 {
		t.Fatalf("outside the window: paid_late, is %s", status(t, e, b.Id))
	}
	if eventStatus(t, e, "pb") != "processed" {
		t.Fatal("the event itself is processed, the decision is on the intent")
	}
	got, err := attentionIntents(e.app, 10)
	if err != nil || len(got) != 1 || got[0].Id != b.Id {
		t.Fatalf("--attention must list the paid_late intent: %v %v", got, err)
	}
	// operator accepts it
	if _, err := e.m.Transition(b.Id, StatusPaid, TransitionInfo{Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	if !e.m.Entitled(e.other.Id, "members", "pro") || len(lateFlags) != 2 || !lateFlags[1] {
		t.Fatal("resolving grants the entitlement and fires OnPaid with Late=true")
	}
	// the plain state machine still refuses failed -> paid for everybody else
	if err := CheckTransition(StatusFailed, StatusPaid); !errors.Is(err, ErrIllegalTransition) {
		t.Fatal("failed -> paid is only reachable through the late path")
	}
}

func TestIllegalEventStillRejected(t *testing.T) {
	e := setup(t)
	e.product(t, "pro-month", 50000, 30, 0)
	in := e.newIntent(t, "")
	_, _ = e.m.Transition(in.Id, StatusPaid, TransitionInfo{})
	_, _ = e.deliver(t, "good", WebhookEvent{EventID: "ff", Type: EventFailed, ProviderRef: e.ref(in)})
	if f := e.q.run(t, e.app); f != 0 || eventStatus(t, e, "ff") != "rejected" || !e.hasAudit(ActionRejected) {
		t.Fatal("paid -> failed is rejected and audited")
	}
}

// ---- Pm6 / Pm7: refunds ------------------------------------------------------------

func refundEvent(id string, in *core.Record, amount int64, ref string) WebhookEvent {
	return WebhookEvent{EventID: id, Type: EventRefunded, ProviderRef: in.GetString("provider_ref"), Amount: amount, Currency: "IDR",
		Data: map[string]any{"refund_ref": ref}}
}

func TestRefundBeforePaidIsParked(t *testing.T) {
	e := setup(t)
	e.product(t, "pro-month", 50000, 30, 0)

	// the paid event itself replays the parked refund
	in := e.newIntent(t, "")
	_, _ = e.deliver(t, "good", refundEvent("r1", in, 20000, "R1"))
	if f := e.q.run(t, e.app); f != 0 || eventStatus(t, e, "r1") != "pending_order" || status(t, e, in.Id) != StatusPending {
		t.Fatalf("an early refund is parked, not rejected (event %s)", eventStatus(t, e, "r1"))
	}
	_, _ = e.deliver(t, "good", WebhookEvent{EventID: "p1", Type: EventPaid, ProviderRef: e.ref(in), Amount: 50000, Currency: "IDR"})
	e.q.run(t, e.app)
	r, _ := e.m.GetIntent(in.Id)
	if r.GetString("status") != StatusPartiallyRefunded || r.GetInt("refunded_amount") != 20000 || eventStatus(t, e, "r1") != "processed" {
		t.Fatalf("parked refund must apply once paid: %s %d", r.GetString("status"), r.GetInt("refunded_amount"))
	}

	// or the reconcile cron does
	in2 := e.newIntent(t, "")
	_, _ = e.deliver(t, "good", refundEvent("r2", in2, 10000, "R2"))
	e.q.run(t, e.app)
	_, _ = e.m.Transition(in2.Id, StatusPaid, TransitionInfo{})
	e.now = e.now.Add(5 * time.Minute)
	rep, _ := e.m.Reconcile(context.Background())
	r, _ = e.m.GetIntent(in2.Id)
	if rep.Retried < 1 || r.GetInt("refunded_amount") != 10000 {
		t.Fatalf("reconcile must retry parked refunds: %+v %d", rep, r.GetInt("refunded_amount"))
	}

	// a refund for an intent that never gets paid is finally rejected
	in3 := e.newIntent(t, "")
	_, _ = e.deliver(t, "good", refundEvent("r3", in3, 10000, "R3"))
	e.q.run(t, e.app)
	e.now = e.now.Add(e.m.IntentTTL + time.Hour)
	_ = e.m.ProcessEvent(context.Background(), mustEvent(t, e, "r3"))
	if eventStatus(t, e, "r3") != "rejected" {
		t.Fatal("parked refunds expire")
	}
}

func mustEvent(t *testing.T, e *env, eventID string) string {
	t.Helper()
	r, err := e.app.FindFirstRecordByFilter(EventsCollection, "event_id={:i}", map[string]any{"i": eventID})
	if err != nil {
		t.Fatal(err)
	}
	return r.Id
}

func TestRefundEchoIsCountedOnce(t *testing.T) {
	e := setup(t)
	e.product(t, "pro-month", 50000, 30, 0)
	ctx := context.Background()
	refunded := func(in *core.Record) int {
		r, _ := e.m.GetIntent(in.Id)
		return r.GetInt("refunded_amount")
	}
	rows := func(in *core.Record) int {
		rs, _ := e.app.FindRecordsByFilter(RefundsCollection, "intent={:i}", "", 0, 0, map[string]any{"i": in.Id})
		return len(rs)
	}

	// 1. the echo arrives while the API refund has no provider id yet: wait, then match
	in := e.newIntent(t, "")
	_, _ = e.m.Transition(in.Id, StatusPaid, TransitionInfo{})
	col, _ := e.app.FindCollectionByNameOrId(RefundsCollection)
	pending := core.NewRecord(col)
	pending.Set("intent", in.Id)
	pending.Set("amount", 20000)
	pending.Set("currency", "IDR")
	pending.Set("status", "pending")
	pending.Set("source", "api")
	if err := e.app.Save(pending); err != nil {
		t.Fatal(err)
	}
	_, _ = e.deliver(t, "good", refundEvent("e1", in, 20000, "R9"))
	if f := e.q.run(t, e.app); f != 1 || refunded(in) != 0 || rows(in) != 1 {
		t.Fatalf("in-flight API refund: the echo must wait (failed=%d refunded=%d rows=%d)", f, refunded(in), rows(in))
	}
	pending.Set("provider_ref", "R9")
	_ = e.app.Save(pending)
	if f := e.q.run(t, e.app); f != 0 || refunded(in) != 20000 || rows(in) != 1 {
		t.Fatalf("retry must complete the API row: refunded=%d rows=%d", refunded(in), rows(in))
	}

	// 2. the echo was counted first, then the API answer carries the same provider id
	in2 := e.newIntent(t, "")
	_, _ = e.m.Transition(in2.Id, StatusPaid, TransitionInfo{})
	_, _ = e.deliver(t, "good", refundEvent("e2", in2, 20000, "R10"))
	e.q.run(t, e.app)
	e.prov.refundRef = "R10"
	rf, err := e.m.Refund(ctx, in2.Id, 20000, "", "")
	if err != nil || refunded(in2) != 20000 || !strings.Contains(rf.GetString("error"), "counted by") {
		t.Fatalf("API answer after the echo must not count again: %v refunded=%d", err, refunded(in2))
	}
	e.prov.refundRef = ""

	// 3. asynchronous API refund, the echo delivered twice
	in3 := e.newIntent(t, "")
	_, _ = e.m.Transition(in3.Id, StatusPaid, TransitionInfo{})
	e.prov.refundDone = false
	rf, err = e.m.Refund(ctx, in3.Id, 20000, "", "")
	if err != nil || rf.GetString("status") != "pending" || rf.GetString("provider_ref") == "" {
		t.Fatalf("pending refund keeps the provider id: %v %s", err, rf.GetString("status"))
	}
	_, _ = e.deliver(t, "good", refundEvent("e3a", in3, 20000, rf.GetString("provider_ref")))
	_, _ = e.deliver(t, "good", refundEvent("e3b", in3, 20000, rf.GetString("provider_ref")))
	e.q.run(t, e.app)
	if refunded(in3) != 20000 || rows(in3) != 1 {
		t.Fatalf("one refund, one count: refunded=%d rows=%d", refunded(in3), rows(in3))
	}

	// 4. an API refund the provider never confirms is marked failed (and audited loudly)
	in4 := e.newIntent(t, "")
	_, _ = e.m.Transition(in4.Id, StatusPaid, TransitionInfo{})
	rf, _ = e.m.Refund(ctx, in4.Id, 10000, "", "")
	e.now = e.now.Add(25 * time.Hour)
	_, _ = e.m.Reconcile(ctx)
	rf, _ = e.app.FindRecordById(RefundsCollection, rf.Id)
	if rf.GetString("status") != "failed" || !e.hasAudit("unconfirmed") {
		t.Fatalf("unconfirmed refund: %s", rf.GetString("status"))
	}
	e.prov.refundDone = true
}

func TestReconcileFindsProviderSideRefund(t *testing.T) {
	e := setup(t)
	e.product(t, "pro-month", 50000, 30, 0)
	in := e.newIntent(t, "")
	_, _ = e.m.Transition(in.Id, StatusPaid, TransitionInfo{})
	e.prov.statuses[e.ref(in)] = &Status{State: StatusPartiallyRefunded, Amount: 50000, Currency: "IDR", RefundedAmount: 10000}
	for i := 0; i < 2; i++ { // the second pass must not count it again
		if _, err := e.m.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	r, _ := e.m.GetIntent(in.Id)
	if r.GetString("status") != StatusPartiallyRefunded || r.GetInt("refunded_amount") != 10000 {
		t.Fatalf("reconcile must see the refund once: %s %d", r.GetString("status"), r.GetInt("refunded_amount"))
	}
	e.prov.statuses[e.ref(in)] = &Status{State: StatusRefunded, Amount: 50000, Currency: "IDR", RefundedAmount: 50000}
	_, _ = e.m.Reconcile(context.Background())
	if status(t, e, in.Id) != StatusRefunded || e.m.Entitled(e.user.Id, "members", "pro") {
		t.Fatal("full provider refund revokes")
	}
}

// ---- Pm8: events that never finish ---------------------------------------------------

func TestEventRetriesAndDeadLetter(t *testing.T) {
	e := setup(t)
	e.product(t, "pro-month", 50000, 30, 0)
	ctx := context.Background()
	lose := func() { e.q.jobs, e.q.unique = nil, map[string]bool{} }

	// stored but the job was lost: the reconcile cron processes it
	in := e.newIntent(t, "")
	ev := WebhookEvent{EventID: "lost", Type: EventPaid, ProviderRef: e.ref(in), Amount: 50000, Currency: "IDR"}
	_, _ = e.deliver(t, "good", ev)
	lose()
	e.now = e.now.Add(3 * time.Minute)
	rep, _ := e.m.Reconcile(ctx)
	if rep.Retried != 1 || status(t, e, in.Id) != StatusPaid {
		t.Fatalf("a received event must not stay unprocessed: %+v", rep)
	}

	// a redelivery of a stored, unfinished event is dispatched again
	in2 := e.newIntent(t, "")
	ev2 := WebhookEvent{EventID: "lost2", Type: EventPaid, ProviderRef: e.ref(in2), Amount: 50000, Currency: "IDR"}
	_, _ = e.deliver(t, "good", ev2)
	lose()
	res, _ := e.deliver(t, "good", ev2)
	if res.Duplicates != 1 || len(e.q.jobs) != 1 {
		t.Fatalf("duplicate of an unfinished event must re-dispatch: %+v jobs=%d", res, len(e.q.jobs))
	}
	e.q.run(t, e.app)
	if status(t, e, in2.Id) != StatusPaid {
		t.Fatal("redelivery must finish the payment")
	}
	res, _ = e.deliver(t, "good", ev2)
	if res.Duplicates != 1 || len(e.q.jobs) != 0 {
		t.Fatal("a processed event is not dispatched again")
	}

	// no intent: retried for orphanGrace (the doc says so), then rejected and audited
	_, _ = e.deliver(t, "good", WebhookEvent{EventID: "orph", Type: EventPaid, ProviderRef: "nobody", Amount: 5, Currency: "IDR"})
	if f := e.q.run(t, e.app); f != 1 || eventStatus(t, e, "orph") != "failed" {
		t.Fatal("orphan waits for its intent")
	}
	e.now = e.now.Add(orphanGrace + time.Minute)
	if f := e.q.run(t, e.app); f != 0 || eventStatus(t, e, "orph") != "rejected" || !e.hasAudit("no matching intent") {
		t.Fatalf("orphan after the grace is rejected and audited: %s", eventStatus(t, e, "orph"))
	}

	// dead letter after MaxEventAttempts
	e.now = time.Now().UTC()
	e.m.MaxEventAttempts = 2
	_, _ = e.deliver(t, "good", WebhookEvent{EventID: "dead", Type: EventPaid, ProviderRef: "nobody2", Amount: 5, Currency: "IDR"})
	e.q.run(t, e.app)
	if f := e.q.run(t, e.app); f != 0 || eventStatus(t, e, "dead") != "dead" || !e.hasAudit(ActionDead) {
		t.Fatalf("dead-letter: %s", eventStatus(t, e, "dead"))
	}
	rep, _ = e.m.Reconcile(ctx)
	if eventStatus(t, e, "dead") != "dead" {
		t.Fatal("dead events are left for a human (replay)")
	}
}

// ---- Pm9: per-intent grant ledger ----------------------------------------------------

func TestRefundRevokesOnlyItsOwnGrant(t *testing.T) {
	e := setup(t)
	e.product(t, "pro-month", 50000, 30, 0)
	ctx := context.Background()
	pay := func(who *core.Record) *core.Record {
		in := e.newIntentFor(t, who)
		if _, err := e.m.Transition(in.Id, StatusPaid, TransitionInfo{}); err != nil {
			t.Fatal(err)
		}
		return in
	}

	a, b := pay(e.user), pay(e.user)
	ent := e.ent(t, e.user)
	near(t, ent.GetDateTime("until").Time(), e.now.Add(60*24*time.Hour), "two renewals stack")
	if ent.GetInt("balance") != 20 {
		t.Fatal("balance of both grants")
	}
	if _, err := e.m.Refund(ctx, a.Id, 0, "", ""); err != nil {
		t.Fatal(err)
	}
	ent = e.ent(t, e.user)
	near(t, ent.GetDateTime("until").Time(), e.now.Add(30*24*time.Hour), "refunding A takes back only A's 30 days")
	if ent.GetString("status") != EntActive || ent.GetInt("balance") != 10 || !e.m.Entitled(e.user.Id, "members", "pro") {
		t.Fatalf("B's contribution must survive: %s balance=%d", ent.GetString("status"), ent.GetInt("balance"))
	}
	if _, err := e.m.Refund(ctx, b.Id, 0, "", ""); err != nil {
		t.Fatal(err)
	}
	ent = e.ent(t, e.user)
	if ent.GetString("status") != EntLapsed || ent.GetInt("balance") != 0 || e.m.Entitled(e.user.Id, "members", "pro") {
		t.Fatal("nothing left: lapsed")
	}

	// the other order: refunding the latest keeps the first
	c, d := pay(e.other), pay(e.other)
	_ = c
	if _, err := e.m.Refund(ctx, d.Id, 0, "", ""); err != nil {
		t.Fatal(err)
	}
	ent = e.ent(t, e.other)
	near(t, ent.GetDateTime("until").Time(), e.now.Add(30*24*time.Hour), "refunding B keeps A")
	if !e.m.Entitled(e.other.Id, "members", "pro") {
		t.Fatal("A's days were paid and not refunded")
	}
}

// ---- Pm10: grace is a pure function of `until` ---------------------------------------

func TestGraceDoesNotFlicker(t *testing.T) {
	e := setup(t)
	e.product(t, "pro-month", 50000, 30, 3)
	in := e.newIntent(t, "")
	_, _ = e.m.Transition(in.Id, StatusPaid, TransitionInfo{})
	ent := e.ent(t, e.user)
	until := ent.GetDateTime("until").Time()

	e.now = until.Add(time.Hour) // the sweep has NOT run
	if !e.m.Entitled(e.user.Id, "members", "pro") {
		t.Fatal("inside the grace period access continues without waiting for the sweep")
	}
	e.now = until.Add(4 * 24 * time.Hour)
	if e.m.Entitled(e.user.Id, "members", "pro") {
		t.Fatal("after the grace period access is gone without waiting for the sweep")
	}
	// a late sweep goes straight to lapsed; it never re-grants the grace
	if n, err := e.m.Sweep(); err != nil || n != 1 {
		t.Fatalf("sweep: %d %v", n, err)
	}
	if got := e.ent(t, e.user); got.GetString("status") != EntLapsed {
		t.Fatalf("late sweep must lapse: %s", got.GetString("status"))
	}
	// the sweep in time keeps `until` as the paid end
	in2 := e.newIntentFor(t, e.other)
	e.now = time.Now().UTC()
	_, _ = e.m.Transition(in2.Id, StatusPaid, TransitionInfo{})
	u2 := e.ent(t, e.other).GetDateTime("until").Time()
	e.now = u2.Add(time.Hour)
	_, _ = e.m.Sweep()
	got := e.ent(t, e.other)
	if got.GetString("status") != EntGrace || !got.GetDateTime("until").Time().Equal(u2) {
		t.Fatalf("grace keeps the paid end: %s %v vs %v", got.GetString("status"), got.GetDateTime("until"), u2)
	}
	if n, _ := e.m.Sweep(); n != 0 {
		t.Fatal("a second sweep inside the grace changes nothing")
	}
}

func TestEntitledRuleGraceInSQL(t *testing.T) {
	e := setup(t)
	mk := func(name string) {
		col := core.NewBaseCollection(name)
		col.Fields.Add(&core.TextField{Name: "title"})
		rule := fmt.Sprintf(`entitled(%q) = true`, name)
		col.ListRule = &rule
		if err := e.app.Save(col); err != nil {
			t.Fatal(err)
		}
		r := core.NewRecord(col)
		r.Set("title", "x")
		if err := e.app.Save(r); err != nil {
			t.Fatal(err)
		}
	}
	ent := func(key, status string, ago time.Duration, graceSeconds int) {
		col, _ := e.app.FindCollectionByNameOrId(EntitlementsCollection)
		r := core.NewRecord(col)
		r.Set("subject", e.user.Id)
		r.Set("subject_collection", "members")
		r.Set("key", key)
		r.Set("status", status)
		r.Set("until", time.Now().UTC().Add(-ago))
		r.Set("grace_seconds", graceSeconds)
		if err := e.app.Save(r); err != nil {
			t.Fatal(err)
		}
	}
	for _, k := range []string{"ingrace", "pastgrace", "nograce", "trialnograce"} {
		mk(k)
	}
	ent("ingrace", EntActive, time.Hour, 3*86400)        // not swept yet, grace running
	ent("pastgrace", EntActive, 4*24*time.Hour, 3*86400) // not swept yet, grace over
	ent("nograce", EntActive, time.Hour, 0)
	ent("trialnograce", EntTrial, time.Hour, 3*86400) // trials have no grace
	h := e.mux(t)
	ut, _ := e.user.NewAuthToken()
	for key, want := range map[string]int{"ingrace": 1, "pastgrace": 0, "nograce": 0, "trialnograce": 0} {
		rec := do(h, "GET", "/api/collections/"+key+"/records", "", ut)
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", key, rec.Code, rec.Body)
		}
		if got := strings.Count(rec.Body.String(), `"title"`); got != want {
			t.Fatalf("%s: rule returned %d rows, want %d (%s)", key, got, want, rec.Body)
		}
		if go_ := e.m.Entitled(e.user.Id, "members", key); go_ != (want == 1) {
			t.Fatalf("%s: Entitled() and the rule function disagree", key)
		}
	}
}

// ---- Pm11 / Pm12 / Pm14: ingestion and API limits --------------------------------------

func TestWebhookFloodIsBounded(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	body := []byte(strings.Repeat("A", 5000))
	invalid := func() []*core.Record {
		rs, _ := e.app.FindRecordsByFilter(EventsCollection, "status='invalid'", "", 0, 0)
		return rs
	}
	for i := 0; i < 40; i++ {
		if _, err := e.m.IngestFrom(ctx, "9.9.9.9", "fake", map[string]string{"x-sig": "bad", "x-secret": "s3cret"}, body); !errors.Is(err, ErrInvalidSignature) {
			t.Fatal(err)
		}
	}
	rs := invalid()
	if len(rs) != 20 {
		t.Fatalf("at most 20 traces per source and hour, got %d", len(rs))
	}
	raw := rs[0].GetString("raw")
	if !strings.HasPrefix(raw, "sha256:") || strings.Contains(raw, "AAAA") || strings.Contains(rs[0].GetString("headers"), "s3cret") {
		t.Fatalf("only a hash and header names are kept: %q %s", raw, rs[0].GetString("headers"))
	}
	if _, err := e.m.IngestFrom(ctx, "8.8.8.8", "fake", map[string]string{"x-sig": "bad"}, body); err == nil || len(invalid()) != 21 {
		t.Fatal("another source has its own allowance")
	}
	e.now = e.now.Add(25 * time.Hour)
	rep, _ := e.m.Reconcile(ctx)
	if rep.Pruned != 21 || len(invalid()) != 0 {
		t.Fatalf("invalid traces are pruned after a day in one statement: %+v", rep)
	}
}

func TestWebhookHTTPLimits(t *testing.T) {
	e := setup(t)
	h := e.mux(t)
	if rec := do(h, "POST", "/api/payments/webhook/fake", strings.Repeat("x", 70<<10), "", "X-Sig", "good"); rec.Code != 413 {
		t.Fatalf("body over 64 KiB: %d", rec.Code)
	}
	n := func() int {
		c, _ := e.app.CountRecords(EventsCollection)
		return int(c)
	}
	before := n()
	e.prov.verifyErr = fmt.Errorf("%w: provider api down", ErrVerifyUnavailable)
	if rec := do(h, "POST", "/api/payments/webhook/fake", `[]`, "", "X-Sig", "good"); rec.Code != 503 {
		t.Fatalf("verification outage is a server error: %d", rec.Code)
	}
	if n() != before {
		t.Fatal("an outage is not a verdict and stores nothing")
	}
	e.prov.verifyErr = nil
	if rec := do(h, "POST", "/api/payments/webhook/fake", `[]`, "", "X-Sig", "bad"); rec.Code != 401 {
		t.Fatalf("bad signature: %d", rec.Code)
	}
}

func TestIntentBrakes(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	mk := func(who *core.Record) error {
		_, _, err := e.m.CreateIntent(ctx, CreateParams{Provider: "fake", Amount: 100, Currency: "IDR", Customer: who})
		return err
	}
	e.m.MaxOpenIntents = 3
	for i := 0; i < 3; i++ {
		if err := mk(e.user); err != nil {
			t.Fatal(err)
		}
	}
	if err := mk(e.user); !errors.Is(err, ErrTooManyIntents) {
		t.Fatalf("open intent cap: %v", err)
	}
	if err := mk(e.other); err != nil {
		t.Fatalf("the cap is per customer: %v", err)
	}
	h := e.mux(t)
	ut, _ := e.user.NewAuthToken()
	if rec := do(h, "POST", "/api/payments/intents", `{"provider":"fake","currency":"IDR","amount":100}`, ut); rec.Code != 429 {
		t.Fatalf("HTTP status of the cap: %d", rec.Code)
	}

	e.m.MaxOpenIntents, e.m.IntentPerMinute = 100, 2
	if err := mk(e.other); err != nil { // other already has 1 in the last minute
		t.Fatal(err)
	}
	if err := mk(e.other); !errors.Is(err, ErrTooManyIntents) {
		t.Fatalf("per-minute cap: %v", err)
	}
}

// ---- Pm19 and friends --------------------------------------------------------------

func TestSmallFixes(t *testing.T) {
	e := setup(t)
	if _, err := e.m.GrantEntitlement(e.user.Id, "members", Grant{Key: "pro", Days: -5}, EntActive); !errors.Is(err, ErrInvalid) {
		t.Fatalf("negative days: %v", err)
	}
	if _, err := e.m.GrantEntitlement(e.user.Id, "members", Grant{Key: "pro", GraceDays: -1}, EntActive); !errors.Is(err, ErrInvalid) {
		t.Fatalf("negative grace: %v", err)
	}
	for _, s := range []string{"12abc", "1.2.3", "", "-", "1e3", "12.5x"} {
		if _, err := ParseAmount(s, "USD"); err == nil {
			t.Errorf("ParseAmount(%q) must fail", s)
		}
	}
	if v, err := ParseAmount("-5.50", "USD"); err != nil || v != -550 {
		t.Errorf("negative amounts: %d %v", v, err)
	}
	if FormatAmount(500, "HUF") != "500" || MinorExponent("TWD") != 0 {
		t.Error("PayPal has no decimals for HUF and TWD")
	}

	// a manual revoke also expires the subscription
	in, _, err := e.m.CreateIntent(context.Background(), CreateParams{Provider: "fake", Amount: 100, Currency: "IDR", Customer: e.user,
		Grant: &Grant{Key: "sub", Days: 30, Subscription: true}})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = e.m.Transition(in.Id, StatusPaid, TransitionInfo{})
	sub, _ := e.app.FindFirstRecordByFilter(SubscriptionsCollection, "product='sub'")
	if sub == nil || sub.GetString("status") != "active" {
		t.Fatal("subscription active")
	}
	if ok, err := e.m.RevokeEntitlement(e.user.Id, "members", "sub"); !ok || err != nil {
		t.Fatal(err)
	}
	sub, _ = e.app.FindRecordById(SubscriptionsCollection, sub.Id)
	if sub.GetString("status") != "expired" {
		t.Fatalf("revoke must sync the subscription: %s", sub.GetString("status"))
	}
	// a refund event without an amount means "the whole payment", not a 0 row
	in2 := e.newIntentFor2(t)
	_, _ = e.m.Transition(in2.Id, StatusPaid, TransitionInfo{})
	_, _ = e.deliver(t, "good", WebhookEvent{EventID: "r0", Type: EventRefunded, ProviderRef: e.ref(in2), Currency: "IDR", Data: map[string]any{"refund_ref": "Rz"}})
	e.q.run(t, e.app)
	if status(t, e, in2.Id) != StatusRefunded {
		t.Fatal("amount 0 = whole payment")
	}
}

func (e *env) newIntentFor2(t *testing.T) *core.Record {
	t.Helper()
	r, _, err := e.m.CreateIntent(context.Background(), CreateParams{Provider: "fake", Amount: 700, Currency: "IDR", Customer: e.other})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestUpgradeCollectionAddsMissingPieces(t *testing.T) {
	have := core.NewBaseCollection("x")
	have.Fields.Add(&core.SelectField{Name: "status", Values: []string{"a"}, MaxSelect: 1})
	want := core.NewBaseCollection("x")
	want.Fields.Add(&core.SelectField{Name: "status", Values: []string{"a", "b"}, MaxSelect: 1}, &core.TextField{Name: "extra"})
	want.AddIndex("idx_x_extra", false, "[[extra]]", "")
	if !upgradeCollection(have, want) {
		t.Fatal("must report a change")
	}
	if have.Fields.GetByName("extra") == nil || len(have.Fields.GetByName("status").(*core.SelectField).Values) != 2 || have.GetIndex("idx_x_extra") == "" {
		t.Fatal("field, select value and index must be added")
	}
	if upgradeCollection(have, want) {
		t.Fatal("second run is a no-op")
	}
}
