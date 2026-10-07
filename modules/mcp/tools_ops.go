//go:build !no_mcp

package mcp

import (
	"fmt"
	"strings"
	"time"
)

type tailIn struct {
	Since string `json:"since,omitempty" jsonschema:"duration (90m, 1h, 7d) or date (2026-10-01, RFC3339); default all retained"`
	Limit int    `json:"limit,omitempty" jsonschema:"max entries, default 50, max 500"`
}

type backupIn struct {
	Name string `json:"name,omitempty" jsonschema:"backup file name or latest (default)"`
}

// parseSince accepts 90m, 1h, 7d, 2026-10-01 or RFC3339; "" is the zero time.
func parseSince(v string, now time.Time) (time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, nil
	}
	if strings.HasSuffix(v, "d") {
		var n int
		if _, err := fmt.Sscanf(v, "%dd", &n); err == nil && n >= 0 {
			return now.Add(-time.Duration(n) * 24 * time.Hour), nil
		}
	}
	if d, err := time.ParseDuration(v); err == nil {
		return now.Add(-d), nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid since %q (use 90m, 1h, 7d, 2026-10-01 or RFC3339)", v)
}

func clampLimit(n int) int {
	if n <= 0 {
		return 50
	}
	if n > 500 {
		return 500
	}
	return n
}

func (s *Server) registerOpsTools() {
	addTool(s, "audit.tail", "Newest entries of the hash-chained audit log (operator only).",
		RoleOperator, false, func(c *call, in tailIn) (map[string]any, error) {
			p := getProviders().AuditTail
			if p == nil {
				return nil, errModuleOff("audit")
			}
			since, err := parseSince(in.Since, s.now())
			if err != nil {
				return nil, err
			}
			res, err := p(s.app, since, clampLimit(in.Limit))
			if err != nil {
				return nil, s.internal(c, "provider", err)
			}
			return map[string]any{"entries": jsonValue(res)}, nil
		})

	addTool(s, "audit.verify", "Verify the audit hash chain (operator only).",
		RoleOperator, false, func(c *call, _ emptyIn) (map[string]any, error) {
			p := getProviders().AuditVerify
			if p == nil {
				return nil, errModuleOff("audit")
			}
			res, err := p(s.app)
			if err != nil {
				return nil, s.internal(c, "provider", err)
			}
			return map[string]any{"result": jsonValue(res)}, nil
		})

	addTool(s, "backup.verify", "Restore a backup to a temp dir and verify it (integrity, counts, files); default the latest (operator only).",
		RoleOperator, false, func(c *call, in backupIn) (map[string]any, error) {
			p := getProviders().BackupVerify
			if p == nil {
				return nil, errModuleOff("backupcheck")
			}
			name := strings.TrimSpace(in.Name)
			if name == "latest" {
				name = ""
			}
			c.set("name", name)
			res, err := p(c.ctx, s.app, name)
			if err != nil {
				return nil, s.internal(c, "provider", err)
			}
			return map[string]any{"report": jsonValue(res)}, nil
		})

	addTool(s, "replica.status", "WAL replication status per database (operator only).",
		RoleOperator, false, func(c *call, _ emptyIn) (map[string]any, error) {
			p := getProviders().ReplicaStatus
			if p == nil {
				return nil, errModuleOff("walreplica")
			}
			res, err := p(s.app)
			if err != nil {
				return nil, s.internal(c, "provider", err)
			}
			return map[string]any{"status": jsonValue(res)}, nil
		})

	addTool(s, "deny.tail", "Newest 401/403/429 denial log entries, newest first (operator only).",
		RoleOperator, false, func(c *call, in tailIn) (map[string]any, error) {
			p := getProviders().DenyTail
			if p == nil {
				return nil, errModuleOff("denylog")
			}
			since, err := parseSince(in.Since, s.now())
			if err != nil {
				return nil, err
			}
			var d time.Duration
			if !since.IsZero() {
				d = s.now().Sub(since)
			}
			res, err := p(s.app, d, clampLimit(in.Limit))
			if err != nil {
				return nil, s.internal(c, "provider", err)
			}
			return map[string]any{"entries": jsonValue(res)}, nil
		})

	addTool(s, "lockout.list", "Identities currently tracked by the failed-login lockout (hashed keys, operator only).",
		RoleOperator, false, func(c *call, _ emptyIn) (map[string]any, error) {
			p := getProviders().LockoutList
			if p == nil {
				return nil, errModuleOff("lockout")
			}
			res, err := p(s.app)
			if err != nil {
				return nil, s.internal(c, "provider", err)
			}
			return map[string]any{"entries": jsonValue(res)}, nil
		})
}
