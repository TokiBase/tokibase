//go:build no_lockout

// Package lockout is compiled out of this binary by the no_lockout build tag.
package lockout

import "github.com/tokibase/tokibase/core"

// Row is the lockout record shape used by the MCP provider wiring.
type Row struct{}

// Enabled reports false in builds with the no_lockout tag.
func Enabled() bool { return false }

// Register is a no-op in builds with the no_lockout tag.
func Register(app core.App) {}

// SetAuditSink is a no-op in builds with the no_lockout tag.
func SetAuditSink(fn func(action, collection, record string, details map[string]any)) {}

// IsLocked always reports false in builds with the no_lockout tag.
func IsLocked(collection string, rec *core.Record) bool { return false }

// RecordFailureFor is a no-op in builds with the no_lockout tag.
func RecordFailureFor(collection string, rec *core.Record) {}

// List returns no rows in builds with the no_lockout tag.
func List(app core.App) ([]Row, error) { return nil, nil }
