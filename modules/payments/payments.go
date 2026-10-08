//go:build !no_payments

package payments

import (
	"context"
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/dbutils"
	"github.com/tokibase/tokibase/tools/hook"
	"github.com/tokibase/tokibase/tools/types"
)

// System collection names.
const (
	IntentsCollection       = "_payment_intents"
	EventsCollection        = "_payment_events"
	ProductsCollection      = "_payment_products"
	RefundsCollection       = "_refunds"
	SubscriptionsCollection = "_subscriptions"
	EntitlementsCollection  = "_entitlements"
	// GrantsCollection is the per-intent ledger of what each paid intent
	// added to an entitlement (days, balance), so a refund takes back exactly
	// that contribution.
	GrantsCollection = "_entitlement_grants"
)

const (
	hookId = "__tokiPayments__"

	// Job kinds.
	JobProcess   = "payments.process"
	JobReconcile = "payments.reconcile"

	reconcileCronKey = "__tokiPayments_reconcile"
	reconcileCron    = "*/15 * * * *"
)

// Audit actions.
const (
	ActionStatus      = "payments.status"
	ActionRejected    = "payments.rejected"
	ActionMismatch    = "payments.amount_mismatch"
	ActionEntitlement = "payments.entitlement"
	ActionRefund      = "payments.refund"
	ActionWebhookBad  = "payments.webhook_invalid"
	ActionLate        = "payments.late_payment" // money arrived for a failed/expired intent (details.level = error)
	ActionDead        = "payments.event_dead"   // an event gave up after MaxEventAttempts
)

// Enabled reports whether the module is on (env TOKI_PAYMENTS=off disables it).
func Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TOKI_PAYMENTS"))) {
	case "off", "false", "0", "disabled":
		return false
	}
	return true
}

var (
	sinkMu     sync.RWMutex
	globalSink func(action, collection, record string, details map[string]any)
)

// SetAuditSink connects status changes to an external audit log.
// Modules must not import each other, so the wiring happens in tokibase.go.
func SetAuditSink(fn func(action, collection, record string, details map[string]any)) {
	sinkMu.Lock()
	globalSink = fn
	sinkMu.Unlock()
}

func audit(action, collection, record string, details map[string]any) {
	sinkMu.RLock()
	fn := globalSink
	sinkMu.RUnlock()
	if fn != nil {
		fn(action, collection, record, details)
	}
}

// PaidEvent is delivered to OnPaid handlers after an intent became paid and
// its entitlements were granted (the transaction is committed).
type PaidEvent struct {
	hook.Event
	App    core.App
	Intent *core.Record
	// Late is true when the payment reached the intent after it had failed or
	// expired (re-opened by the framework or granted by an operator). The
	// consumer may want to treat the order differently (it may have been cancelled).
	Late bool
}

const (
	moduleStoreKey = "__tokiPaymentsModule__"
	paidHookKey    = "__tokiPaymentsOnPaid__"
	lateHookKey    = "__tokiPaymentsOnLate__"
)

var storeMu sync.Mutex

// OnPaid returns the hook triggered for every `payment.paid` transition, for
// Go consumers (and WASM/webhook bridges built on top). There is no generic
// custom-event path in modules/webhooks (it captures record events of user
// collections only), so consumers bind here; see docs/modules/payments.md.
//
// OnPaid is at-most-once: it fires in the process that committed the
// transition, after the commit. A crash between the commit and the handler, or
// a handler error (only logged), loses the notification; make fulfilment
// idempotent and able to find paid intents again (`toki payments intents list
// --status paid`).
func OnPaid(app core.App) *hook.Hook[*PaidEvent] { return paidHook(app, paidHookKey) }

// OnLatePayment is triggered when a verified payment reached an intent that
// had failed or expired and was too old to be re-opened (status paid_late).
// Nothing is granted yet; the operator decides. Fires after the commit, with
// the same at-most-once semantics as OnPaid.
func OnLatePayment(app core.App) *hook.Hook[*PaidEvent] { return paidHook(app, lateHookKey) }

func paidHook(app core.App, key string) *hook.Hook[*PaidEvent] {
	storeMu.Lock()
	defer storeMu.Unlock()
	if h, ok := app.Store().Get(key).(*hook.Hook[*PaidEvent]); ok {
		return h
	}
	h := &hook.Hook[*PaidEvent]{}
	app.Store().Set(key, h)
	return h
}

// Module holds the providers and tunables of one app.
type Module struct {
	app core.App
	Now func() time.Time

	mu        sync.RWMutex
	providers map[string]Provider

	rl *ipLimiter
	// bad caps how many failed webhook attempts per IP and hour are stored/audited.
	bad *hourCap

	// LateWindow: a verified payment for a failed/expired intent that changed
	// less than this long ago, with a matching provider reference, re-opens
	// the intent to paid; otherwise it becomes paid_late for an operator.
	LateWindow time.Duration
	// RefundPendingTTL: a pending API refund not confirmed after this long is marked failed (alert).
	RefundPendingTTL time.Duration
	// MaxEventAttempts: a webhook event that keeps failing is dead-lettered after this many tries.
	MaxEventAttempts int
	// IntentPerMinute / MaxOpenIntents: default per-customer brakes on CreateIntent
	// (they apply even when no `payments:intent` rate limit rule is configured).
	IntentPerMinute int
	MaxOpenIntents  int

	// ReconcileAfter: pending intents older than this are checked with the provider.
	ReconcileAfter time.Duration
	// IntentTTL: a pending intent the provider never resolves expires after this.
	IntentTTL time.Duration
}

// For returns the module registered for app (nil if payments is not registered).
func For(app core.App) *Module {
	m, _ := app.Store().Get(moduleStoreKey).(*Module)
	return m
}

func envInt(name string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name))); err == nil && n > 0 {
		return n
	}
	return def
}

// Register creates the collections, loads the providers configured in the
// environment, binds routes, job handlers and the reconciliation cron.
func Register(app core.App) *Module {
	m := &Module{
		app: app, Now: time.Now, providers: map[string]Provider{},
		ReconcileAfter:   time.Duration(envInt("TOKI_PAYMENTS_RECONCILE_MINUTES", 10)) * time.Minute,
		IntentTTL:        time.Duration(envInt("TOKI_PAYMENTS_INTENT_TTL_HOURS", 168)) * time.Hour,
		rl:               newIPLimiter(envInt("TOKI_PAYMENTS_WEBHOOK_RPM", 300)),
		bad:              newHourCap(envInt("TOKI_PAYMENTS_INVALID_STORE_PER_HOUR", 20)),
		LateWindow:       time.Duration(envInt("TOKI_PAYMENTS_LATE_WINDOW_HOURS", 24)) * time.Hour,
		RefundPendingTTL: 24 * time.Hour, MaxEventAttempts: envInt("TOKI_PAYMENTS_MAX_EVENT_ATTEMPTS", 12),
		IntentPerMinute: envInt("TOKI_PAYMENTS_INTENT_PER_MINUTE", 30),
		MaxOpenIntents:  envInt("TOKI_PAYMENTS_MAX_OPEN_INTENTS", 20),
	}
	app.Store().Set(moduleStoreKey, m)
	m.loadProviders()

	ensure := func() {
		if err := EnsureCollections(app); err != nil {
			app.Logger().Error("payments: failed to initialize collections", "error", err)
		}
	}
	if app.IsBootstrapped() {
		ensure()
	}
	app.OnBootstrap().Bind(&hook.Handler[*core.BootstrapEvent]{
		Id: hookId, Priority: -1,
		Func: func(e *core.BootstrapEvent) error {
			if err := e.Next(); err != nil {
				return err
			}
			ensure()
			return nil
		},
	})

	q := kernel.Jobs(app)
	q.Register(JobProcess, m.handleProcessJob)
	q.Register(JobReconcile, m.handleReconcileJob)

	app.OnServe().Bind(&hook.Handler[*core.ServeEvent]{
		Id: hookId,
		Func: func(se *core.ServeEvent) error {
			m.bindRoutes(se)
			_ = app.Cron().Add(reconcileCronKey, reconcileCron, m.cronTick)
			return se.Next()
		},
	})
	return m
}

func (m *Module) loadProviders() {
	for _, name := range factoryNames() {
		p, err := factory(name)(Getenv)
		if err != nil {
			m.app.Logger().Error("payments: provider "+name+" is misconfigured", "error", err)
			continue
		}
		if p != nil {
			m.AddProvider(p)
		}
	}
}

// AddProvider registers (or replaces) a provider adapter.
func (m *Module) AddProvider(p Provider) {
	m.mu.Lock()
	m.providers[strings.ToLower(p.Name())] = p
	m.mu.Unlock()
}

// Provider returns a configured provider.
func (m *Module) Provider(name string) (Provider, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.providers[strings.ToLower(name)]
	if !ok {
		return nil, ErrUnknownProvider
	}
	return p, nil
}

// ProviderNames lists the configured providers.
func (m *Module) ProviderNames() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []string
	for n := range m.providers {
		out = append(out, n)
	}
	return out
}

func (m *Module) now() time.Time { return m.Now().UTC() }

func (m *Module) dt(t time.Time) types.DateTime {
	d, _ := types.ParseDateTime(t.UTC())
	return d
}

// cronTick enqueues a reconcile job once per slot (all nodes sharing the DB tick).
func (m *Module) cronTick() {
	slot := m.now().Truncate(time.Minute).Format("200601021504")
	_, err := kernel.Jobs(m.app).Enqueue(context.Background(), JobReconcile, nil,
		kernel.CronKey("payments.reconcile:"+slot), kernel.MaxAttempts(3))
	if errors.Is(err, kernel.ErrNoJobQueue) {
		_, err = m.Reconcile(context.Background())
	}
	if err != nil {
		m.app.Logger().Error("payments: reconcile tick failed", "error", err)
	}
}

// ---- collections ---------------------------------------------------------

func auto() []core.Field {
	return []core.Field{
		&core.AutodateField{Name: "created", OnCreate: true},
		&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true},
	}
}

func sel(name string, vals ...string) *core.SelectField {
	return &core.SelectField{Name: name, Values: vals, MaxSelect: 1}
}

// EnsureCollections creates the payments system collections when missing.
func EnsureCollections(app core.App) error {
	type def struct {
		name  string
		build func(c *core.Collection)
	}
	defs := []def{
		{IntentsCollection, func(c *core.Collection) {
			c.Fields.Add(
				&core.TextField{Name: "provider", Required: true},
				sel("status", Statuses()...),
				&core.NumberField{Name: "amount", OnlyInt: true},
				&core.TextField{Name: "currency", Required: true},
				&core.TextField{Name: "idempotency_key"},
				&core.TextField{Name: "order_ref"},
				&core.TextField{Name: "customer"},
				&core.TextField{Name: "customer_collection"},
				&core.TextField{Name: "product"},
				&core.TextField{Name: "description"},
				&core.TextField{Name: "checkout_url", Max: 4096},
				&core.TextField{Name: "provider_ref", Max: 1024},
				&core.JSONField{Name: "metadata", MaxSize: 64 << 10},
				&core.JSONField{Name: "provider_data", MaxSize: 64 << 10},
				&core.JSONField{Name: "grant", MaxSize: 8 << 10},
				&core.NumberField{Name: "refunded_amount", OnlyInt: true},
				&core.DateField{Name: "paid_at"},
				&core.TextField{Name: "last_error", Max: 2000},
			)
			c.Fields.Add(auto()...)
			c.AddIndex("idx_payint_idem", true, "[[customer]], [[customer_collection]], [[idempotency_key]]", "[[idempotency_key]] != ''")
			c.AddIndex("idx_payint_ref", false, "[[provider]], [[provider_ref]]", "")
			c.AddIndex("idx_payint_status", false, "[[status]], [[created]]", "")
		}},
		{EventsCollection, func(c *core.Collection) {
			c.Fields.Add(
				&core.TextField{Name: "provider", Required: true},
				&core.TextField{Name: "event_id"},
				&core.TextField{Name: "type"},
				&core.TextField{Name: "provider_ref", Max: 1024},
				&core.TextField{Name: "intent"},
				&core.BoolField{Name: "verified"},
				&core.TextField{Name: "verify_error", Max: 2000},
				&core.TextField{Name: "raw", Max: 1 << 20},
				&core.JSONField{Name: "headers", MaxSize: 16 << 10},
				&core.JSONField{Name: "event", MaxSize: 64 << 10},
				sel("status", "received", "processed", "ignored", "rejected", "failed", "invalid", "pending_order", "dead"),
				&core.NumberField{Name: "attempts", OnlyInt: true},
				&core.TextField{Name: "error", Max: 2000},
				&core.DateField{Name: "processed_at"},
			)
			c.Fields.Add(auto()...)
			c.AddIndex("idx_payevt_dedupe", true, "[[provider]], [[event_id]]", "[[event_id]] != ''")
			c.AddIndex("idx_payevt_status", false, "[[status]], [[created]]", "")
		}},
		{ProductsCollection, func(c *core.Collection) {
			c.Fields.Add(
				&core.TextField{Name: "slug", Required: true},
				&core.BoolField{Name: "enabled"},
				&core.TextField{Name: "name"},
				&core.NumberField{Name: "amount", OnlyInt: true}, // must be > 0 with a currency: the price is always the product's
				&core.TextField{Name: "currency"},
				&core.TextField{Name: "entitlement_key"},
				&core.NumberField{Name: "duration_days", OnlyInt: true}, // 0 = does not expire
				&core.NumberField{Name: "trial_days", OnlyInt: true},    // reserved, not used yet (trials are granted by hand)
				&core.NumberField{Name: "grace_days", OnlyInt: true},
				&core.NumberField{Name: "quota", OnlyInt: true},
				&core.NumberField{Name: "balance_add", OnlyInt: true},
				&core.BoolField{Name: "subscription"},
			)
			c.Fields.Add(auto()...)
			c.AddIndex("idx_payprod_slug", true, "[[slug]]", "")
		}},
		{RefundsCollection, func(c *core.Collection) {
			c.Fields.Add(
				&core.TextField{Name: "intent", Required: true},
				&core.NumberField{Name: "amount", OnlyInt: true},
				&core.TextField{Name: "currency"},
				sel("status", "pending", "succeeded", "failed"),
				sel("source", "api", "provider"),
				&core.TextField{Name: "provider_ref", Max: 1024},
				&core.TextField{Name: "reason", Max: 1000},
				&core.TextField{Name: "idempotency_key"},
				&core.TextField{Name: "error", Max: 2000},
			)
			c.Fields.Add(auto()...)
			c.AddIndex("idx_payref_idem", true, "[[intent]], [[idempotency_key]]", "[[idempotency_key]] != ''")
			c.AddIndex("idx_payref_ref", false, "[[provider_ref]]", "")
			c.AddIndex("idx_payref_ref_u", true, "[[provider_ref]]", "[[provider_ref]] != ''")
		}},
		{GrantsCollection, func(c *core.Collection) {
			c.Fields.Add(
				&core.TextField{Name: "entitlement", Required: true},
				&core.TextField{Name: "intent"}, // empty = manual grant
				&core.NumberField{Name: "days", OnlyInt: true},
				&core.NumberField{Name: "balance", OnlyInt: true},
				&core.DateField{Name: "ends"}, // the entitlement's end right after this grant (empty = no end)
				&core.BoolField{Name: "revoked"},
			)
			c.Fields.Add(auto()...)
			c.AddIndex("idx_payegr_ent", false, "[[entitlement]], [[revoked]]", "")
			c.AddIndex("idx_payegr_intent", false, "[[intent]]", "")
		}},
		{SubscriptionsCollection, func(c *core.Collection) {
			c.Fields.Add(
				&core.TextField{Name: "subject", Required: true},
				&core.TextField{Name: "subject_collection"},
				&core.TextField{Name: "product"},
				&core.TextField{Name: "provider"},
				&core.TextField{Name: "provider_ref", Max: 1024},
				sel("status", "active", "trial", "past_due", "canceled", "expired"),
				&core.DateField{Name: "current_period_end"},
				&core.TextField{Name: "last_intent"},
			)
			c.Fields.Add(auto()...)
			c.AddIndex("idx_paysub_subject", true, "[[subject]], [[subject_collection]], [[product]]", "")
		}},
		{EntitlementsCollection, func(c *core.Collection) {
			c.Fields.Add(
				&core.TextField{Name: "subject", Required: true},
				&core.TextField{Name: "subject_collection"},
				&core.TextField{Name: "key", Required: true},
				sel("status", "active", "trial", "grace", "lapsed"),
				&core.DateField{Name: "until"}, // end of access for the current status; empty = no end
				&core.DateField{Name: "period_end"},
				&core.NumberField{Name: "grace_seconds", OnlyInt: true},
				&core.NumberField{Name: "quota", OnlyInt: true},
				&core.NumberField{Name: "balance", OnlyInt: true},
				&core.TextField{Name: "source_intent"},
				&core.TextField{Name: "source_subscription"},
				&core.JSONField{Name: "metadata", MaxSize: 16 << 10},
			)
			c.Fields.Add(auto()...)
			c.AddIndex("idx_payent_subject_key", true, "[[subject]], [[subject_collection]], [[key]]", "")
			c.AddIndex("idx_payent_source", false, "[[source_intent]]", "")
		}},
	}
	for _, d := range defs {
		want := core.NewBaseCollection(d.name)
		want.System = true // rules stay nil: superusers only; clients use /api/payments/*
		d.build(want)
		if c, _ := app.FindCollectionByNameOrId(d.name); c != nil {
			if upgradeCollection(c, want) {
				if err := app.Save(c); err != nil {
					return err
				}
			}
			continue
		}
		if err := app.Save(want); err != nil {
			return err
		}
	}
	return nil
}

// upgradeCollection adds the fields, select values and indexes a newer
// version of the module defines to a collection created by an older one
// (additive only, nothing is removed or retyped). It reports whether it changed anything.
func upgradeCollection(have, want *core.Collection) bool {
	changed := false
	for _, f := range want.Fields {
		cur := have.Fields.GetByName(f.GetName())
		if cur == nil {
			have.Fields.Add(f)
			changed = true
			continue
		}
		ws, ok1 := f.(*core.SelectField)
		cs, ok2 := cur.(*core.SelectField)
		if ok1 && ok2 {
			for _, v := range ws.Values {
				if !slices.Contains(cs.Values, v) {
					cs.Values = append(cs.Values, v)
					changed = true
				}
			}
		}
	}
	for _, ix := range want.Indexes {
		if name := dbutils.ParseIndex(ix).IndexName; name != "" && have.GetIndex(name) == "" {
			have.Indexes = append(have.Indexes, ix)
			changed = true
		}
	}
	return changed
}
