//go:build no_lockout

package cmd

import (
	"errors"

	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/core"
)

// NewLockoutCommand returns a hidden command that reports the module is compiled out.
func NewLockoutCommand(app core.App) *cobra.Command {
	return &cobra.Command{
		Use: "lockout", Hidden: true, SilenceUsage: true,
		RunE: func(*cobra.Command, []string) error {
			return errors.New("lockout is not available in this build (no_lockout)")
		},
	}
}
