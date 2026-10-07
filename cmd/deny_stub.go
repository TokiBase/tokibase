//go:build no_denylog

package cmd

import (
	"errors"

	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/core"
)

// NewDenyCommand returns a hidden command that reports the module is compiled out.
func NewDenyCommand(app core.App) *cobra.Command {
	return &cobra.Command{
		Use: "deny", Hidden: true, SilenceUsage: true,
		RunE: func(*cobra.Command, []string) error {
			return errors.New("deny is not available in this build (no_denylog)")
		},
	}
}
