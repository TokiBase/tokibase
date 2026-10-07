//go:build no_timelint

package cmd

import (
	"errors"

	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/core"
)

// NewTimeCommand returns a hidden command that reports the module is compiled out.
func NewTimeCommand(app core.App) *cobra.Command {
	return &cobra.Command{
		Use: "time", Hidden: true, SilenceUsage: true,
		RunE: func(*cobra.Command, []string) error {
			return errors.New("time is not available in this build (no_timelint)")
		},
	}
}
