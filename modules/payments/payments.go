//go:build !no_payments

package payments

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
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
}

const (
	moduleStoreKey = "__tokiPaymentsModule__"
	paidHookKey    = "__tokiPaymentsOnPaid__"
)

var storeMu sync.Mutex

// OnPaid returns the hook triggered for every `payment.paid` transition, for
// Go consumers (and WASM/webhook bridges built on top). There is no generic
// custom-event path in modules/webhooks (it captures record events of user
// collections only), so consumers bind here; see docs/modules/payments.md.
func OnPaid(app core.App) *hook.Hook[*PaidEvent] {
	storeMu.Lock()
	defer storeMu.Unlock()
	if h, ok := app.Store().Get(paidHookKey).(*hook.Hook[*PaidEvent]); ok {
		return h
	}
	h := &hook.Hook[*PaidEvent]{}
	app.Store().Set(paidHookKey, h)
	return h
}

// Module holds the providers and tunables of one app.
type Module struct {
	app core.App
	Now func() time.Time

	mu        sync.RWMutex
	providers map[string]Provider

	rl *ipLimiter

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
		ReconcileAfter: time.Duration(envInt("TOKI_PAYMENTS_RECONCILE_MINUTES", 10)) * time.Minute,
		IntentTTL:      time.Duration(envInt("TOKI_PAYMENTS_INTENT_TTL_HOURS", 168)) * time.Hour,
		rl:             newIPLimiter(envInt("TOKI_PAYMENTS_WEBHOOK_RPM", 300)),
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
				sel("status", "received", "processed", "ignored", "rejected", "failed", "invalid"),
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
				&core.NumberField{Name: "amount", OnlyInt: true}, // 0 = client may choose (server side only)
				&core.TextField{Name: "currency"},
				&core.TextField{Name: "entitlement_key"},
				&core.NumberField{Name: "duration_days", OnlyInt: true}, // 0 = does not expire
				&core.NumberField{Name: "trial_days", OnlyInt: true},
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
		if c, _ := app.FindCollectionByNameOrId(d.name); c != nil {
			continue
		}
		c := core.NewBaseCollection(d.name)
		c.System = true // rules stay nil: superusers only; clients use /api/payments/*
		d.build(c)
		if err := app.Save(c); err != nil {
			return err
		}
	}
	return nil
}
