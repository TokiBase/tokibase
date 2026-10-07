// Package mcp is a Model Context Protocol server that lets AI agents inspect
// and manage a TokiBase instance through typed tools, with their own identity
// (`_agents`), rule enforcement and audit.
//
// Modules never import each other: the root package wires the instance data
// providers (SetProviders) and the audit log (SetAuditSink).
package mcp

import (
	"context"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/tokibase/tokibase/core"
)

// CollectionName is the system collection holding agent identities.
const CollectionName = "_agents"

// Providers connects the MCP tools to other modules. Every field is optional:
// a nil provider makes the matching tool answer "module not enabled".
// Results must be JSON serializable.
type Providers struct {
	Version string   // build version
	Profile string   // product profile, for toki://instance
	Modules []string // enabled modules, for toki://instance and AGENTS.md

	RuleLint      func(app core.App) (any, error)
	AuditTail     func(app core.App, since time.Time, limit int) (any, error) // newest first
	AuditVerify   func(app core.App) (any, error)
	BackupVerify  func(ctx context.Context, app core.App, name string) (any, error) // name "" = latest
	ReplicaStatus func(app core.App) (any, error)
	DenyTail      func(app core.App, since time.Duration, limit int) (any, error)
	LockoutList   func(app core.App) (any, error)
}

var (
	cfgMu     sync.RWMutex
	providers Providers
	auditSink func(action, collection, record string, details map[string]any)
)

// SetProviders wires the other modules (called by tokibase.go).
func SetProviders(p Providers) {
	cfgMu.Lock()
	providers = p
	cfgMu.Unlock()
}

func getProviders() Providers {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	return providers
}

// SetAuditSink connects agent actions to an external audit log.
// Modules must not import each other, so the wiring happens in tokibase.go.
//
// Actions are `agent.<tool>` (e.g. `agent.records.create`) plus
// `agent.created` / `agent.revoked` for the CLI (details["cli"] == true).
// Details always carry agent, agent_id, role, session.
func SetAuditSink(fn func(action, collection, record string, details map[string]any)) {
	cfgMu.Lock()
	auditSink = fn
	cfgMu.Unlock()
}

func emit(action, collection, record string, details map[string]any) {
	cfgMu.RLock()
	fn := auditSink
	cfgMu.RUnlock()
	if fn != nil {
		fn(action, collection, record, details)
	}
}

// HTTPEnabled reports whether the (future, PR 2) HTTP transport may be
// registered. Reserved: env TOKI_MCP=off disables it. PR 1 only has stdio.
func HTTPEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TOKI_MCP"))) {
	case "off", "false", "0", "disabled":
		return false
	}
	return true
}
