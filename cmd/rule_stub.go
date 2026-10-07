//go:build no_ruleguard

package cmd

import (
	"errors"

	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/core"
)

// NewRuleCommand returns a hidden command that reports the module is compiled out.
func NewRuleCommand(app core.App) *cobra.Command {
	return &cobra.Command{
		Use: "rule", Hidden: true, SilenceUsage: true,
		RunE: func(*cobra.Command, []string) error {
			return errors.New("rule is not available in this build (no_ruleguard)")
		},
	}
}
