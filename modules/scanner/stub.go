//go:build no_scanner

// Package scanner is compiled out of this binary by the no_scanner build tag.
// This file keeps the surface used by the root package so the wiring compiles.
package scanner

import (
	"errors"

	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/core"
)

// Register is a no-op in builds with the no_scanner tag.
func Register(app core.App) {}

// NewCommand returns a hidden command that reports the module is compiled out.
func NewCommand(app core.App) *cobra.Command {
	return &cobra.Command{
		Use: "scan", Hidden: true, SilenceUsage: true,
		RunE: func(*cobra.Command, []string) error {
			return errors.New("scanner is not available in this build (no_scanner)")
		},
	}
}

func Enabled() bool { return false }
