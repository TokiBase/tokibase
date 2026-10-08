package tokibase

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/cmd"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/adminlock"
	"github.com/tokibase/tokibase/modules/audit"
	"github.com/tokibase/tokibase/modules/backupcheck"
	"github.com/tokibase/tokibase/modules/batchguard"
	"github.com/tokibase/tokibase/modules/computed"
	"github.com/tokibase/tokibase/modules/crypto"
	"github.com/tokibase/tokibase/modules/denylog"
	"github.com/tokibase/tokibase/modules/fieldperm"
	"github.com/tokibase/tokibase/modules/geo"
	"github.com/tokibase/tokibase/modules/jobs"
	"github.com/tokibase/tokibase/modules/lockout"
	"github.com/tokibase/tokibase/modules/mcp"
	"github.com/tokibase/tokibase/modules/nativeauth"
	"github.com/tokibase/tokibase/modules/passkey"
	"github.com/tokibase/tokibase/modules/payments"
	"github.com/tokibase/tokibase/modules/printer"
	"github.com/tokibase/tokibase/modules/push"
	"github.com/tokibase/tokibase/modules/roles"
	"github.com/tokibase/tokibase/modules/ruleguard"
	"github.com/tokibase/tokibase/modules/scanner"
	"github.com/tokibase/tokibase/modules/sessions"
	"github.com/tokibase/tokibase/modules/store/sqlite"
	toksync "github.com/tokibase/tokibase/modules/sync"
	"github.com/tokibase/tokibase/modules/timelint"
	"github.com/tokibase/tokibase/modules/tlscheck"
	"github.com/tokibase/tokibase/modules/totp"
	"github.com/tokibase/tokibase/modules/walreplica"
	"github.com/tokibase/tokibase/modules/wasm"
	"github.com/tokibase/tokibase/modules/webhooks"
	"github.com/tokibase/tokibase/tools/hook"
	"github.com/tokibase/tokibase/tools/list"
	"github.com/tokibase/tokibase/tools/osutils"
	"github.com/tokibase/tokibase/tools/routine"

	_ "github.com/tokibase/tokibase/migrations"
)

var _ core.App = (*PocketBase)(nil)

// Version of PocketBase
var Version = "(untracked)"

// PocketBase defines a PocketBase app launcher.
//
// It implements [core.App] via embedding and all of the app interface methods
// could be accessed directly through the instance (eg. PocketBase.DataDir()).
type PocketBase struct {
	core.App

	devFlag           bool
	dataDirFlag       string
	encryptionEnvFlag string
	queryTimeout      int
	hideStartBanner   bool

	// RootCmd is the main console command
	RootCmd *cobra.Command
}

// Config is the PocketBase initialization config struct.
type Config struct {
	// hide the default console server info on app startup
	HideStartBanner bool

	// optional default values for the console flags
	DefaultDev           bool
	DefaultDataDir       string // if not set, it will fallback to "./pb_data"
	DefaultEncryptionEnv string
	DefaultQueryTimeout  time.Duration // default to core.DefaultQueryTimeout (in seconds)

	// optional DB configurations
	DataMaxOpenConns int                // default to core.DefaultDataMaxOpenConns
	DataMaxIdleConns int                // default to core.DefaultDataMaxIdleConns
	AuxMaxOpenConns  int                // default to core.DefaultAuxMaxOpenConns
	AuxMaxIdleConns  int                // default to core.DefaultAuxMaxIdleConns
	DBConnect        core.DBConnectFunc // default to core.dbConnect

	// SkipFlagParse disables the eager parsing of os.Args for the global
	// flags (--dir, --dev, ...). Used by embedders (package embed) so the
	// host process arguments never leak into the app config.
	SkipFlagParse bool
}

// New creates a new PocketBase instance with the default configuration.
// Use [NewWithConfig] if you want to provide a custom configuration.
//
// Note that the application will not be initialized/bootstrapped yet,
// aka. DB connections, migrations, app settings, etc. will not be accessible.
// Everything will be initialized when [PocketBase.Start] is executed.
// If you want to initialize the application before calling [PocketBase.Start],
// then you'll have to manually call [PocketBase.Bootstrap].
func New() *PocketBase {
	_, isUsingGoRun := inspectRuntime()

	return NewWithConfig(Config{
		DefaultDev: isUsingGoRun,
	})
}

// NewWithConfig creates a new PocketBase instance with the provided config.
//
// Note that the application will not be initialized/bootstrapped yet,
// aka. DB connections, migrations, app settings, etc. will not be accessible.
// Everything will be initialized when [PocketBase.Start] is executed.
// If you want to initialize the application before calling [PocketBase.Start],
// then you'll have to manually call [PocketBase.Bootstrap].
func NewWithConfig(config Config) *PocketBase {
	// initialize a default data directory based on the executable baseDir
	if config.DefaultDataDir == "" {
		baseDir, _ := inspectRuntime()
		config.DefaultDataDir = filepath.Join(baseDir, "pb_data")
	}

	if config.DefaultQueryTimeout == 0 {
		config.DefaultQueryTimeout = core.DefaultQueryTimeout
	}

	executableName := filepath.Base(os.Args[0])

	pb := &PocketBase{
		RootCmd: &cobra.Command{
			Use:     executableName,
			Short:   executableName + " CLI",
			Version: Version,
			FParseErrWhitelist: cobra.FParseErrWhitelist{
				UnknownFlags: true,
			},
			// no need to provide the default cobra completion command
			CompletionOptions: cobra.CompletionOptions{
				DisableDefaultCmd: true,
			},
		},
		devFlag:           config.DefaultDev,
		dataDirFlag:       config.DefaultDataDir,
		encryptionEnvFlag: config.DefaultEncryptionEnv,
		hideStartBanner:   config.HideStartBanner,
	}

	// don't write command errors to the stderr because the error is
	// propagated back to the app.Start() and could result in duplication
	pb.RootCmd.SetErr(&nopWrite{})

	// parse base flags
	// (errors are ignored, since the full flags parsing happens on Execute())
	if config.SkipFlagParse {
		pb.RootCmd.PersistentFlags().StringVar(&pb.dataDirFlag, "dir", config.DefaultDataDir, "the data directory")
		pb.RootCmd.PersistentFlags().IntVar(&pb.queryTimeout, "queryTimeout", int(config.DefaultQueryTimeout.Seconds()), "")
	} else {
		pb.eagerParseFlags(&config)
	}

	// initialize the app instance
	pb.App = core.NewBaseApp(core.BaseAppConfig{
		IsDev:            pb.devFlag,
		DataDir:          pb.dataDirFlag,
		EncryptionEnv:    pb.encryptionEnvFlag,
		QueryTimeout:     time.Duration(pb.queryTimeout) * time.Second,
		DataMaxOpenConns: config.DataMaxOpenConns,
		DataMaxIdleConns: config.DataMaxIdleConns,
		AuxMaxOpenConns:  config.AuxMaxOpenConns,
		AuxMaxIdleConns:  config.AuxMaxIdleConns,
		DBConnect:        config.DBConnect,
	})

	// make public ("") API rules explicit (policy file: pb_data/ruleguard.json)
	ruleguard.Register(pb.App.(core.App))
	// audit log (disable with TOKI_AUDIT=off)
	var auditLog *audit.Log
	if audit.Enabled() {
		auditLog = audit.Register(pb.App)
	}

	// Admin UI mode: TOKI_ADMIN_UI=on|readonly|off (default on)
	adminlock.Register(pb.App.(core.App))
	if auditLog != nil {
		adminlock.SetAuditSink(func(b adminlock.Block) {
			req, _ := json.Marshal(map[string]any{
				"method": b.Method, "path": b.Path, "ip": b.IP, "user_agent": b.UserAgent,
			})
			reqStr := string(req)
			err := auditLog.Append(&audit.Entry{
				ActorKind: "superuser", ActorID: b.ActorID, ActorCollection: b.ActorColl,
				Action: adminlock.ActionBlocked, Collection: b.Collection,
				Record: b.Record + ":" + b.Action, Request: &reqStr,
			})
			if err != nil {
				pb.App.Logger().Warn("audit: failed to record admin.blocked", "error", err)
			}
		})
	}

	// progressive per-identity lockout of failed password/OTP auth (TOKI_LOCKOUT=off disables)
	if lockout.Enabled() {
		lockout.Register(pb.App.(core.App))
		if auditLog != nil {
			lockout.SetAuditSink(func(action, collection, record string, details map[string]any) {
				after, _ := json.Marshal(details)
				afterStr := string(after)
				if err := auditLog.Append(&audit.Entry{
					ActorKind: "system", Action: action, Collection: collection,
					Record: record, After: &afterStr,
				}); err != nil {
					pb.Logger().Warn("audit: failed to record "+action, "error", err)
				}
			})
		}
	}

	// server-side sessions: sid claim, revocation, rotation (TOKI_SESSIONS=off disables)
	if sessions.Enabled() {
		sessions.Register(pb.App.(core.App))
		if auditLog != nil {
			sessions.SetAuditSink(func(action, collection, record string, details map[string]any) {
				after, _ := json.Marshal(details)
				afterStr := string(after)
				if err := auditLog.Append(&audit.Entry{
					ActorKind: "system", Action: action, Collection: collection,
					Record: record, After: &afterStr,
				}); err != nil {
					pb.Logger().Warn("audit: failed to record "+action, "error", err)
				}
			})
		}
	}

	// WebAuthn passkeys: active only when TOKI_PASSKEY_RP_ID is set (see docs/modules/passkey.md)
	passkey.Register(pb.App.(core.App))
	passkey.SetFailureSink(lockout.RecordFailureFor)
	passkey.SetLockedSink(lockout.IsLocked)
	if auditLog != nil {
		passkey.SetAuditSink(func(action, collection, record string, details map[string]any) {
			after, _ := json.Marshal(details)
			afterStr := string(after)
			if err := auditLog.Append(&audit.Entry{
				ActorKind: "system", Action: action, Collection: collection,
				Record: record, After: &afterStr,
			}); err != nil {
				pb.Logger().Warn("audit: failed to record "+action, "error", err)
			}
		})
	}

	// TOTP (RFC 6238) as an MFA method plus recovery codes and enforcement (see docs/modules/totp.md)
	totp.Register(pb.App.(core.App))
	totp.SetFailureSink(lockout.RecordFailureFor)
	totp.SetLockedSink(lockout.IsLocked)
	if auditLog != nil {
		totp.SetAuditSink(func(action, collection, record string, details map[string]any) {
			after, _ := json.Marshal(details)
			afterStr := string(after)
			if err := auditLog.Append(&audit.Entry{
				ActorKind: "system", Action: action, Collection: collection,
				Record: record, After: &afterStr,
			}); err != nil {
				pb.Logger().Warn("audit: failed to record "+action, "error", err)
			}
		})
	}

	// native Google/Apple ID token sign-in mapped to OAuth2 external auths (TOKI_NATIVEAUTH=off disables; see docs/modules/nativeauth.md)
	if nativeauth.Enabled() {
		nativeauth.Register(pb.App.(core.App))
		nativeauth.SetFailureSink(lockout.RecordFailureFor)
		nativeauth.SetLockedSink(lockout.IsLocked)
		if auditLog != nil {
			nativeauth.SetAuditSink(func(action, collection, record string, details map[string]any) {
				after, _ := json.Marshal(details)
				afterStr := string(after)
				if err := auditLog.Append(&audit.Entry{
					ActorKind: "system", Action: action, Collection: collection,
					Record: record, After: &afterStr,
				}); err != nil {
					pb.Logger().Warn("audit: failed to record "+action, "error", err)
				}
			})
		}
	}

	// durable job queue (TOKI_JOBS=off disables; TOKI_JOBS_WORKERS, TOKI_ROLE=worker)
	if jobs.Enabled() {
		jobs.Register(pb.App.(core.App))
		if auditLog != nil {
			jobs.SetAuditSink(func(action, collection, record string, details map[string]any) {
				after, _ := json.Marshal(details)
				afterStr := string(after)
				if err := auditLog.Append(&audit.Entry{
					ActorKind: "system", Action: action, Collection: collection, Record: record, After: &afterStr,
				}); err != nil {
					pb.Logger().Warn("audit: failed to record "+action, "error", err)
				}
			})
		}
	}

	// per-field read/write rules in _field_rules (see docs/modules/fieldperm.md)
	fieldperm.Register(pb.App.(core.App))
	if auditLog != nil {
		fieldperm.SetAuditSink(func(action, collection, record string, details map[string]any) {
			after, _ := json.Marshal(details)
			afterStr := string(after)
			if err := auditLog.Append(&audit.Entry{
				ActorKind: "system", Action: action, Collection: collection,
				Record: record, After: &afterStr,
			}); err != nil {
				pb.Logger().Warn("audit: failed to record "+action, "error", err)
			}
		})
	}

	// cross-record validation of /api/batch in _batch_rules (see docs/modules/batchguard.md)
	batchguard.Register(pb.App.(core.App))

	// server-maintained counters and rollups in _computed_fields (see docs/modules/computed.md)
	computed.Register(pb.App.(core.App))
	if auditLog != nil {
		computed.SetAuditSink(func(action, collection, record string, details map[string]any) {
			after, _ := json.Marshal(details)
			afterStr := string(after)
			if err := auditLog.Append(&audit.Entry{
				ActorKind: "system", Action: action, Collection: collection,
				Record: record, After: &afterStr,
			}); err != nil {
				pb.Logger().Warn("audit: failed to record "+action, "error", err)
			}
		})
	}

	// per-field encryption at rest (inactive without a master key, see docs/modules/crypto.md)
	crypto.Register(pb.App.(core.App))
	if auditLog != nil {
		crypto.SetAuditSink(func(action, collection, record string, details map[string]any) {
			after, _ := json.Marshal(details)
			afterStr := string(after)
			if err := auditLog.Append(&audit.Entry{
				ActorKind: "system", Action: action, Collection: collection,
				Record: record, After: &afterStr,
			}); err != nil {
				pb.Logger().Warn("audit: failed to record "+action, "error", err)
			}
		})
	}

	// hub/spoke change capture (TOKI_SYNC_ROLE=off|hub|spoke, default off, see docs/modules/sync.md)
	toksync.Register(pb.App.(core.App))
	if auditLog != nil {
		toksync.SetAuditSink(func(action, collection, record string, details map[string]any) {
			en := &audit.Entry{ActorKind: audit.ActorSystem, Action: action, Collection: collection, Record: record}
			// sync entries carry the ORIGINAL actor and the request that triggered them
			if k, _ := details["actor_kind"].(string); k != "" {
				en.ActorKind = k
				en.ActorID, _ = details["actor_id"].(string)
				en.ActorCollection, _ = details["actor_collection"].(string)
			}
			if req, ok := details["request"]; ok {
				rb, _ := json.Marshal(req)
				rs := string(rb)
				en.Request = &rs
				rest := make(map[string]any, len(details))
				for k, v := range details {
					if k != "request" {
						rest[k] = v
					}
				}
				details = rest
			}
			after, _ := json.Marshal(details)
			afterStr := string(after)
			en.After = &afterStr
			if err := auditLog.Append(en); err != nil {
				pb.App.Logger().Warn("audit: failed to record "+action, "error", err)
			}
		})
	}

	// warn when serving plain HTTP on a reachable address without trusted proxy headers (TOKI_TLS_CHECK=warn|strict|off)
	tlscheck.Register(pb.App.(core.App))

	// date values without a time zone: TOKI_TIMELINT=off|warn|strict (default warn)
	timelint.Register(pb.App.(core.App))
	geo.Register(pb.App.(core.App))

	// named roles and scoped memberships, rule functions @role()/@member() (see docs/modules/roles.md)
	roles.Register(pb.App.(core.App))

	// structured logs for every 401/403/429 response (TOKI_DENYLOG=off disables)
	denylog.Register(pb.App.(core.App))

	// verify every created backup by restoring it to a temp dir (TOKI_BACKUP_VERIFY=off disables)
	backupcheck.Register(pb.App.(core.App))
	if auditLog != nil {
		// record every automatic verification result in the audit log
		backupcheck.OnResult = func(app kernel.App, r backupcheck.Report) {
			after, _ := json.Marshal(r)
			afterStr := string(after)
			status := "ok"
			if !r.OK() {
				status = "failed"
			}
			err := auditLog.Append(&audit.Entry{
				ActorKind:  "system",
				Action:     "backup.verify",
				Collection: "",
				Record:     r.Name + ":" + status,
				After:      &afterStr,
			})
			if err != nil {
				app.Logger().Warn("audit: failed to record backup.verify", "error", err)
			}
		}
	}

	// outbound webhooks with signatures, retries and dead-letter (TOKI_WEBHOOKS=off disables)
	if webhooks.Enabled() {
		webhooks.Register(pb.App.(core.App))
		if auditLog != nil {
			webhooks.SetAuditSink(func(action, collection, record string, details map[string]any) {
				after, _ := json.Marshal(details)
				afterStr := string(after)
				if err := auditLog.Append(&audit.Entry{
					ActorKind: "system", Action: action, Collection: collection,
					Record: record, After: &afterStr,
				}); err != nil {
					pb.Logger().Warn("audit: failed to record "+action, "error", err)
				}
			})
		}
	}

	// ESC/POS receipt and ticket printing through durable jobs (opt-in: TOKI_PRINTER=on)
	if printer.Enabled() {
		printer.Register(pb.App.(core.App))
		if auditLog != nil {
			printer.SetAuditSink(func(action, collection, record string, details map[string]any) {
				after, _ := json.Marshal(details)
				afterStr := string(after)
				if err := auditLog.Append(&audit.Entry{
					ActorKind: "system", Action: action, Collection: collection,
					Record: record, After: &afterStr,
				}); err != nil {
					pb.Logger().Warn("audit: failed to record "+action, "error", err)
				}
			})
		}
	}

	// barcode/QR scanner ingestion: serial, evdev and web wedge (opt in with TOKI_SCANNER=on)
	if scanner.Enabled() {
		scanner.Register(pb.App.(core.App))
	}

	// provider-neutral payments: webhooks, intents, entitlements (TOKI_PAYMENTS=off disables)
	if payments.Enabled() {
		payments.Register(pb.App.(core.App))
		if auditLog != nil {
			payments.SetAuditSink(func(action, collection, record string, details map[string]any) {
				after, _ := json.Marshal(details)
				afterStr := string(after)
				if err := auditLog.Append(&audit.Entry{
					ActorKind: "system", Action: action, Collection: collection,
					Record: record, After: &afterStr,
				}); err != nil {
					pb.Logger().Warn("audit: failed to record "+action, "error", err)
				}
			})
		}
	}

	// sandboxed WASM hooks from pb_hooks_wasm/ (TOKI_WASM=off or -tags no_wasm disables)
	wasm.Register(pb.App.(core.App), pb.RootCmd)

	// push notifications to FCM/APNs through the job queue (TOKI_PUSH=off disables)
	if push.Enabled() {
		push.Register(pb.App.(core.App))
		if auditLog != nil {
			push.SetAuditSink(func(action, collection, record string, details map[string]any) {
				after, _ := json.Marshal(details)
				afterStr := string(after)
				if err := auditLog.Append(&audit.Entry{
					ActorKind: "system", Action: action, Collection: collection,
					Record: record, After: &afterStr,
				}); err != nil {
					pb.Logger().Warn("audit: failed to record "+action, "error", err)
				}
			})
		}
	}

	// continuous WAL replication of data.db/auxiliary.db (inactive unless TOKI_REPLICA_URL is set)
	walreplica.Register(pb.App.(core.App))
	if auditLog != nil {
		// `toki replica promote` runs without a bootstrapped app: write the entry into the promoted database
		walreplica.SetAuditSink(func(r walreplica.PromoteResult) {
			tmp := core.NewBaseApp(core.BaseAppConfig{DataDir: r.Dir})
			if err := tmp.Bootstrap(); err != nil {
				fmt.Fprintln(os.Stderr, "audit: failed to record replica.promote:", err)
				return
			}
			defer tmp.ResetBootstrapState()
			after, _ := json.Marshal(r)
			afterStr := string(after)
			l := audit.New(tmp)
			err := l.Init()
			if err == nil {
				err = l.Append(&audit.Entry{ActorKind: "system", Action: "replica.promote", Record: r.FromURL, After: &afterStr})
			}
			if err != nil {
				fmt.Fprintln(os.Stderr, "audit: failed to record replica.promote:", err)
			}
		})
	}

	// MCP server for AI agents: `_agents` identities, stdio transport always, streamable HTTP at /api/mcp with TOKI_MCP=on
	mcp.Register(pb.App.(core.App))
	mcp.RegisterHTTP(pb.App.(core.App)) // /api/mcp, only with TOKI_MCP=on
	if auditLog != nil {
		mcp.SetAuditSink(func(action, collection, record string, details map[string]any) {
			after, _ := json.Marshal(details)
			afterStr := string(after)
			en := &audit.Entry{
				ActorKind: audit.ActorAgent, ActorCollection: mcp.CollectionName,
				Action: action, Collection: collection, Record: record, After: &afterStr,
			}
			if id, _ := details["agent_id"].(string); id != "" {
				en.ActorID = id
			}
			if cli, _ := details["cli"].(bool); cli {
				// created/revoked from the command line by a human operator
				en.ActorKind, en.ActorID, en.ActorCollection = audit.ActorSystem, "", ""
			}
			if err := auditLog.Append(en); err != nil {
				pb.App.Logger().Warn("audit: failed to record "+action, "error", err)
			}
		})
	}
	mcp.SetProviders(mcpProviders(auditLog != nil))

	// hide the default help command (allow only `--help` flag)
	pb.RootCmd.SetHelpCommand(&cobra.Command{Hidden: true})

	// refuse to start when a compiled-out (no_<module>) module still owns data or env config
	kernel.BindStubbedModuleGuard(pb.App)

	// https://github.com/tokibase/tokibase/issues/6136
	pb.OnBootstrap().Bind(&hook.Handler[*core.BootstrapEvent]{
		Id: ModerncDepsCheckHookId,
		Func: func(be *core.BootstrapEvent) error {
			if err := be.Next(); err != nil {
				return err
			}

			// run separately to avoid blocking
			app := be.App
			routine.FireAndForget(func() {
				sqlite.CheckModerncDeps(app)
			})

			return nil
		},
	})

	return pb
}

// Start starts the application, aka. registers the default system
// commands (serve, superuser, version) and executes pb.RootCmd.
func (pb *PocketBase) Start() error {
	// register system commands
	pb.RootCmd.AddCommand(cmd.NewSuperuserCommand(pb))
	pb.RootCmd.AddCommand(cmd.NewRuleCommand(pb))
	pb.RootCmd.AddCommand(cmd.NewBackupCommand(pb))
	pb.RootCmd.AddCommand(cmd.NewDBCommand(pb))
	pb.RootCmd.AddCommand(cmd.NewReplicaCommand(pb))
	pb.RootCmd.AddCommand(cmd.NewServeCommand(pb, !pb.hideStartBanner))
	if audit.Enabled() {
		pb.RootCmd.AddCommand(audit.NewCommand(pb))
	}
	if webhooks.Enabled() {
		pb.RootCmd.AddCommand(webhooks.NewCommand(pb))
	}
	pb.RootCmd.AddCommand(scanner.NewCommand(pb))
	if push.Enabled() {
		pb.RootCmd.AddCommand(push.NewCommand(pb))
	}
	if printer.Enabled() {
		pb.RootCmd.AddCommand(printer.NewCommand(pb))
	}
	pb.RootCmd.AddCommand(cmd.NewLockoutCommand(pb))
	pb.RootCmd.AddCommand(cmd.NewSessionsCommand(pb))
	pb.RootCmd.AddCommand(passkey.NewCommand(pb))
	pb.RootCmd.AddCommand(totp.NewCommand(pb))
	if jobs.Enabled() {
		pb.RootCmd.AddCommand(jobs.NewCommand(pb))
	}
	pb.RootCmd.AddCommand(cmd.NewTimeCommand(pb))
	pb.RootCmd.AddCommand(cmd.NewDenyCommand(pb))
	pb.RootCmd.AddCommand(cmd.NewFieldPermCommand(pb))
	pb.RootCmd.AddCommand(computed.NewCommand(pb))
	pb.RootCmd.AddCommand(toksync.NewCommand(pb))
	pb.RootCmd.AddCommand(batchguard.NewCommand(pb))
	pb.RootCmd.AddCommand(crypto.NewCommand(pb))
	pb.RootCmd.AddCommand(geo.NewCommand(pb))
	pb.RootCmd.AddCommand(roles.NewCommand(pb))
	if payments.Enabled() {
		pb.RootCmd.AddCommand(payments.NewCommand(pb))
	}
	if c := wasm.NewCommand(pb); c != nil {
		pb.RootCmd.AddCommand(c)
	}
	for _, c := range mcp.NewCommands(pb) {
		pb.RootCmd.AddCommand(c)
	}

	return pb.Execute()
}

// Execute initializes the application (if not already) and executes
// the pb.RootCmd with graceful shutdown support.
//
// This method differs from pb.Start() by not registering the default
// system commands!
func (pb *PocketBase) Execute() error {
	if !pb.skipBootstrap() {
		if err := pb.Bootstrap(); err != nil {
			return err
		}
	}

	execCh := make(chan error, 1)
	sigCh := make(chan os.Signal, 1)

	// listen for interrupt signal to gracefully shutdown the application
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	// execute the root command
	go func() {
		execCh <- routine.SafeWrap(pb.RootCmd.Execute)()
	}()

	// wait for either an OS signal or the command to complete
	var execErr error
	select {
	case <-sigCh:
	case execErr = <-execCh:
	}

	signal.Stop(sigCh)

	// trigger cleanups
	//
	// @todo consider skipping and just call the finalizer in case OnTerminate was already invoked manually?
	event := new(core.TerminateEvent)
	event.App = pb
	return pb.OnTerminate().Trigger(event, func(e *core.TerminateEvent) error {
		return errors.Join(e.App.ClearBootstrap(), execErr)
	})
}

// eagerParseFlags parses the global app flags before calling pb.RootCmd.Execute().
// so we can have all PocketBase flags ready for use on initialization.
func (pb *PocketBase) eagerParseFlags(config *Config) error {
	pb.RootCmd.PersistentFlags().StringVar(
		&pb.dataDirFlag,
		"dir",
		config.DefaultDataDir,
		"the PocketBase data directory",
	)

	pb.RootCmd.PersistentFlags().StringVar(
		&pb.encryptionEnvFlag,
		"encryptionEnv",
		config.DefaultEncryptionEnv,
		"the env variable whose value of 32 characters will be used \nas encryption key for the app settings (default none)",
	)

	pb.RootCmd.PersistentFlags().BoolVar(
		&pb.devFlag,
		"dev",
		config.DefaultDev,
		"enable dev mode, aka. printing logs and sql statements to the console",
	)

	pb.RootCmd.PersistentFlags().IntVar(
		&pb.queryTimeout,
		"queryTimeout",
		int(config.DefaultQueryTimeout.Seconds()),
		"the default SELECT queries timeout in seconds",
	)

	return pb.RootCmd.ParseFlags(os.Args[1:])
}

// skipBootstrap eagerly checks if the app should skip the bootstrap process:
// - already bootstrapped
// - is unknown command
// - is the default help command
// - is the default version command
//
// https://github.com/tokibase/tokibase/issues/404
// https://github.com/tokibase/tokibase/discussions/1267
func (pb *PocketBase) skipBootstrap() bool {
	flags := []string{
		"-h",
		"--help",
		"-v",
		"--version",
	}

	if pb.IsBootstrapped() {
		return true // already bootstrapped
	}

	found, _, err := pb.RootCmd.Find(os.Args[1:])
	if err != nil {
		return true // unknown command
	}

	// commands that work on a replica or a not yet existing data dir
	if found.Annotations[cmd.AnnotationSkipBootstrap] == "true" {
		return true
	}

	for _, arg := range os.Args {
		if !list.ExistInSlice(arg, flags) {
			continue
		}

		// ensure that there is no user defined flag with the same name/shorthand
		trimmed := strings.TrimLeft(arg, "-")
		if len(trimmed) > 1 && found.Flags().Lookup(trimmed) == nil {
			return true
		}
		if len(trimmed) == 1 && found.Flags().ShorthandLookup(trimmed) == nil {
			return true
		}
	}

	return false
}

// inspectRuntime tries to find the base executable directory and how it was run.
//
// note: we are using os.Args[0] and not os.Executable() since it could
// break existing aliased binaries (eg. the community maintained homebrew package)
func inspectRuntime() (baseDir string, withGoRun bool) {
	if osutils.IsProbablyGoRun() {
		// probably ran with go run
		withGoRun = true
		baseDir, _ = os.Getwd()
	} else {
		// probably ran with go build
		withGoRun = false
		baseDir = filepath.Dir(os.Args[0])
	}
	return
}

var _ io.Writer = (*nopWrite)(nil)

type nopWrite struct{}

func (w *nopWrite) Write(p []byte) (n int, err error) {
	return
}
