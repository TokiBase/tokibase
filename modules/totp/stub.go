//go:build no_totp

// Package totp is compiled out of this binary by the no_totp build tag.
// This file keeps the surface used by the root package so the wiring compiles.
package totp

import (
	"errors"

	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/core"
)

// Module is the placeholder returned by Register in builds with the no_totp tag.
type Module struct{}

// Register is a no-op in builds with the no_totp tag.
func Register(app core.App) *Module { return &Module{} }

// SetAuditSink is a no-op in builds with the no_totp tag.
func SetAuditSink(fn func(action, collection, record string, details map[string]any)) {}

// SetFailureSink is a no-op in builds with the no_totp tag.
func SetFailureSink(fn func(collection string, rec *core.Record)) {}

// SetLockedSink is a no-op in builds with the no_totp tag.
func SetLockedSink(fn func(collection string, rec *core.Record) bool) {}

// NewCommand returns a hidden command that reports the module is compiled out.
func NewCommand(app core.App) *cobra.Command {
	return &cobra.Command{
		Use: "totp", Hidden: true, SilenceUsage: true,
		RunE: func(*cobra.Command, []string) error {
			return errors.New("totp is not available in this build (no_totp)")
		},
	}
}
