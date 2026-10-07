// Package lockout implements a progressive, per-identity lockout for failed
// authentication (password and OTP), independent of the client IP.
package lockout

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/tools/hook"
	"github.com/tokibase/tokibase/tools/router"
	"github.com/tokibase/tokibase/tools/security"
	"github.com/tokibase/tokibase/tools/store"
)

const (
	hookId        = "__tokiLockout__"
	hookPriority  = -1 << 20
	TableName     = "_lockout"
	persistEvery  = 5
	maxCacheItems = 20000
	timeLayout    = "2006-01-02 15:04:05.000Z"

	// AuditAction is the action name passed to the audit sink.
	AuditAction = "auth.lockout"

	msgPassword = "Failed to authenticate."
	msgOTP      = "Invalid or expired OTP"
)

const createTableSQL = `CREATE TABLE IF NOT EXISTS {{_lockout}} (
	[[key]]           TEXT PRIMARY KEY NOT NULL,
	[[failures]]      INTEGER NOT NULL DEFAULT 0,
	[[first_failure]] TEXT NOT NULL DEFAULT '',
	[[locked_until]]  TEXT,
	[[updated]]       TEXT NOT NULL DEFAULT ''
);`

// Policy is the lockout configuration.
type Policy struct {
	Threshold int
	Window    time.Duration
	Base      time.Duration
	Max       time.Duration
}

// Enabled reports whether lockout is enabled (env TOKI_LOCKOUT=off disables it).
func Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TOKI_LOCKOUT"))) {
	case "off", "false", "0", "disabled":
		return false
	}
	return true
}

// LoadPolicy reads the policy from the environment (invalid values fall back to defaults).
func LoadPolicy() Policy {
	p := Policy{Threshold: 5, Window: 15 * time.Minute, Base: time.Minute, Max: time.Hour}
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("TOKI_LOCKOUT_THRESHOLD"))); err == nil && n > 0 {
		p.Threshold = n
	}
	dur := func(name string, dst *time.Duration) {
		if d, err := time.ParseDuration(strings.TrimSpace(os.Getenv(name))); err == nil && d > 0 {
			*dst = d
		}
	}
	dur("TOKI_LOCKOUT_WINDOW", &p.Window)
	dur("TOKI_LOCKOUT_BASE", &p.Base)
	dur("TOKI_LOCKOUT_MAX", &p.Max)
	if p.Max < p.Base {
		p.Max = p.Base
	}
	return p
}

// LockDuration returns the lock length for the n-th lock (n starts at 1).
func (p Policy) LockDuration(n int) time.Duration {
	d := p.Base
	for i := 1; i < n; i++ {
		d *= 2
		if d >= p.Max || d <= 0 {
			return p.Max
		}
	}
	if d > p.Max {
		return p.Max
	}
	return d
}

type entry struct {
	failures    int
	first       time.Time
	lockedUntil time.Time
	updated     time.Time
}

// Module is the lockout state machine.
type Module struct {
	app    core.App
	policy Policy
	now    func() time.Time

	mu    sync.Mutex
	cache *store.Store[string, *entry]

	sinkMu sync.RWMutex
	sink   func(action, collection, record string, details map[string]any)
}

var (
	sinkMu     sync.RWMutex
	globalSink func(action, collection, record string, details map[string]any)
)

// SetAuditSink connects lock events to an external audit log.
// Modules must not import each other, so the wiring happens in tokibase.go.
func SetAuditSink(fn func(action, collection, record string, details map[string]any)) {
	sinkMu.Lock()
	globalSink = fn
	sinkMu.Unlock()
}

// IdentityHash returns the sha256 prefix used in logs and audit entries.
func IdentityHash(collection, identity string) string {
	return security.SHA256(Key(collection, identity))[:16]
}

// Key builds the storage key.
func Key(collection, identity string) string {
	return collection + ":" + strings.ToLower(strings.TrimSpace(identity))
}

// New creates a module with the given policy (no hooks are bound).
func New(app core.App, p Policy) *Module {
	return &Module{app: app, policy: p, now: time.Now, cache: store.New[string, *entry](nil)}
}

// active is the module bound by Register, used by RecordFailure.
var active atomic.Pointer[Module]

// RecordFailure counts one failed authentication for an identity on behalf of
// an auth method that has no lockout hook of its own (e.g. passkeys; wired in
// tokibase.go). The key is the same as for OTP: <collection>:<identity>. It is
// a no-op when the module is not registered.
func RecordFailure(collection, identity string) {
	if m := active.Load(); m != nil {
		m.failure(collection, identity, Key(collection, identity))
	}
}

// Register binds the lockout hooks to app.
func Register(app core.App) *Module {
	m := New(app, LoadPolicy())
	active.Store(m)

	init := func() {
		if _, err := app.AuxDB().NewQuery(createTableSQL).Execute(); err != nil {
			app.Logger().Error("lockout: failed to initialize the _lockout table", "error", err)
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

	app.OnRecordAuthWithPasswordRequest().Bind(&hook.Handler[*core.RecordAuthWithPasswordRequestEvent]{
		Id: hookId, Priority: hookPriority,
		Func: func(e *core.RecordAuthWithPasswordRequestEvent) error {
			return m.guard(e.RequestEvent, e.Collection.Name, e.Identity, msgPassword, e.Next)
		},
	})

	// OnRecordAuthWithOTPRequest only fires after the OTP password was already
	// validated, so wrong OTPs never reach it: guard the route instead.
	app.OnServe().Bind(&hook.Handler[*core.ServeEvent]{
		Id: hookId, Priority: hookPriority,
		Func: func(e *core.ServeEvent) error {
			e.Router.Bind(m.otpMiddleware())
			return e.Next()
		},
	})
	return m
}

// guard wraps next with the lock check and failure/success accounting.
func (m *Module) guard(e *core.RequestEvent, collection, identity, msg string, next func() error) error {
	key := Key(collection, identity)
	if wait := m.lockedFor(key); wait > 0 {
		secs := int((wait + time.Second - 1) / time.Second)
		e.Response.Header().Set("Retry-After", strconv.Itoa(secs))
		return e.BadRequestError(msg, errors.New("identity locked"))
	}
	err := next()
	if err == nil {
		m.success(key)
		return nil
	}
	if errors.Is(err, apis.ErrMFA) {
		// first factor accepted, MFA second step required: credentials were valid
		m.success(key)
		return err
	}
	var apiErr *router.ApiError
	if errors.As(err, &apiErr) && apiErr.Status == 400 {
		m.failure(collection, identity, key)
	}
	return err
}

// lockedFor returns the remaining lock time (0 when not locked).
func (m *Module) lockedFor(key string) time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	en := m.loadLocked(key)
	if en == nil || !en.lockedUntil.After(now) {
		return 0
	}
	// locks are always persisted: re-read the row so that an unlock done by
	// another process (CLI) takes effect immediately.
	row, ok := m.readRow(key)
	if !ok || !row.lockedUntil.After(now) {
		m.cache.Remove(key)
		return 0
	}
	en.lockedUntil = row.lockedUntil
	return en.lockedUntil.Sub(now)
}

// loadLocked returns the cached entry or loads it from the DB (m.mu held).
func (m *Module) loadLocked(key string) *entry {
	if en, ok := m.cache.GetOk(key); ok {
		return en
	}
	row, ok := m.readRow(key)
	if !ok {
		return nil
	}
	m.put(key, row)
	return row
}

func (m *Module) put(key string, en *entry) {
	if m.cache.Length() >= maxCacheItems {
		// evict the least recently updated unlocked entry (locks are persisted anyway)
		var oldK string
		var oldT time.Time
		for k, v := range m.cache.GetAll() {
			if oldK == "" || v.updated.Before(oldT) {
				oldK, oldT = k, v.updated
			}
		}
		m.cache.Remove(oldK)
	}
	m.cache.Set(key, en)
}

func (m *Module) success(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, cached := m.cache.GetOk(key)
	m.cache.Remove(key)
	if cached || m.rowExists(key) {
		m.deleteRow(key)
	}
}

func (m *Module) failure(collection, identity, key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	en := m.loadLocked(key)
	if en == nil {
		en = &entry{first: now}
		m.put(key, en)
	}
	// forget old failures (the window runs from the last failure or lock end)
	ref := en.updated
	if en.lockedUntil.After(ref) {
		ref = en.lockedUntil
	}
	if !ref.IsZero() && now.After(ref.Add(m.policy.Window)) {
		*en = entry{first: now}
	}
	en.failures++
	en.updated = now

	locked := false
	if en.failures%m.policy.Threshold == 0 {
		n := en.failures / m.policy.Threshold
		en.lockedUntil = now.Add(m.policy.LockDuration(n))
		locked = true
	}
	if locked || en.failures%persistEvery == 0 {
		m.writeRow(key, en)
	}
	if locked {
		m.announce(collection, identity, en)
	}
}

func (m *Module) announce(collection, identity string, en *entry) {
	h := IdentityHash(collection, identity)
	m.app.Logger().Warn("lockout: identity locked after repeated failed authentication",
		"collection", collection, "identity", h,
		"failures", en.failures, "lockedUntil", en.lockedUntil.UTC().Format(time.RFC3339))
	sinkMu.RLock()
	fn := globalSink
	sinkMu.RUnlock()
	if fn != nil {
		fn(AuditAction, collection, h, map[string]any{
			"failures":    en.failures,
			"lockedUntil": en.lockedUntil.UTC().Format(time.RFC3339),
		})
	}
}

// --- storage ---------------------------------------------------------

func (m *Module) readRow(key string) (*entry, bool) {
	var r struct {
		Failures     int    `db:"failures"`
		FirstFailure string `db:"first_failure"`
		LockedUntil  string `db:"locked_until"`
		Updated      string `db:"updated"`
	}
	err := m.app.AuxDB().NewQuery("SELECT [[failures]], [[first_failure]], COALESCE([[locked_until]], '') AS [[locked_until]], [[updated]] FROM {{_lockout}} WHERE [[key]]={:k}").
		Bind(dbx.Params{"k": key}).One(&r)
	if err != nil {
		return nil, false
	}
	return &entry{failures: r.Failures, first: ParseTime(r.FirstFailure), lockedUntil: ParseTime(r.LockedUntil), updated: ParseTime(r.Updated)}, true
}

func (m *Module) rowExists(key string) bool {
	_, ok := m.readRow(key)
	return ok
}

func (m *Module) writeRow(key string, en *entry) {
	var lu any
	if !en.lockedUntil.IsZero() {
		lu = en.lockedUntil.UTC().Format(timeLayout)
	}
	_, err := m.app.AuxDB().NewQuery(`INSERT INTO {{_lockout}} ([[key]],[[failures]],[[first_failure]],[[locked_until]],[[updated]])
		VALUES ({:k},{:f},{:ff},{:lu},{:u})
		ON CONFLICT([[key]]) DO UPDATE SET [[failures]]=excluded.[[failures]], [[first_failure]]=excluded.[[first_failure]],
		[[locked_until]]=excluded.[[locked_until]], [[updated]]=excluded.[[updated]]`).
		Bind(dbx.Params{"k": key, "f": en.failures, "ff": en.first.UTC().Format(timeLayout), "lu": lu, "u": en.updated.UTC().Format(timeLayout)}).
		Execute()
	if err != nil {
		m.app.Logger().Error("lockout: failed to persist state", "error", err)
	}
}

func (m *Module) deleteRow(key string) {
	if _, err := m.app.AuxDB().NewQuery("DELETE FROM {{_lockout}} WHERE [[key]]={:k}").Bind(dbx.Params{"k": key}).Execute(); err != nil {
		m.app.Logger().Error("lockout: failed to delete state", "error", err)
	}
}

// ParseTime parses the stored datetime layout (zero time on failure).
func ParseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(timeLayout, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
