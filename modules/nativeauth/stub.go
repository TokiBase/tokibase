//go:build no_nativeauth

// Package nativeauth is compiled out of this binary by the no_nativeauth build tag.
package nativeauth

import "github.com/tokibase/tokibase/core"

// Module is the placeholder returned by Register in builds with the no_nativeauth tag.
type Module struct{}

// Enabled reports false in builds with the no_nativeauth tag.
func Enabled() bool { return false }

// Register is a no-op in builds with the no_nativeauth tag.
func Register(app core.App) *Module { return &Module{} }

// SetAuditSink is a no-op in builds with the no_nativeauth tag.
func SetAuditSink(fn func(action, collection, record string, details map[string]any)) {}

// SetFailureSink is a no-op in builds with the no_nativeauth tag.
func SetFailureSink(fn func(collection string, rec *core.Record)) {}

// SetLockedSink is a no-op in builds with the no_nativeauth tag.
func SetLockedSink(fn func(collection string, rec *core.Record) bool) {}
