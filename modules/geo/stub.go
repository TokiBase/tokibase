//go:build no_geo

// Package geo is compiled out of this binary by the no_geo build tag.
// This file keeps the surface used by the root package so the wiring compiles.
package geo

import (
	"errors"

	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/core"
)

// Register is a no-op in builds with the no_geo tag.
func Register(app core.App) {}

// NewCommand returns a hidden command that reports the module is compiled out.
func NewCommand(app core.App) *cobra.Command {
	return &cobra.Command{
		Use: "geo", Hidden: true, SilenceUsage: true,
		RunE: func(*cobra.Command, []string) error {
			return errors.New("geo is not available in this build (no_geo)")
		},
	}
}
