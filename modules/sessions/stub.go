//go:build no_sessions

// Package sessions is compiled out of this binary by the no_sessions build tag.
// This file keeps the surface used by the root package so the wiring compiles.
package sessions

import "github.com/tokibase/tokibase/core"

// Register is a no-op in builds with the no_sessions tag.
func Register(app core.App) {}

// SetAuditSink is a no-op in builds with the no_sessions tag.
func SetAuditSink(fn func(action, collection, record string, details map[string]any)) {}

func Enabled() bool { return false }
