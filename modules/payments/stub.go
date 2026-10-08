//go:build no_payments

// Package payments is compiled out of this binary by the no_payments build tag.
// This file keeps the surface used by the root package so the wiring compiles.
package payments

import (
	"errors"

	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/core"
)

// Enabled is always false in builds with the no_payments tag.
func Enabled() bool { return false }

// Register is a no-op in builds with the no_payments tag.
func Register(app core.App) {}

// SetAuditSink is a no-op in builds with the no_payments tag.
func SetAuditSink(fn func(action, collection, record string, details map[string]any)) {}

// NewCommand returns a hidden command that reports the module is compiled out.
func NewCommand(app core.App) *cobra.Command {
	return &cobra.Command{
		Use: "payments", Hidden: true, SilenceUsage: true,
		RunE: func(*cobra.Command, []string) error {
			return errors.New("payments is not available in this build (no_payments)")
		},
	}
}
