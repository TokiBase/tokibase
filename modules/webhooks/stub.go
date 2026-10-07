//go:build no_webhooks

// Package webhooks is compiled out of this binary by the no_webhooks build tag.
// This file keeps the surface used by the root package so the wiring compiles.
package webhooks

import (
	"errors"

	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/core"
)

// Register is a no-op in builds with the no_webhooks tag.
func Register(app core.App) {}

// SetAuditSink is a no-op in builds with the no_webhooks tag.
func SetAuditSink(fn func(action, collection, record string, details map[string]any)) {}

// NewCommand returns a hidden command that reports the module is compiled out.
func NewCommand(app core.App) *cobra.Command {
	return &cobra.Command{
		Use: "webhooks", Hidden: true, SilenceUsage: true,
		RunE: func(*cobra.Command, []string) error {
			return errors.New("webhooks is not available in this build (no_webhooks)")
		},
	}
}

func Enabled() bool { return false }
