//go:build no_sessions

package cmd

import (
	"errors"

	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/core"
)

// NewSessionsCommand returns a hidden command that reports the module is compiled out.
func NewSessionsCommand(app core.App) *cobra.Command {
	return &cobra.Command{
		Use: "sessions", Hidden: true, SilenceUsage: true,
		RunE: func(*cobra.Command, []string) error {
			return errors.New("sessions is not available in this build (no_sessions)")
		},
	}
}
