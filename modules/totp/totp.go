// Package totp adds RFC 6238 time-based one-time passwords as an MFA method
// that plugs into the upstream mfaId flow, plus single-use recovery codes and
// per-role enforcement.
package totp

import (
	"crypto/sha256"
	"encoding/base64"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/hook"
)

const (
	hookId         = "__tokiTotp__"
	CollectionName = "_totp"

	// AuthMethod is the AuthMethod passed to RecordAuthResponse.
	AuthMethod = "totp"

	FreshAuthWindow = 10 * time.Minute
	maxAttempts     = 5 // code attempts per minute per mfaId (and per record on confirm)

	EnvKey           = "TOKI_TOTP_KEY"
	EnvIssuer        = "TOKI_TOTP_ISSUER"
	EnvRequiredRoles = "TOKI_TOTP_REQUIRED_ROLES"
	EnvRequireSuper  = "TOKI_TOTP_REQUIRE_SUPERUSERS"
	EnvGraceDays     = "TOKI_TOTP_GRACE_DAYS"

	ParamEnforcedAt = "totp_enforced_at"
	ParamEnforce    = "totp_enforce"

	ActionEnabled     = "auth.totp_enabled"
	ActionDisabled    = "auth.totp_disabled"
	ActionRecoveryUse = "auth.recovery_code_used"
	ActionRecoveryNew = "auth.recovery_codes_regenerated"
	ActionEnforce     = "auth.totp_enforcement_changed"
	ActionReauth      = "auth.totp_reauth_failed"
	ActionBlocked     = "auth.totp_required_blocked"

	CodeRequired = "totp_required"
)

var (
	sinkMu    sync.RWMutex
	auditSink func(action, collection, record string, details map[string]any)
)

// SetAuditSink connects TOTP events to an external audit log (tokibase.go wires it).
func SetAuditSink(fn func(action, collection, record string, details map[string]any)) {
	sinkMu.Lock()
	auditSink = fn
	sinkMu.Unlock()
}

func audit(action, collection, record string, details map[string]any) {
	sinkMu.RLock()
	fn := auditSink
	sinkMu.RUnlock()
	if fn != nil {
		fn(action, collection, record, details)
	}
}

var (
	failureMu   sync.RWMutex
	failureSink func(collection string, rec *core.Record)
	lockedSink  func(collection string, rec *core.Record) bool
)

// SetFailureSink counts a failed login code against the record (lockout).
func SetFailureSink(fn func(collection string, rec *core.Record)) {
	failureMu.Lock()
	failureSink = fn
	failureMu.Unlock()
}

// SetLockedSink refuses code checks while the record is locked.
func SetLockedSink(fn func(collection string, rec *core.Record) bool) {
	failureMu.Lock()
	lockedSink = fn
	failureMu.Unlock()
}

func failure(collection string, rec *core.Record) {
	failureMu.RLock()
	fn := failureSink
	failureMu.RUnlock()
	if fn != nil && rec != nil {
		fn(collection, rec)
	}
}

func isLocked(collection string, rec *core.Record) bool {
	failureMu.RLock()
	fn := lockedSink
	failureMu.RUnlock()
	return fn != nil && rec != nil && fn(collection, rec)
}

// loadKey returns the 32-byte secret-encryption key: TOKI_TOTP_KEY (base64 of
// 32 bytes) or, failing that, sha256 of the app settings encryption env value.
func loadKey(app core.App) []byte {
	if v := strings.TrimSpace(os.Getenv(EnvKey)); v != "" {
		for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
			if b, err := enc.DecodeString(v); err == nil && len(b) == 32 {
				return b
			}
		}
		return nil // invalid TOKI_TOTP_KEY: never fall back silently
	}
	if env := app.EncryptionEnv(); env != "" {
		if v := os.Getenv(env); v != "" {
			s := sha256.Sum256([]byte("toki-totp:" + v))
			return s[:]
		}
	}
	return nil
}

// Module is the TOTP module.
type Module struct {
	app core.App
	now func() time.Time

	mu      sync.Mutex // serializes code/recovery consumption (single process per pb_data)
	limiter *limiter
}

// Register binds the module (always; endpoints refuse setup without a key).
func Register(app core.App) *Module {
	m := &Module{app: app, now: time.Now, limiter: newLimiter()}
	m.bind()
	return m
}

func (m *Module) init() {
	if loadKey(m.app) == nil {
		m.app.Logger().Info("totp: no encryption key (set " + EnvKey + " as base64 of 32 bytes or the settings encryption env); enrolment disabled")
	} else if err := EnsureCollection(m.app); err != nil {
		m.app.Logger().Error("totp: failed to initialize the _totp collection", "error", err)
	}
	if c := loadEnforcement(m.app); c.active() {
		if _, ok := readParam(m.app, ParamEnforcedAt); !ok {
			setParam(m.app, ParamEnforcedAt, m.now().UTC().Format(time.RFC3339))
		}
	}
}

func (m *Module) bind() {
	app := m.app
	if app.IsBootstrapped() {
		m.init()
	}
	app.OnBootstrap().Bind(&hook.Handler[*core.BootstrapEvent]{
		Id: hookId, Priority: -1,
		Func: func(e *core.BootstrapEvent) error {
			if err := e.Next(); err != nil {
				return err
			}
			m.init()
			return nil
		},
	})

	app.OnRecordAfterDeleteSuccess().Bind(&hook.Handler[*core.RecordEvent]{
		Id: hookId,
		Func: func(e *core.RecordEvent) error {
			if err := e.Next(); err != nil {
				return err
			}
			if !e.Record.Collection().IsAuth() {
				return nil
			}
			rows, err := e.App.FindAllRecords(CollectionName, hashExp(e.Record.Collection().Id, e.Record.Id))
			if err != nil {
				return nil
			}
			for _, r := range rows {
				_ = e.App.Delete(r)
			}
			return nil
		},
	})

	app.OnRecordAuthRequest().Bind(&hook.Handler[*core.RecordAuthRequestEvent]{
		Id: hookId, Priority: -100,
		Func: m.enforceHook,
	})

	app.OnServe().Bind(&hook.Handler[*core.ServeEvent]{
		Id: hookId,
		Func: func(e *core.ServeEvent) error {
			m.bindRoutes(e.Router)
			return e.Next()
		},
	})
}

// EnsureCollection creates `_totp` (system, superuser-only) when missing.
func EnsureCollection(app kernel.App) error {
	if _, err := app.FindCollectionByNameOrId(CollectionName); err == nil {
		return nil
	}
	c := kernel.NewBaseCollection(CollectionName)
	c.System = true
	c.Fields.Add(
		&kernel.TextField{Name: "collection", Required: true, Max: 64},
		&kernel.TextField{Name: "record", Required: true, Max: 64},
		&kernel.TextField{Name: "secret", Required: true, Hidden: true, Max: 1024},
		&kernel.TextField{Name: "issuer", Max: 128},
		&kernel.BoolField{Name: "enabled"},
		&kernel.JSONField{Name: "recovery_codes", Hidden: true, MaxSize: 65536},
		&kernel.DateField{Name: "last_used_at"},
		&kernel.NumberField{Name: "last_counter"},
		&kernel.AutodateField{Name: "created", OnCreate: true},
	)
	c.AddIndex("idx_totp_owner", true, "collection, record", "")
	if err := app.Save(c); err != nil {
		if _, e2 := app.FindCollectionByNameOrId(CollectionName); e2 == nil {
			return nil
		}
		return err
	}
	return nil
}
