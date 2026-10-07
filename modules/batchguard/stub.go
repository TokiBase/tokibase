//go:build no_batchguard

// Package batchguard is compiled out of this binary by the no_batchguard build tag.
package batchguard

import (
	"errors"

	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/core"
)

// Register is a no-op in builds with the no_batchguard tag.
func Register(app core.App) {}

// NewCommand returns a hidden command that reports the module is compiled out.
func NewCommand(app core.App) *cobra.Command {
	return &cobra.Command{
		Use: "batch", Hidden: true, SilenceUsage: true,
		RunE: func(*cobra.Command, []string) error {
			return errors.New("batchguard is not available in this build (no_batchguard)")
		},
	}
}
