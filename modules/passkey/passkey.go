// Package passkey adds WebAuthn/FIDO2 passkeys to every auth collection:
// registration for logged-in users, passwordless login with discoverable
// credentials and management. The login result is the standard PocketBase
// auth response (apis.RecordAuthResponse), so sessions, MFA, authRule and the
// OnRecordAuthRequest hooks apply unchanged.
package passkey

import (
	"errors"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/hook"
)

const (
	hookId         = "__tokiPasskey__"
	CollectionName = "_passkeys"
	ChallengeTable = "_passkey_challenges"

	// AuthMethod is the AuthMethod value passed to RecordAuthResponse (and stored as MFA method).
	AuthMethod = "passkey"

	ChallengeTTL = 5 * time.Minute
	// maxChallenges is the cap of pending challenges per auth collection.
	maxChallenges = 200000
	// maxChallengesPerIP is the cap of pending challenges per client IP; the
	// oldest ones are evicted when it is exceeded.
	maxChallengesPerIP = 20
	maxPerUser         = 20

	// FreshAuthWindow is how recent the auth token must be (or a password
	// must be re-submitted) to register or delete a passkey.
	FreshAuthWindow = 10 * time.Minute

	EnvRPID    = "TOKI_PASSKEY_RP_ID"
	EnvRPName  = "TOKI_PASSKEY_RP_NAME"
	EnvOrigins = "TOKI_PASSKEY_ORIGINS"
	EnvClone   = "TOKI_PASSKEY_CLONE_POLICY"
	// EnvIdentityHint=1 lets login/options honour the submitted identity and
	// return allowCredentials (reveals which accounts have passkeys).
	EnvIdentityHint = "TOKI_PASSKEY_ALLOW_IDENTITY_HINT"

	ActionRegister = "auth.passkey_register"
	ActionDelete   = "auth.passkey_delete"
	ActionLogin    = "auth.passkey_login"
	ActionClone    = "auth.passkey_clone_suspected"
	ActionReauth   = "auth.passkey_reauth_failed"
)

const createChallengesSQL = `CREATE TABLE IF NOT EXISTS {{_passkey_challenges}} (
	[[challenge]]  TEXT PRIMARY KEY NOT NULL,
	[[kind]]       TEXT NOT NULL DEFAULT '',
	[[collection]] TEXT NOT NULL DEFAULT '',
	[[record]]     TEXT NOT NULL DEFAULT '',
	[[ip]]         TEXT NOT NULL DEFAULT '',
	[[data]]       TEXT NOT NULL DEFAULT '',
	[[expires]]    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS {{idx__passkey_challenges_expires}} ON {{_passkey_challenges}} ([[expires]]);`

// Config is the relying party configuration.
type Config struct {
	RPID    string
	RPName  string
	Origins []string
	// IdentityHint enables allowCredentials in login/options (default off).
	IdentityHint bool
	// ClonePolicy is "warn" (default: flag and audit, login proceeds) or "deny".
	ClonePolicy string
}

// LoadConfig reads the configuration from the environment.
func LoadConfig() Config {
	c := Config{
		RPID:        strings.TrimSpace(os.Getenv(EnvRPID)),
		RPName:      strings.TrimSpace(os.Getenv(EnvRPName)),
		ClonePolicy: "warn",
	}
	for _, o := range strings.Split(os.Getenv(EnvOrigins), ",") {
		if o = strings.TrimSpace(o); o != "" {
			c.Origins = append(c.Origins, o)
		}
	}
	if c.RPID != "" && len(c.Origins) == 0 {
		c.Origins = []string{"https://" + c.RPID}
	}
	if c.RPName == "" {
		c.RPName = c.RPID
	}
	c.IdentityHint = strings.TrimSpace(os.Getenv(EnvIdentityHint)) == "1"
	if strings.EqualFold(strings.TrimSpace(os.Getenv(EnvClone)), "deny") {
		c.ClonePolicy = "deny"
	}
	return c
}

// Active reports whether the configuration is sufficient to serve passkeys.
func (c Config) Active() bool { return c.RPID != "" && len(c.Origins) > 0 }

var (
	sinkMu      sync.RWMutex
	auditSink   func(action, collection, record string, details map[string]any)
	failureSink func(collection string, rec *core.Record)
	lockedSink  func(collection string, rec *core.Record) bool
)

// SetAuditSink connects register/delete/login/clone events to an external
// audit log. Modules must not import each other, so tokibase.go wires it.
func SetAuditSink(fn func(action, collection, record string, details map[string]any)) {
	sinkMu.Lock()
	auditSink = fn
	sinkMu.Unlock()
}

// SetFailureSink is called with (collection name, auth record) for a failed
// assertion that is attributable to the record: a known credential whose
// signature is well-formed but invalid, or a failed password re-auth. The
// receiver must key the failure like the password flow (lockout wiring).
func SetFailureSink(fn func(collection string, rec *core.Record)) {
	sinkMu.Lock()
	failureSink = fn
	sinkMu.Unlock()
}

// SetLockedSink connects the lockout state: passkey login and re-auth are
// refused (with the usual generic error) while it reports the record locked.
func SetLockedSink(fn func(collection string, rec *core.Record) bool) {
	sinkMu.Lock()
	lockedSink = fn
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

func failure(collection string, rec *core.Record) {
	sinkMu.RLock()
	fn := failureSink
	sinkMu.RUnlock()
	if fn != nil && rec != nil {
		fn(collection, rec)
	}
}

func isLocked(collection string, rec *core.Record) bool {
	sinkMu.RLock()
	fn := lockedSink
	sinkMu.RUnlock()
	return fn != nil && rec != nil && fn(collection, rec)
}

// Module holds the relying party and the app.
type Module struct {
	app core.App
	cfg Config
	wa  *webauthn.WebAuthn
	now func() time.Time

	ipLimit *limiter
	idLimit *limiter
}

// New builds the module for cfg (no hooks are bound). It fails on an invalid
// relying party configuration.
func New(app core.App, cfg Config) (*Module, error) {
	if !cfg.Active() {
		return nil, errors.New("passkey: " + EnvRPID + " is not set")
	}
	var web, opaque []string
	for _, o := range cfg.Origins {
		if protocol.IsOpaqueOrigin(o) {
			opaque = append(opaque, o)
		} else {
			web = append(web, o)
		}
	}
	yes := true
	wa, err := webauthn.New(&webauthn.Config{
		RPID:                  cfg.RPID,
		RPDisplayName:         cfg.RPName,
		RPOrigins:             web,
		RPOpaqueOrigins:       opaque,
		AttestationPreference: protocol.PreferNoAttestation,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			RequireResidentKey: &yes,
			ResidentKey:        protocol.ResidentKeyRequirementRequired,
			// registration accepts authenticators without UV (recorded as
			// uv_capable=false); login always requires UV (see loginOptions).
			UserVerification: protocol.VerificationPreferred,
		},
	})
	if err != nil {
		return nil, err
	}
	return &Module{app: app, cfg: cfg, wa: wa, now: time.Now, ipLimit: newLimiter(), idLimit: newLimiter()}, nil
}

// Register binds the module to app when the environment configures it, and
// returns nil (module inactive, endpoints 404, logged at boot) otherwise.
func Register(app core.App) *Module {
	cfg := LoadConfig()
	if !cfg.Active() {
		app.OnBootstrap().Bind(&hook.Handler[*core.BootstrapEvent]{
			Id: hookId, Priority: -1,
			Func: func(e *core.BootstrapEvent) error {
				if err := e.Next(); err != nil {
					return err
				}
				app.Logger().Info("passkey: inactive (set " + EnvRPID + " and " + EnvOrigins + " to enable)")
				return nil
			},
		})
		return nil
	}
	m, err := New(app, cfg)
	if err != nil {
		app.OnBootstrap().Bind(&hook.Handler[*core.BootstrapEvent]{
			Id: hookId, Priority: -1,
			Func: func(e *core.BootstrapEvent) error {
				if err := e.Next(); err != nil {
					return err
				}
				app.Logger().Error("passkey: invalid configuration, module inactive", "error", err)
				return nil
			},
		})
		return nil
	}
	m.Bind()
	return m
}

// Bind registers the bootstrap, delete-cascade and route hooks.
func (m *Module) Bind() {
	app := m.app
	init := func() {
		if err := EnsureCollection(app); err != nil {
			app.Logger().Error("passkey: failed to initialize the _passkeys collection", "error", err)
		}
		if _, err := app.AuxDB().NewQuery(createChallengesSQL).Execute(); err != nil {
			app.Logger().Error("passkey: failed to initialize the _passkey_challenges table", "error", err)
		}
		// upgrade tables created by earlier versions (ignore "duplicate column")
		_, _ = app.AuxDB().NewQuery("ALTER TABLE {{_passkey_challenges}} ADD COLUMN [[ip]] TEXT NOT NULL DEFAULT ''").Execute()
		_, _ = app.AuxDB().NewQuery("CREATE INDEX IF NOT EXISTS {{idx__passkey_challenges_ip}} ON {{_passkey_challenges}} ([[ip]])").Execute()
	}
	if app.IsBootstrapped() {
		init()
	}
	app.OnBootstrap().Bind(&hook.Handler[*core.BootstrapEvent]{
		Id: hookId, Priority: -1,
		Func: func(e *core.BootstrapEvent) error {
			if err := e.Next(); err != nil {
				return err
			}
			init()
			app.Logger().Info("passkey: active", "rpId", m.cfg.RPID, "origins", len(m.cfg.Origins), "clonePolicy", m.cfg.ClonePolicy)
			return nil
		},
	})

	// remove the passkeys of a deleted auth record
	app.OnRecordAfterDeleteSuccess().Bind(&hook.Handler[*core.RecordEvent]{
		Id: hookId,
		Func: func(e *core.RecordEvent) error {
			if err := e.Next(); err != nil {
				return err
			}
			if !e.Record.Collection().IsAuth() {
				return nil
			}
			rows, err := e.App.FindAllRecords(CollectionName, kernelHash(e.Record.Collection().Id, e.Record.Id))
			if err != nil {
				return nil
			}
			for _, r := range rows {
				if err := e.App.Delete(r); err != nil {
					e.App.Logger().Warn("passkey: failed to delete passkey of removed record", "error", err)
				}
			}
			return nil
		},
	})

	app.OnServe().Bind(&hook.Handler[*core.ServeEvent]{
		Id: hookId,
		Func: func(e *core.ServeEvent) error {
			m.bindRoutes(e.Router)
			return e.Next()
		},
	})
}

// EnsureCollection creates `_passkeys` (system, superuser-only rules) when missing.
func EnsureCollection(app kernel.App) error {
	if c, err := app.FindCollectionByNameOrId(CollectionName); err == nil {
		if c.Fields.GetByName("uv_capable") == nil { // upgrade
			c.Fields.Add(&kernel.BoolField{Name: "uv_capable"})
			return app.Save(c)
		}
		return nil
	}
	c := kernel.NewBaseCollection(CollectionName)
	c.System = true
	c.Fields.Add(
		&kernel.TextField{Name: "collection", Required: true, Max: 64},
		&kernel.TextField{Name: "record", Required: true, Max: 64},
		&kernel.TextField{Name: "credential_id", Required: true, Max: 2048},
		&kernel.TextField{Name: "public_key", Required: true, Hidden: true, Max: 8192},
		&kernel.TextField{Name: "aaguid", Max: 64},
		&kernel.NumberField{Name: "sign_count"},
		&kernel.JSONField{Name: "transports"},
		&kernel.BoolField{Name: "backup_eligible"},
		&kernel.BoolField{Name: "backup_state"},
		&kernel.BoolField{Name: "clone_suspected"},
		&kernel.BoolField{Name: "uv_capable"},
		&kernel.TextField{Name: "name", Max: 64},
		&kernel.DateField{Name: "last_used"},
		&kernel.AutodateField{Name: "created", OnCreate: true},
	)
	c.AddIndex("idx_passkeys_credential_id", true, "credential_id", "")
	c.AddIndex("idx_passkeys_owner", false, "collection, record", "")
	if err := app.Save(c); err != nil {
		if _, e2 := app.FindCollectionByNameOrId(CollectionName); e2 == nil {
			return nil
		}
		return err
	}
	return nil
}
