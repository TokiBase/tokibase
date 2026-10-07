package webhooks

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/tools/hook"
	"github.com/tokibase/tokibase/tools/security"
)

const (
	hookId       = "__tokiWebhooks__"
	hookPriority = 1 << 20 // run after other handlers: the event already succeeded
	// cacheTTL is how long the webhook config is cached. Changes made by
	// another process (CLI, second node) are picked up after at most this long;
	// when the cache is older, capture reads the enabled webhooks from the DB, so
	// no event is written against a stale list for longer than this.
	cacheTTL = 5 * time.Second

	defaultTimeoutMs   = 10000
	defaultMaxAttempts = 8
	maxTimeoutMs       = 30000
	minTimeoutMs       = 100

	// MinSecretLen is the minimum length of a webhook secret.
	MinSecretLen = 16

	defaultMaxPayload = 256 << 10
)

func maxPayloadBytes() int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("TOKI_WEBHOOK_MAX_PAYLOAD_BYTES"))); err == nil && n > 0 {
		return n
	}
	return defaultMaxPayload
}

// Enabled reports whether webhooks are enabled (env TOKI_WEBHOOKS=off disables).
func Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TOKI_WEBHOOKS"))) {
	case "off", "false", "0", "disabled":
		return false
	}
	return true
}

func workerCount() int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("TOKI_WEBHOOK_WORKERS"))); err == nil && n > 0 {
		return n
	}
	return 4
}

var (
	sinkMu     sync.Mutex
	globalSink func(action, collection, record string, details map[string]any)
)

// SetAuditSink connects dead-letter events to an external audit log.
// Modules must not import each other, so the wiring happens in tokibase.go.
func SetAuditSink(fn func(action, collection, record string, details map[string]any)) {
	sinkMu.Lock()
	globalSink = fn
	sinkMu.Unlock()
}

func audit(action, collection, record string, details map[string]any) {
	sinkMu.Lock()
	fn := globalSink
	sinkMu.Unlock()
	if fn != nil {
		fn(action, collection, record, details)
	}
}

// Webhook is a row of the _webhooks collection.
type Webhook struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	URL         string            `json:"url"`
	Secret      string            `json:"-"`
	Events      []string          `json:"events"`
	Collections []string          `json:"collections"`
	Enabled     bool              `json:"enabled"`
	Headers     map[string]string `json:"headers"`
	TimeoutMs   int               `json:"timeout_ms"`
	MaxAttempts int               `json:"max_attempts"`
}

func webhookOf(r *core.Record) *Webhook {
	w := &Webhook{
		ID: r.Id, Name: r.GetString("name"), URL: r.GetString("url"), Secret: r.GetString("secret"),
		Enabled: r.GetBool("enabled"), TimeoutMs: r.GetInt("timeout_ms"), MaxAttempts: r.GetInt("max_attempts"),
	}
	_ = r.UnmarshalJSONField("events", &w.Events)
	_ = r.UnmarshalJSONField("collections", &w.Collections)
	_ = r.UnmarshalJSONField("headers", &w.Headers)
	if w.TimeoutMs <= 0 {
		w.TimeoutMs = defaultTimeoutMs
	}
	if w.TimeoutMs < minTimeoutMs {
		w.TimeoutMs = minTimeoutMs
	}
	if w.TimeoutMs > maxTimeoutMs {
		w.TimeoutMs = maxTimeoutMs
	}
	if w.MaxAttempts <= 0 {
		w.MaxAttempts = defaultMaxAttempts
	}
	return w
}

// matches reports whether the webhook subscribes to event for collection.
func (w *Webhook) matches(event, collection string) bool {
	if !w.Enabled {
		return false
	}
	if len(w.Collections) > 0 {
		ok := false
		for _, c := range w.Collections {
			if c == collection {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	for _, p := range w.Events {
		if p == event || p == "*" {
			return true
		}
		if strings.HasSuffix(p, ".*") && strings.HasPrefix(event, strings.TrimSuffix(p, "*")) {
			return true
		}
	}
	return false
}

var ensureMu sync.Mutex

// ensureCollection creates the _webhooks config collection when missing.
func ensureCollection(app core.App) error {
	ensureMu.Lock()
	defer ensureMu.Unlock()
	if _, err := app.FindCachedCollectionByNameOrId(ConfigCollection); err == nil {
		return nil
	}
	if !app.HasTable("_collections") {
		return errors.New("webhooks: _collections table is not ready")
	}
	c := core.NewBaseCollection(ConfigCollection)
	// rules stay nil: superuser only
	c.Fields.Add(
		&core.TextField{Name: "name", Required: true, Max: 100},
		&core.TextField{Name: "url", Required: true, Max: 2000},
		&core.TextField{Name: "secret", Hidden: true, Max: 500},
		&core.JSONField{Name: "events", MaxSize: 64 << 10},
		&core.JSONField{Name: "collections", MaxSize: 64 << 10},
		&core.BoolField{Name: "enabled"},
		&core.JSONField{Name: "headers", MaxSize: 64 << 10},
		&core.NumberField{Name: "timeout_ms", OnlyInt: true, Min: floatPtr(0)},
		&core.NumberField{Name: "max_attempts", OnlyInt: true, Min: floatPtr(0)},
		&core.AutodateField{Name: "created", OnCreate: true},
		&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true},
	)
	c.AddIndex("idx_toki_webhooks_name", true, "name", "")
	return app.Save(c)
}

func floatPtr(f float64) *float64 { return &f }

// Module is the registered webhooks module.
type Module struct {
	app core.App

	mu       sync.Mutex
	cache    []*Webhook
	loadedAt time.Time

	wake   chan struct{}
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// Register binds the capture hooks and the worker lifecycle to app.
// The delivery table is created on bootstrap (or immediately when the app is
// already bootstrapped); workers start on serve, see Start for tests.
func Register(app core.App) *Module {
	m := &Module{app: app, wake: make(chan struct{}, 1)}

	init := func() {
		if err := initTable(app); err != nil {
			app.Logger().Error("webhooks: failed to initialize the delivery table", "error", err)
		}
		if err := ensureCollection(app); err != nil {
			app.Logger().Error("webhooks: failed to initialize the _webhooks collection", "error", err)
		}
	}
	if app.IsBootstrapped() {
		init()
	}
	app.OnBootstrap().Bind(&hook.Handler[*core.BootstrapEvent]{
		Id: hookId, Priority: hookPriority,
		Func: func(e *core.BootstrapEvent) error {
			if err := e.Next(); err != nil {
				return err
			}
			init()
			return nil
		},
	})
	app.OnServe().Bind(&hook.Handler[*core.ServeEvent]{
		Id: hookId, Priority: hookPriority,
		Func: func(e *core.ServeEvent) error {
			m.Start()
			return e.Next()
		},
	})
	app.OnTerminate().Bind(&hook.Handler[*core.TerminateEvent]{
		Id: hookId,
		Func: func(e *core.TerminateEvent) error {
			m.Stop()
			return e.Next()
		},
	})

	// config cache invalidation + validation
	for _, h := range []func(...string) *hook.TaggedHook[*core.RecordEvent]{
		app.OnRecordAfterCreateSuccess, app.OnRecordAfterUpdateSuccess, app.OnRecordAfterDeleteSuccess,
	} {
		h(ConfigCollection).Bind(&hook.Handler[*core.RecordEvent]{
			Id: hookId + "cfg", Priority: hookPriority,
			Func: func(e *core.RecordEvent) error {
				m.invalidate()
				return e.Next()
			},
		})
	}
	app.OnRecordValidate(ConfigCollection).Bind(&hook.Handler[*core.RecordEvent]{
		Id: hookId + "val",
		Func: func(e *core.RecordEvent) error {
			if err := validateConfig(webhookOf(e.Record)); err != nil {
				return err
			}
			return e.Next()
		},
	})

	m.bindCapture()
	return m
}

// forbiddenHeaders are set by the HTTP stack or by TokiBase and cannot be configured.
var forbiddenHeaders = map[string]bool{
	"host": true, "content-length": true, "transfer-encoding": true, "connection": true,
	"x-toki-signature": true, "x-toki-timestamp": true, "x-toki-delivery": true, "x-toki-event": true, "x-toki-seq": true,
}

// Redacted returns a copy that is safe to print: header values are masked
// (they usually hold bearer tokens); the secret is never serialized.
func (w *Webhook) Redacted() *Webhook {
	cp := *w
	cp.Secret = ""
	if w.Headers != nil {
		cp.Headers = make(map[string]string, len(w.Headers))
		for k := range w.Headers {
			cp.Headers[k] = "***"
		}
	}
	return &cp
}

func validateConfig(w *Webhook) error {
	if len(w.Secret) < MinSecretLen {
		return fmt.Errorf("webhooks: secret is required and must be at least %d characters", MinSecretLen)
	}
	for k, v := range w.Headers {
		if forbiddenHeaders[strings.ToLower(strings.TrimSpace(k))] {
			return fmt.Errorf("webhooks: header %q cannot be set", k)
		}
		if strings.ContainsAny(k, "\r\n") || strings.ContainsAny(v, "\r\n") {
			return fmt.Errorf("webhooks: header %q contains a line break", k)
		}
	}
	u, err := url.Parse(w.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("webhooks: url must be an absolute http(s) URL")
	}
	for _, p := range w.Events {
		if p == "" {
			return fmt.Errorf("webhooks: empty event pattern")
		}
	}
	return nil
}

func (m *Module) invalidate() {
	m.mu.Lock()
	m.cache = nil
	m.loadedAt = time.Time{}
	m.mu.Unlock()
}

// webhooks returns the config, re-reading the DB when the cache is older than
// cacheTTL. On a load error the stale cache is used (and retried on the next
// event); without any cache the error is returned so the caller can report the
// lost event instead of silently dropping it.
func (m *Module) webhooks() ([]*Webhook, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.loadedAt.IsZero() && time.Since(m.loadedAt) < cacheTTL {
		return m.cache, nil
	}
	list, err := loadAll(m.app)
	if err != nil {
		m.app.Logger().Warn("webhooks: failed to load config", "error", err)
		if m.cache != nil {
			return m.cache, nil
		}
		return nil, err
	}
	m.cache, m.loadedAt = list, time.Now()
	return list, nil
}

func loadAll(app core.App) ([]*Webhook, error) {
	if _, err := app.FindCachedCollectionByNameOrId(ConfigCollection); err != nil {
		return nil, nil // not created yet
	}
	recs, err := app.FindAllRecords(ConfigCollection)
	if err != nil {
		return nil, err
	}
	out := make([]*Webhook, 0, len(recs))
	for _, r := range recs {
		out = append(out, webhookOf(r))
	}
	return out, nil
}

// LoadAll returns every configured webhook (secrets included, for CLI/internal use).
func LoadAll(app core.App) ([]*Webhook, error) {
	if err := ensureCollection(app); err != nil {
		return nil, err
	}
	return loadAll(app)
}

func findByName(app core.App, name string) (*core.Record, error) {
	if err := ensureCollection(app); err != nil {
		return nil, err
	}
	r, err := app.FindFirstRecordByData(ConfigCollection, "name", name)
	if err != nil {
		return nil, fmt.Errorf("%w: %q", errNotFound, name)
	}
	return r, nil
}

// Add creates a webhook. An empty secret is generated and returned.
func Add(app core.App, w Webhook) (*Webhook, error) {
	if err := ensureCollection(app); err != nil {
		return nil, err
	}
	if w.Name == "" {
		return nil, errors.New("name is required")
	}
	if len(w.Events) == 0 {
		return nil, errors.New("at least one event is required")
	}
	if w.Secret == "" {
		w.Secret = security.RandomString(32)
	}
	if len(w.Secret) < MinSecretLen {
		return nil, fmt.Errorf("secret must be at least %d characters", MinSecretLen)
	}
	col, err := app.FindCachedCollectionByNameOrId(ConfigCollection)
	if err != nil {
		return nil, err
	}
	r := core.NewRecord(col)
	r.Set("name", w.Name)
	r.Set("url", w.URL)
	r.Set("secret", w.Secret)
	r.Set("events", w.Events)
	if w.Collections == nil {
		w.Collections = []string{}
	}
	r.Set("collections", w.Collections)
	r.Set("enabled", true)
	if w.Headers == nil {
		w.Headers = map[string]string{}
	}
	r.Set("headers", w.Headers)
	r.Set("timeout_ms", defaultTimeoutMs)
	r.Set("max_attempts", defaultMaxAttempts)
	if err := app.Save(r); err != nil {
		return nil, err
	}
	return webhookOf(r), nil
}

// Remove deletes a webhook by name.
func Remove(app core.App, name string) error {
	r, err := findByName(app, name)
	if err != nil {
		return err
	}
	return app.Delete(r)
}

// Find returns a webhook by name.
func Find(app core.App, name string) (*Webhook, error) {
	r, err := findByName(app, name)
	if err != nil {
		return nil, err
	}
	return webhookOf(r), nil
}

// Start launches the delivery workers (idempotent).
func (m *Module) Start() {
	m.mu.Lock()
	if m.cancel != nil {
		m.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.mu.Unlock()

	for i := 0; i < workerCount(); i++ {
		m.wg.Add(1)
		go m.worker(ctx, i == 0)
	}
}

// Stop stops the workers and waits for in-flight deliveries.
func (m *Module) Stop() {
	m.mu.Lock()
	cancel := m.cancel
	m.cancel = nil
	m.mu.Unlock()
	if cancel != nil {
		cancel()
		m.wg.Wait()
	}
}

func (m *Module) notify() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// enqueue creates a delivery for every webhook subscribed to the event.
func (m *Module) enqueue(event, collection, recordID string, p *Payload) {
	hooks, err := m.webhooks()
	if err != nil {
		m.app.Logger().Error("webhooks: event dropped, config unavailable", "event", event, "collection", collection, "error", err)
		return
	}
	for _, w := range hooks {
		if !w.matches(event, collection) {
			continue
		}
		seq, err := nextSeq(m.app, w.ID)
		if err != nil {
			m.app.Logger().Warn("webhooks: failed to allocate sequence number", "webhook", w.Name, "error", err)
		}
		body, err := encodePayload(p, seq, recordID)
		if err != nil {
			m.app.Logger().Warn("webhooks: failed to encode payload", "event", event, "error", err)
			continue
		}
		if _, err := insertDelivery(m.app, w, event, collection, recordID, body, seq, nowFn()); err != nil {
			m.app.Logger().Warn("webhooks: failed to queue delivery", "event", event, "webhook", w.Name, "error", err)
			continue
		}
		m.notify()
	}
}
