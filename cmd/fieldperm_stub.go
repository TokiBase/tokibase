//go:build no_fieldperm

package cmd

import (
	"errors"

	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/core"
)

// NewFieldPermCommand returns a hidden command that reports the module is compiled out.
func NewFieldPermCommand(app core.App) *cobra.Command {
	return &cobra.Command{
		Use: "fieldperm", Hidden: true, SilenceUsage: true,
		RunE: func(*cobra.Command, []string) error {
			return errors.New("fieldperm is not available in this build (no_fieldperm)")
		},
	}
}
