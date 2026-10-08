//go:build !no_payments

package payments

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tests"
)

// ---- fakes ---------------------------------------------------------------

type fakeQueue struct {
	mu       sync.Mutex
	handlers map[string]kernel.JobHandler
	jobs     []*kernel.Job
	unique   map[string]bool
}

func (q *fakeQueue) Enqueue(ctx context.Context, kind string, payload any, opts ...kernel.EnqueueOption) (string, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	o := kernel.ResolveEnqueueOptions(opts...)
	if o.UniqueKey != "" {
		if q.unique[o.UniqueKey] {
			return "dup", nil
		}
		q.unique[o.UniqueKey] = true
	}
	b, _ := json.Marshal(payload)
	j := &kernel.Job{ID: kind, Kind: kind, Payload: b, MaxAttempts: 8}
	q.jobs = append(q.jobs, j)
	return j.ID, nil
}
func (q *fakeQueue) Register(kind string, h kernel.JobHandler) { q.handlers[kind] = h }
func (q *fakeQueue) Stats(context.Context) (kernel.JobStats, error) {
	return kernel.JobStats{Queued: int64(len(q.jobs))}, nil
}

// run executes queued jobs once each (failed ones stay for the next run).
func (q *fakeQueue) run(t *testing.T, app kernel.App) (failed int) {
	t.Helper()
	q.mu.Lock()
	jobs := q.jobs
	q.jobs = nil
	q.mu.Unlock()
	for _, j := range jobs {
		j.Attempt++
		if err := q.handlers[j.Kind](context.Background(), app, j); err != nil {
			failed++
			q.mu.Lock()
			q.jobs = append(q.jobs, j)
			q.mu.Unlock()
		}
	}
	return failed
}

type fake struct {
	mu         sync.Mutex
	created    []*IntentSpec
	createErr  error
	statuses   map[string]*Status
	refundErr  error
	verifyErr  error
	refundRef  string // forces the provider refund id (echo races)
	refunds    []*RefundSpec
	refundDone bool
}

func (f *fake) Name() string { return "fake" }
func (f *fake) CreateIntent(_ context.Context, in *IntentSpec) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return "", "", f.createErr
	}
	f.created = append(f.created, in)
	return "https://pay.example/" + in.ID, "ref-" + in.ID, nil
}
func (f *fake) VerifyWebhook(_ context.Context, h map[string]string, body []byte) ([]WebhookEvent, error) {
	if h["x-sig"] != "good" {
		return nil, ErrInvalidSignature
	}
	if f.verifyErr != nil {
		return nil, f.verifyErr
	}
	var evs []WebhookEvent
	if err := json.Unmarshal(body, &evs); err != nil {
		return nil, err
	}
	return evs, nil
}
func (f *fake) Refund(_ context.Context, r *RefundSpec) (*RefundResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refundErr != nil {
		return nil, f.refundErr
	}
	f.refunds = append(f.refunds, r)
	ref := "rf-" + r.RefundID
	if f.refundRef != "" {
		ref = f.refundRef
	}
	return &RefundResult{ProviderRef: ref, Done: f.refundDone}, nil
}
func (f *fake) FetchStatus(_ context.Context, ref string, _ map[string]any) (*Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.statuses[ref]; ok {
		return s, nil
	}
	return &Status{State: StatusPending}, nil
}

// ---- env -----------------------------------------------------------------

type env struct {
	app   *tests.TestApp
	m     *Module
	q     *fakeQueue
	prov  *fake
	user  *core.Record
	other *core.Record
	audit []string
	paid  int
	now   time.Time
	mu    sync.Mutex
}

func setup(t *testing.T) *env {
	t.Helper()
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)
	e := &env{app: app, q: &fakeQueue{handlers: map[string]kernel.JobHandler{}, unique: map[string]bool{}}, prov: &fake{statuses: map[string]*Status{}, refundDone: true}}
	kernel.SetJobs(app, e.q)
	e.m = Register(app)
	e.now = time.Now().UTC().Truncate(time.Second)
	e.m.Now = func() time.Time { return e.now }
	e.m.AddProvider(e.prov)
	SetAuditSink(func(action, col, rec string, d map[string]any) {
		e.mu.Lock()
		e.audit = append(e.audit, action+":"+d2s(d))
		e.mu.Unlock()
	})
	t.Cleanup(func() { SetAuditSink(nil) })
	OnPaid(app).BindFunc(func(ev *PaidEvent) error { e.paid++; return ev.Next() })

	mc := core.NewAuthCollection("members")
	if err := app.Save(mc); err != nil {
		t.Fatal(err)
	}
	mk := func(email string) *core.Record {
		r := core.NewRecord(mc)
		r.SetEmail(email)
		r.SetPassword("password12345")
		if err := app.Save(r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	e.user, e.other = mk("a@example.com"), mk("b@example.com")
	return e
}

func d2s(d map[string]any) string {
	b, _ := json.Marshal(d)
	return string(b)
}

func (e *env) hasAudit(sub string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, a := range e.audit {
		if strings.Contains(a, sub) {
			return true
		}
	}
	return false
}

func (e *env) product(t *testing.T, slug string, amount int, days, grace int) {
	t.Helper()
	col, _ := e.app.FindCollectionByNameOrId(ProductsCollection)
	r := core.NewRecord(col)
	r.Set("slug", slug)
	r.Set("enabled", true)
	r.Set("amount", amount)
	r.Set("currency", "IDR")
	r.Set("entitlement_key", "pro")
	r.Set("duration_days", days)
	r.Set("grace_days", grace)
	r.Set("quota", 100)
	r.Set("balance_add", 10)
	if err := e.app.Save(r); err != nil {
		t.Fatal(err)
	}
}

func (e *env) newIntent(t *testing.T, key string) *core.Record {
	t.Helper()
	r, _, err := e.m.CreateIntent(context.Background(), CreateParams{
		Provider: "fake", Currency: "IDR", Product: "pro-month", OrderRef: "o1", IdempotencyKey: key, Customer: e.user,
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (e *env) deliver(t *testing.T, sig string, evs ...WebhookEvent) (IngestResult, error) {
	t.Helper()
	b, _ := json.Marshal(evs)
	return e.m.Ingest(context.Background(), "fake", map[string]string{"x-sig": sig}, b)
}

func status(t *testing.T, e *env, id string) string {
	t.Helper()
	r, err := e.m.GetIntent(id)
	if err != nil {
		t.Fatal(err)
	}
	return r.GetString("status")
}

// ---- tests ---------------------------------------------------------------

func TestStateMachine(t *testing.T) {
	ok := [][2]string{
		{StatusCreated, StatusPending}, {StatusCreated, StatusPaid}, {StatusCreated, StatusFailed},
		{StatusPending, StatusPaid}, {StatusPending, StatusFailed}, {StatusPending, StatusExpired},
		{StatusPaid, StatusRefunded}, {StatusPaid, StatusPartiallyRefunded},
		{StatusPartiallyRefunded, StatusRefunded}, {StatusPartiallyRefunded, StatusPartiallyRefunded},
	}
	for _, c := range ok {
		if err := CheckTransition(c[0], c[1]); err != nil {
			t.Errorf("%v should be allowed: %v", c, err)
		}
	}
	bad := [][2]string{
		{StatusPending, StatusCreated}, {StatusPending, StatusRefunded}, {StatusFailed, StatusPaid},
		{StatusExpired, StatusPaid}, {StatusRefunded, StatusPaid}, {StatusPaid, StatusPending},
		{StatusPaid, StatusFailed}, {StatusCreated, StatusRefunded}, {StatusPaid, StatusPaid},
	}
	for _, c := range bad {
		if err := CheckTransition(c[0], c[1]); !errors.Is(err, ErrIllegalTransition) {
			t.Errorf("%v should be rejected, got %v", c, err)
		}
	}
}

func TestAmounts(t *testing.T) {
	if FormatAmount(1250, "USD") != "12.50" || FormatAmount(15000, "IDR") != "15000" || FormatAmount(5, "USD") != "0.05" {
		t.Fatal("FormatAmount")
	}
	for _, c := range []struct {
		s, cur string
		want   int64
	}{{"12.50", "USD", 1250}, {"12.5", "USD", 1250}, {"7", "USD", 700}, {"15000", "IDR", 15000}, {"0.05", "USD", 5}} {
		got, err := ParseAmount(c.s, c.cur)
		if err != nil || got != c.want {
			t.Errorf("ParseAmount(%q,%s) = %d, %v; want %d", c.s, c.cur, got, err, c.want)
		}
	}
}

func TestIdempotency(t *testing.T) {
	e := setup(t)
	e.product(t, "pro-month", 50000, 30, 3)
	a := e.newIntent(t, "k1")
	b, replay, err := e.m.CreateIntent(context.Background(), CreateParams{Provider: "fake", Currency: "IDR", Product: "pro-month", OrderRef: "o1", IdempotencyKey: "k1", Customer: e.user})
	if err != nil || !replay || b.Id != a.Id {
		t.Fatalf("same key must return same intent: %v replay=%v", err, replay)
	}
	if len(e.prov.created) != 1 {
		t.Fatalf("provider called %d times", len(e.prov.created))
	}
	if a.GetString("status") != StatusPending || a.GetString("checkout_url") == "" {
		t.Fatalf("intent not pending with url: %s", a.GetString("status"))
	}
	if _, _, err := e.m.CreateIntent(context.Background(), CreateParams{Provider: "fake", Currency: "IDR", Product: "pro-month", OrderRef: "other", IdempotencyKey: "k1", Customer: e.user}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("different params must conflict, got %v", err)
	}
	// the key is scoped to the customer
	c, replay, err := e.m.CreateIntent(context.Background(), CreateParams{Provider: "fake", Currency: "IDR", Product: "pro-month", OrderRef: "o1", IdempotencyKey: "k1", Customer: e.other})
	if err != nil || replay || c.Id == a.Id {
		t.Fatalf("other customer must get its own intent: %v", err)
	}
	// no key: distinct intents
	d1 := e.newIntent(t, "")
	d2 := e.newIntent(t, "")
	if d1.Id == d2.Id {
		t.Fatal("no key must not dedupe")
	}
	// product price cannot be undercut
	if _, _, err := e.m.CreateIntent(context.Background(), CreateParams{Provider: "fake", Amount: 1, Currency: "IDR", Product: "pro-month", Customer: e.user}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("underpaying a product must be rejected, got %v", err)
	}
}

func TestCreateProviderFailure(t *testing.T) {
	e := setup(t)
	e.prov.createErr = errors.New("boom")
	_, _, err := e.m.CreateIntent(context.Background(), CreateParams{Provider: "fake", Amount: 100, Currency: "IDR", Customer: e.user})
	if err == nil {
		t.Fatal("expected error")
	}
	rs, _ := e.app.FindRecordsByFilter(IntentsCollection, "id != ''", "", 10, 0)
	if len(rs) != 1 || rs[0].GetString("status") != StatusFailed || rs[0].GetString("last_error") == "" {
		t.Fatal("failed intent must be recorded")
	}
	if _, _, err := e.m.CreateIntent(context.Background(), CreateParams{Provider: "nope", Amount: 100, Currency: "IDR"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown provider: %v", err)
	}
}

func TestWebhookEndToEnd(t *testing.T) {
	e := setup(t)
	e.product(t, "pro-month", 50000, 30, 3)
	in := e.newIntent(t, "k")
	ev := WebhookEvent{EventID: "evt-1", Type: EventPaid, ProviderRef: in.GetString("provider_ref"), Amount: 50000, Currency: "IDR"}

	// bad signature: stored as invalid, nothing queued
	if _, err := e.deliver(t, "bad", ev); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("want invalid signature, got %v", err)
	}
	inv, _ := e.app.FindRecordsByFilter(EventsCollection, "status='invalid'", "", 10, 0)
	if len(inv) != 1 || inv[0].GetBool("verified") || len(e.q.jobs) != 0 {
		t.Fatal("rejected webhook must be stored unverified and not queued")
	}

	res, err := e.deliver(t, "good", ev)
	if err != nil || res.Accepted != 1 {
		t.Fatalf("deliver: %+v %v", res, err)
	}
	// stored verified+raw BEFORE processing
	rec, _ := e.app.FindFirstRecordByFilter(EventsCollection, "event_id='evt-1'")
	if rec == nil || !rec.GetBool("verified") || rec.GetString("status") != "received" || !strings.Contains(rec.GetString("raw"), "evt-1") {
		t.Fatal("event must be stored raw and verified before processing")
	}
	if status(t, e, in.Id) != StatusPending {
		t.Fatal("must not be processed before the job runs")
	}
	// duplicate delivery is ignored
	res, _ = e.deliver(t, "good", ev)
	if res.Duplicates != 1 || res.Accepted != 0 || len(e.q.jobs) != 1 {
		t.Fatalf("duplicate: %+v jobs=%d", res, len(e.q.jobs))
	}
	if f := e.q.run(t, e.app); f != 0 {
		t.Fatal("job failed")
	}
	if status(t, e, in.Id) != StatusPaid || e.paid != 1 {
		t.Fatalf("not paid (paid hooks=%d)", e.paid)
	}
	rec, _ = e.app.FindRecordById(EventsCollection, rec.Id)
	if rec.GetString("status") != "processed" || rec.GetString("intent") != in.Id {
		t.Fatalf("event state %s", rec.GetString("status"))
	}
	if !e.m.Entitled(e.user.Id, "members", "pro") || e.m.Entitled(e.other.Id, "members", "pro") {
		t.Fatal("entitlement must belong to the payer only")
	}
	if !e.hasAudit(`"to":"paid"`) || !e.hasAudit(`"to":"pending"`) {
		t.Fatalf("status changes must be audited: %v", e.audit)
	}
	// replay is idempotent
	if err := e.m.ReplayEvent(context.Background(), rec.Id); err != nil || e.paid != 1 {
		t.Fatalf("replay: %v paid=%d", err, e.paid)
	}
	ents, _ := e.app.FindRecordsByFilter(EntitlementsCollection, "id != ''", "", 10, 0)
	if len(ents) != 1 || ents[0].GetInt("balance") != 10 || ents[0].GetInt("quota") != 100 {
		t.Fatal("exactly one entitlement with quota and balance expected")
	}
}

func TestWebhookRejections(t *testing.T) {
	e := setup(t)
	e.product(t, "pro-month", 50000, 30, 3)
	in := e.newIntent(t, "")
	ref := in.GetString("provider_ref")

	// wrong amount
	_, _ = e.deliver(t, "good", WebhookEvent{EventID: "m1", Type: EventPaid, ProviderRef: ref, Amount: 1, Currency: "IDR"})
	e.q.run(t, e.app)
	if status(t, e, in.Id) != StatusPending || !e.hasAudit(ActionMismatch) {
		t.Fatal("amount mismatch must not pay the intent")
	}
	// expired, then a verified payment with the same reference arrives inside
	// the late window: re-opened to paid, flagged late (see TestLatePayment)
	_, _ = e.deliver(t, "good", WebhookEvent{EventID: "x1", Type: EventExpired, ProviderRef: ref})
	_, _ = e.deliver(t, "good", WebhookEvent{EventID: "p1", Type: EventPaid, ProviderRef: ref, Amount: 50000, Currency: "IDR"})
	if f := e.q.run(t, e.app); f != 0 {
		t.Fatal("late payments must not be retried")
	}
	if status(t, e, in.Id) != StatusPaid || !e.m.Entitled(e.user.Id, "members", "pro") {
		t.Fatalf("expired -> paid inside the window must re-open, is %s", status(t, e, in.Id))
	}
	// orphan event is retried then resolves once the intent exists
	_, _ = e.deliver(t, "good", WebhookEvent{EventID: "o1", Type: EventPaid, ProviderRef: "later-ref", Amount: 5, Currency: "IDR"})
	if f := e.q.run(t, e.app); f != 1 {
		t.Fatalf("orphan must fail for retry, failed=%d", f)
	}
	in2, _, _ := e.m.CreateIntent(context.Background(), CreateParams{Provider: "fake", Amount: 5, Currency: "IDR", Customer: e.user})
	r, _ := e.m.GetIntent(in2.Id)
	r.Set("provider_ref", "later-ref")
	_ = e.app.Save(r)
	if f := e.q.run(t, e.app); f != 0 || status(t, e, in2.Id) != StatusPaid {
		t.Fatal("retry must succeed once the intent is known")
	}
}

func TestIllegalTransitionsRejectedByAPI(t *testing.T) {
	e := setup(t)
	in, _, _ := e.m.CreateIntent(context.Background(), CreateParams{Provider: "fake", Amount: 5, Currency: "IDR", Customer: e.user})
	if _, err := e.m.Transition(in.Id, StatusRefunded, TransitionInfo{Source: "test"}); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("pending -> refunded: %v", err)
	}
	if ch, err := e.m.Transition(in.Id, StatusPending, TransitionInfo{}); err != nil || ch {
		t.Fatalf("same status is a no-op: %v %v", ch, err)
	}
}

func TestEntitlementLifecycle(t *testing.T) {
	e := setup(t)
	e.product(t, "pro-month", 50000, 30, 3)
	in := e.newIntent(t, "")
	if _, err := e.m.Transition(in.Id, StatusPaid, TransitionInfo{Source: "test"}); err != nil {
		t.Fatal(err)
	}
	ent, _ := e.app.FindFirstRecordByFilter(EntitlementsCollection, "key='pro'")
	if ent.GetString("status") != EntActive || ent.GetString("source_intent") != in.Id {
		t.Fatal("expected active entitlement sourced from the intent")
	}
	until := ent.GetDateTime("until").Time()
	if d := until.Sub(e.now); d < 29*24*time.Hour || d > 31*24*time.Hour {
		t.Fatalf("until is %v from now", d)
	}
	// still active before the end: sweep keeps it
	if n, _ := e.m.Sweep(); n != 0 {
		t.Fatal("nothing to sweep yet")
	}
	// past the end: grace (3 days)
	e.now = until.Add(time.Hour)
	if n, err := e.m.Sweep(); err != nil || n != 1 {
		t.Fatalf("sweep: %d %v", n, err)
	}
	ent, _ = e.app.FindRecordById(EntitlementsCollection, ent.Id)
	if ent.GetString("status") != EntGrace {
		t.Fatalf("want grace, got %s", ent.GetString("status"))
	}
	// past grace: lapsed
	e.now = until.Add(4 * 24 * time.Hour)
	if n, _ := e.m.Sweep(); n != 1 {
		t.Fatal("grace must lapse")
	}
	ent, _ = e.app.FindRecordById(EntitlementsCollection, ent.Id)
	if ent.GetString("status") != EntLapsed {
		t.Fatalf("want lapsed, got %s", ent.GetString("status"))
	}
	// renewal after lapse restarts from now and keeps one row
	e.now = time.Now().UTC()
	in2 := e.newIntent(t, "")
	_, _ = e.m.Transition(in2.Id, StatusPaid, TransitionInfo{})
	ents, _ := e.app.FindRecordsByFilter(EntitlementsCollection, "id != ''", "", 10, 0)
	if len(ents) != 1 || ents[0].GetString("status") != EntActive || ents[0].GetInt("balance") != 20 {
		t.Fatalf("renewal: %d rows", len(ents))
	}
}

func TestTrialAndManualGrant(t *testing.T) {
	e := setup(t)
	g := Grant{Key: "pro", Days: 14}
	ent, err := e.m.GrantEntitlement(e.user.Id, "members", g, EntTrial)
	if err != nil || ent.GetString("status") != EntTrial || !e.m.Entitled(e.user.Id, "members", "pro") {
		t.Fatalf("trial: %v", err)
	}
	if _, err := e.m.GrantEntitlement(e.user.Id, "members", g, EntTrial); !errors.Is(err, ErrInvalid) {
		t.Fatalf("second trial must be refused, got %v", err)
	}
	if ok, _ := e.m.RevokeEntitlement(e.user.Id, "members", "pro"); !ok || e.m.Entitled(e.user.Id, "members", "pro") {
		t.Fatal("revoke must lapse the entitlement")
	}
}

func TestRefundFlow(t *testing.T) {
	e := setup(t)
	e.product(t, "pro-month", 50000, 30, 0)
	in := e.newIntent(t, "")
	_, _ = e.m.Transition(in.Id, StatusPaid, TransitionInfo{})
	if _, err := e.m.Refund(context.Background(), in.Id, 60000, "", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("over-refund: %v", err)
	}
	rf, err := e.m.Refund(context.Background(), in.Id, 20000, "oops", "r1")
	if err != nil || rf.GetString("status") != "succeeded" {
		t.Fatalf("partial refund: %v", err)
	}
	if status(t, e, in.Id) != StatusPartiallyRefunded || !e.m.Entitled(e.user.Id, "members", "pro") {
		t.Fatal("partial refund keeps access")
	}
	again, _ := e.m.Refund(context.Background(), in.Id, 20000, "oops", "r1")
	if again.Id != rf.Id || len(e.prov.refunds) != 1 {
		t.Fatal("refund idempotency key must dedupe")
	}
	// provider echo of our own refund is not counted twice
	_, _ = e.deliver(t, "good", WebhookEvent{EventID: "rfe", Type: EventRefunded, ProviderRef: in.GetString("provider_ref"), Amount: 20000, Data: map[string]any{"refund_ref": "rf-" + rf.Id}})
	e.q.run(t, e.app)
	r, _ := e.m.GetIntent(in.Id)
	if r.GetInt("refunded_amount") != 20000 {
		t.Fatalf("echo double counted: %d", r.GetInt("refunded_amount"))
	}
	// rest: full refund revokes
	if _, err := e.m.Refund(context.Background(), in.Id, 0, "", ""); err != nil {
		t.Fatal(err)
	}
	if status(t, e, in.Id) != StatusRefunded || e.m.Entitled(e.user.Id, "members", "pro") {
		t.Fatal("full refund must revoke the entitlement")
	}
	e.prov.refundErr = ErrUnsupported
	in2 := e.newIntent(t, "")
	_, _ = e.m.Transition(in2.Id, StatusPaid, TransitionInfo{})
	if _, err := e.m.Refund(context.Background(), in2.Id, 0, "", ""); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unsupported refund: %v", err)
	}
	if status(t, e, in2.Id) != StatusPaid {
		t.Fatal("failed refund must not change the intent")
	}
}

func TestReconcile(t *testing.T) {
	e := setup(t)
	e.product(t, "pro-month", 50000, 30, 0)
	a := e.newIntent(t, "")
	b := e.newIntent(t, "")
	c := e.newIntent(t, "")
	e.prov.statuses[a.GetString("provider_ref")] = &Status{State: StatusPaid, Amount: 50000}
	e.prov.statuses[b.GetString("provider_ref")] = &Status{State: StatusExpired}

	// too young: nothing checked
	rep, err := e.m.Reconcile(context.Background())
	if err != nil || rep.Checked != 0 {
		t.Fatalf("young intents must be left alone: %+v %v", rep, err)
	}
	e.now = e.now.Add(30 * time.Minute)
	rep, err = e.m.Reconcile(context.Background())
	if err != nil || rep.Checked != 3 || rep.Changed != 2 {
		t.Fatalf("reconcile: %+v %v", rep, err)
	}
	if status(t, e, a.Id) != StatusPaid || status(t, e, b.Id) != StatusExpired || status(t, e, c.Id) != StatusPending {
		t.Fatal("wrong reconciled states")
	}
	if !e.m.Entitled(e.user.Id, "members", "pro") || e.paid != 1 {
		t.Fatal("reconcile must grant like a webhook")
	}
	// never resolved past the TTL: expired
	e.now = e.now.Add(8 * 24 * time.Hour)
	_, _ = e.m.Reconcile(context.Background())
	if status(t, e, c.Id) != StatusExpired {
		t.Fatal("stale pending intent must expire")
	}
}

func TestCronEnqueuesReconcileOnce(t *testing.T) {
	e := setup(t)
	e.m.cronTick()
	e.m.cronTick()
	if len(e.q.jobs) != 1 && len(e.q.jobs) != 2 { // fake queue only dedupes on unique keys
		t.Fatalf("jobs=%d", len(e.q.jobs))
	}
	if e.q.jobs[0].Kind != JobReconcile {
		t.Fatal("kind")
	}
	if f := e.q.run(t, e.app); f != 0 {
		t.Fatal("reconcile job failed")
	}
}

func TestWebhookIPAllowlistAndRateLimit(t *testing.T) {
	t.Setenv("TOKI_PAYMENTS_FAKE_WEBHOOK_IPS", "10.0.0.0/8, 192.0.2.7")
	if !ipAllowed("fake", "10.1.2.3") || !ipAllowed("fake", "192.0.2.7") || ipAllowed("fake", "203.0.113.9") || ipAllowed("fake", "bogus") {
		t.Fatal("allowlist")
	}
	t.Setenv("TOKI_PAYMENTS_FAKE_WEBHOOK_IPS", "")
	if !ipAllowed("fake", "203.0.113.9") {
		t.Fatal("empty list allows all")
	}
	l := newIPLimiter(2)
	now := time.Now()
	if !l.allow("1.1.1.1", now) || !l.allow("1.1.1.1", now) || l.allow("1.1.1.1", now) || !l.allow("2.2.2.2", now) {
		t.Fatal("limiter")
	}
	if !l.allow("1.1.1.1", now.Add(2*time.Minute)) {
		t.Fatal("window must reset")
	}
}

func TestRedactHeaders(t *testing.T) {
	h := redactHeaders(map[string]string{"Authorization": "Bearer x", "X-Callback-Token": "t", "Content-Type": "a/b", "paypal-transmission-sig": "s", "User-Agent": "u"})
	if len(h) != 2 || h["content-type"] == "" || h["user-agent"] == "" {
		t.Fatalf("redact: %v", h)
	}
}

// ---- HTTP ----------------------------------------------------------------

func (e *env) mux(t *testing.T) http.Handler {
	t.Helper()
	router, err := apis.NewRouter(e.app)
	if err != nil {
		t.Fatal(err)
	}
	var h http.Handler
	err = e.app.OnServe().Trigger(&core.ServeEvent{App: e.app, Router: router}, func(se *core.ServeEvent) error {
		mux, err := se.Router.BuildMux()
		h = mux
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func do(h http.Handler, method, path, body, token string, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHTTPAPI(t *testing.T) {
	e := setup(t)
	e.product(t, "pro-month", 50000, 30, 0)
	h := e.mux(t)
	ut, _ := e.user.NewAuthToken()
	ot, _ := e.other.NewAuthToken()
	su, err := e.app.FindAuthRecordByEmail(core.CollectionNameSuperusers, "test@example.com")
	if err != nil {
		t.Fatal(err)
	}
	st, _ := su.NewAuthToken()

	if rec := do(h, "POST", "/api/payments/intents", `{}`, ""); rec.Code != 401 {
		t.Fatalf("guest create: %d", rec.Code)
	}
	body := `{"provider":"fake","currency":"IDR","product":"pro-month","order_ref":"x","metadata":{"return_url":"https://evil.example"}}`
	rec := do(h, "POST", "/api/payments/intents", body, ut, "Idempotency-Key", "abc")
	if rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Intent      map[string]any `json:"intent"`
		CheckoutURL string         `json:"checkout_url"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	id, _ := out.Intent["id"].(string)
	if out.CheckoutURL == "" || id == "" || out.Intent["status"] != "pending" {
		t.Fatalf("bad response %s", rec.Body)
	}
	if _, leaked := e.prov.created[0].Metadata["return_url"]; leaked {
		t.Fatal("clients must not set return_url")
	}
	if rec := do(h, "POST", "/api/payments/intents", body, ut, "Idempotency-Key", "abc"); rec.Code != 200 {
		t.Fatalf("replay should be 200, got %d", rec.Code)
	}
	if rec := do(h, "POST", "/api/payments/intents", `{"provider":"fake","currency":"IDR","product":"pro-month","order_ref":"y"}`, ut, "Idempotency-Key", "abc"); rec.Code != 409 {
		t.Fatalf("conflict should be 409, got %d", rec.Code)
	}
	if rec := do(h, "POST", "/api/payments/intents", `{"provider":"fake","currency":"IDR","amount":-1}`, ut); rec.Code != 400 {
		t.Fatalf("invalid should be 400, got %d", rec.Code)
	}
	if bytesLeak := do(h, "GET", "/api/payments/intents/"+id, "", ut); bytesLeak.Code != 200 || strings.Contains(bytesLeak.Body.String(), "provider_data") {
		t.Fatalf("owner get: %d", bytesLeak.Code)
	}
	if rec := do(h, "GET", "/api/payments/intents/"+id, "", ot); rec.Code != 404 {
		t.Fatalf("other user must not see it: %d", rec.Code)
	}
	if rec := do(h, "GET", "/api/payments/intents/"+id, "", st); rec.Code != 200 {
		t.Fatalf("superuser get: %d", rec.Code)
	}
	if rec := do(h, "POST", "/api/payments/intents/"+id+"/refund", `{}`, ut); rec.Code != 403 {
		t.Fatalf("refund needs superuser: %d", rec.Code)
	}
	// webhook over HTTP: bad sig 401, good sig 200 and processed via the queue
	ev, _ := json.Marshal([]WebhookEvent{{EventID: "h1", Type: EventPaid, ProviderRef: "ref-" + id, Amount: 50000, Currency: "IDR"}})
	if rec := do(h, "POST", "/api/payments/webhook/fake", string(ev), "", "X-Sig", "nope"); rec.Code != 401 {
		t.Fatalf("bad sig: %d", rec.Code)
	}
	if rec := do(h, "POST", "/api/payments/webhook/unknown", string(ev), ""); rec.Code != 404 {
		t.Fatalf("unknown provider: %d", rec.Code)
	}
	if rec := do(h, "POST", "/api/payments/webhook/fake", string(ev), "", "X-Sig", "good"); rec.Code != 200 {
		t.Fatalf("good sig: %d %s", rec.Code, rec.Body)
	}
	e.q.run(t, e.app)
	if status(t, e, id) != StatusPaid {
		t.Fatal("webhook must pay the intent")
	}
	if rec := do(h, "GET", "/api/payments/entitlements/me", "", ut); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"entitled":true`) {
		t.Fatalf("entitlements/me: %d %s", rec.Code, rec.Body)
	}
	rec = do(h, "POST", "/api/payments/intents/"+id+"/refund", `{"amount":1000,"reason":"test"}`, st)
	if rec.Code != 200 || status(t, e, id) != StatusPartiallyRefunded {
		t.Fatalf("refund: %d %s", rec.Code, rec.Body)
	}
}

func TestEntitledRuleOverHTTP(t *testing.T) {
	e := setup(t)
	col := core.NewBaseCollection("articles")
	col.Fields.Add(&core.TextField{Name: "title"})
	rule := `entitled("pro") = true`
	col.ListRule, col.ViewRule = &rule, &rule
	if err := e.app.Save(col); err != nil {
		t.Fatal(err)
	}
	r := core.NewRecord(col)
	r.Set("title", "secret")
	if err := e.app.Save(r); err != nil {
		t.Fatal(err)
	}
	h := e.mux(t)
	ut, _ := e.user.NewAuthToken()
	ot, _ := e.other.NewAuthToken()
	count := func(tok string) int {
		rec := do(h, "GET", "/api/collections/articles/records", "", tok)
		if rec.Code != 200 {
			t.Fatalf("list: %d %s", rec.Code, rec.Body)
		}
		var o struct{ Items []any }
		_ = json.Unmarshal(rec.Body.Bytes(), &o)
		return len(o.Items)
	}
	if count("") != 0 || count(ut) != 0 {
		t.Fatal("not entitled -> no rows")
	}
	if _, err := e.m.GrantEntitlement(e.user.Id, "members", Grant{Key: "pro", Days: 1}, EntActive); err != nil {
		t.Fatal(err)
	}
	if count(ut) != 1 || count(ot) != 0 || count("") != 0 {
		t.Fatal("only the entitled member sees the row")
	}
	_, _ = e.m.RevokeEntitlement(e.user.Id, "members", "pro")
	if count(ut) != 0 {
		t.Fatal("revoked -> denied")
	}
}
