package tokibase

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/modules/adminlock"
	"github.com/tokibase/tokibase/modules/audit"
	"github.com/tokibase/tokibase/modules/backupcheck"
	"github.com/tokibase/tokibase/modules/denylog"
	"github.com/tokibase/tokibase/modules/lockout"
	"github.com/tokibase/tokibase/modules/mcp"
	"github.com/tokibase/tokibase/modules/ruleguard"
	"github.com/tokibase/tokibase/modules/timelint"
	"github.com/tokibase/tokibase/modules/walreplica"
)

// mcpProviders connects the MCP tools to the other modules (modules never
// import each other, the root package wires them).
func mcpProviders(auditOn bool) mcp.Providers {
	p := mcp.Providers{
		Version: Version,
		Profile: "solo",
		Modules: enabledModules(auditOn),
		RuleLint: func(app core.App) (any, error) {
			pol, err := ruleguard.Load(app.DataDir())
			if err != nil {
				return nil, err
			}
			return ruleguard.Lint(app, pol)
		},
		BackupVerify: func(ctx context.Context, app core.App, name string) (any, error) {
			if name == "" {
				var err error
				if name, err = backupcheck.Latest(ctx, app); err != nil {
					return nil, err
				}
			}
			return backupcheck.Verify(ctx, app, name)
		},
		ReplicaStatus: func(app core.App) (any, error) {
			if !walreplica.Active(app) {
				return map[string]any{"active": false}, nil
			}
			healthy, reason := walreplica.Healthy(app)
			return map[string]any{
				"active": true, "healthy": healthy, "reason": reason, "databases": walreplica.Status(app),
			}, nil
		},
	}
	if auditOn {
		p.AuditTail = func(app core.App, since time.Time, limit int) (any, error) {
			return audit.Query(app, since, limit, true)
		}
		p.AuditVerify = func(app core.App) (any, error) { return audit.Verify(app) }
	}
	if denylog.Enabled() {
		p.DenyTail = func(app core.App, since time.Duration, limit int) (any, error) {
			return denylog.Tail(app, since, limit)
		}
	}
	if lockout.Enabled() {
		p.LockoutList = func(app core.App) (any, error) { return lockout.List(app) }
	}
	return p
}

func enabledModules(auditOn bool) []string {
	mods := []string{"ruleguard", "backupcheck", "mcp"}
	if auditOn {
		mods = append(mods, "audit")
	}
	if lockout.Enabled() {
		mods = append(mods, "lockout")
	}
	if denylog.Enabled() {
		mods = append(mods, "denylog")
	}
	if m := adminlock.ModeFromEnv(); m != "" {
		mods = append(mods, fmt.Sprintf("adminlock (%v)", m))
	}
	if pol := timelint.PolicyFromEnv(); fmt.Sprint(pol) != "off" {
		mods = append(mods, fmt.Sprintf("timelint (%v)", pol))
	}
	if strings.TrimSpace(os.Getenv("TOKI_REPLICA_URL")) != "" {
		mods = append(mods, "walreplica")
	}
	return mods
}
