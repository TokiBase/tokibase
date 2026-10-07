//go:build no_ghupdate

package main

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase"
)

// registerGHUpdate registers a placeholder `update` command in builds with
// the no_ghupdate tag that fails loudly instead of being an unknown command.
func registerGHUpdate(app *tokibase.PocketBase) {
	app.RootCmd.AddCommand(&cobra.Command{
		Use:                "update",
		Short:              "Not available: compiled out (no_ghupdate)",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("the update command is compiled out of this build (no_ghupdate)")
		},
	})
}
