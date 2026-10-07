//go:build no_fieldperm

// Package fieldperm is compiled out of this binary by the no_fieldperm build tag.
// This file keeps the surface used by the root package so the wiring compiles.
package fieldperm

import "github.com/tokibase/tokibase/core"

// Register is a no-op in builds with the no_fieldperm tag.
func Register(app core.App) {}

// SetAuditSink is a no-op in builds with the no_fieldperm tag.
func SetAuditSink(fn func(action, collection, record string, details map[string]any)) {}
