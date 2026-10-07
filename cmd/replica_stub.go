//go:build no_replica

package cmd

import (
	"errors"

	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/core"
)

// NewReplicaCommand returns a hidden command that reports the module is compiled out.
func NewReplicaCommand(app core.App) *cobra.Command {
	return &cobra.Command{
		Use: "replica", Hidden: true, SilenceUsage: true,
		RunE: func(*cobra.Command, []string) error {
			return errors.New("replica is not available in this build (no_replica)")
		},
	}
}
