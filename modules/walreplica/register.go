//go:build !no_replica

package walreplica

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/hook"
)

const (
	bootstrapHookId = "__walreplicaBootstrap__"
	clearHookId     = "__walreplicaClear__"
)

// Register binds the replication lifecycle using the TOKI_REPLICA_* environment
// (read at bootstrap). With TOKI_REPLICA_URL unset the module does nothing.
func Register(app core.App) {
	register(app, nil)
}

// RegisterWithConfig is like [Register] with an explicit configuration.
func RegisterWithConfig(app core.App, cfg Config) {
	register(app, &cfg)
}

func register(app core.App, explicit *Config) {
	// Start after a successful bootstrap (the app has opened both databases).
	app.OnBootstrap().Bind(&hook.Handler[*core.BootstrapEvent]{
		Id:       bootstrapHookId,
		Priority: -1,
		Func: func(e *core.BootstrapEvent) error {
			if err := e.Next(); err != nil {
				return err
			}

			cfg := Config{}
			if explicit != nil {
				cfg = *explicit
				fillDefaults(&cfg)
			} else {
				var err error
				if cfg, err = FromEnv(); err != nil {
					return err
				}
			}
			if !cfg.Enabled() || skipCommand(os.Args[1:]) || get(e.App) != nil {
				return nil
			}

			return start(e.App, cfg)
		},
	})

	// Runs on app termination and on any ClearBootstrap: the replicator can
	// only be closed after the app closed its own connections, so its stop
	// code runs after e.Next() (which closes them).
	app.OnBootstrapClear().Bind(&hook.Handler[*core.BootstrapEvent]{
		Id:       clearHookId,
		Priority: -9999,
		Func: func(e *core.BootstrapEvent) error {
			e.App.Store().Remove(blockedKey)
			r := get(e.App)
			if r == nil {
				return e.Next()
			}

			// best effort final sync while the app connections are still open
			ctx, cancel := context.WithTimeout(context.Background(), shutdownSyncTimeout)
			if err := r.syncNow(ctx); err != nil {
				e.App.Logger().Error("walreplica: final sync failed", "error", err)
			}
			cancel()

			nextErr := e.Next() // closes the app database connections

			e.App.Store().Remove(storeKey)
			e.App.Store().Remove(kernel.StoreKeyDisableCheckpoint)
			if err := r.close(); err != nil {
				e.App.Logger().Error("walreplica: failed to stop replication cleanly", "error", err)
			} else {
				e.App.Logger().Info("walreplica: replication stopped")
			}
			return nextErr
		},
	})
}

func fillDefaults(c *Config) {
	if c.SyncInterval <= 0 {
		c.SyncInterval = DefaultSyncInterval
	}
	if c.Retention <= 0 {
		c.Retention = DefaultRetention
	}
	if c.SnapshotInterval <= 0 {
		c.SnapshotInterval = DefaultSnapshotInterval
	}
	if c.MaxMB < 0 {
		c.MaxMB = 0
	}
}

func start(app kernel.App, cfg Config) error {
	lease, err := acquireLease(cfg.URL, app.DataDir(), cfg.SyncInterval, time.Now())
	var held *leaseHeldError
	if errors.As(err, &held) {
		// another live node writes to this replica: serve, but do not replicate
		reason := held.Error()
		app.Store().Set(blockedKey, &blocked{reason: reason, holder: &held.holder})
		app.Logger().Error("walreplica: replication NOT started: "+reason+" (stop the other node or set "+EnvTakeover+"=1 to take over)",
			slog.String("url", redactURL(cfg.URL)), slog.String("holderNode", held.holder.NodeID))
		return nil
	}
	if err != nil {
		return err
	}
	if lease == nil {
		app.Logger().Warn("walreplica: lease guard is only implemented for file:// replicas; nothing prevents a second replicator on this url")
	}

	r, err := newReplicator(app, cfg)
	if err != nil {
		if lease != nil {
			lease.release()
		}
		return err
	}
	r.lease = lease
	if err := r.open(context.Background()); err != nil {
		_ = r.close()
		return err
	}
	if lease != nil {
		go lease.run(func(err error) {
			app.Logger().Error("walreplica: lease heartbeat failed", "error", err)
		})
	}

	app.Store().Set(storeKey, r)
	// litestream owns the WAL checkpoints; the app's nightly TRUNCATE
	// checkpoint would fight with its read lock
	app.Store().Set(kernel.StoreKeyDisableCheckpoint, true)

	app.Logger().Info("walreplica: replication started",
		slog.String("url", redactURL(cfg.URL)),
		slog.String("syncInterval", cfg.SyncInterval.String()),
		slog.String("retention", cfg.Retention.String()),
		slog.String("snapshotInterval", cfg.SnapshotInterval.String()),
		slog.Int64("maxMB", cfg.MaxMB),
	)
	return nil
}

// skipCommand reports the short lived CLI commands that must not start a
// second replicator next to a running server.
func skipCommand(args []string) bool {
	var cmd, sub string
	for _, a := range args {
		if len(a) > 0 && a[0] == '-' {
			continue
		}
		if cmd == "" {
			cmd = a
		} else {
			sub = a
			break
		}
	}
	switch cmd {
	case "superuser", "rule", "migrate", "version", "completion", "help", "mcp", "agent", "gen":
		return true
	case "replica":
		return sub != "snapshot"
	}
	return false
}
