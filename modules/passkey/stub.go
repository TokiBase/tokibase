//go:build no_passkey

// Package passkey is compiled out of this binary by the no_passkey build tag.
// This file keeps the surface used by the root package so the wiring compiles.
package passkey

import (
	"errors"

	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/core"
)

// Register is a no-op in builds with the no_passkey tag.
func Register(app core.App) {}

// SetAuditSink is a no-op in builds with the no_passkey tag.
func SetAuditSink(fn func(action, collection, record string, details map[string]any)) {}

// NewCommand returns a hidden command that reports the module is compiled out.
func NewCommand(app core.App) *cobra.Command {
	return &cobra.Command{
		Use: "passkey", Hidden: true, SilenceUsage: true,
		RunE: func(*cobra.Command, []string) error {
			return errors.New("passkey is not available in this build (no_passkey)")
		},
	}
}

func SetFailureSink(fn func(collection string, rec *core.Record)) {}

func SetLockedSink(fn func(collection string, rec *core.Record) bool) {}
