//go:build no_migratecmd

package main

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase"
)

// registerMigrateCmd registers a placeholder `migrate` command in builds with
// the no_migratecmd tag that fails loudly instead of doing nothing
// (automigrate is not available either).
func registerMigrateCmd(app *tokibase.PocketBase, migrationsDir string, automigrate bool) {
	app.RootCmd.AddCommand(&cobra.Command{
		Use:                "migrate",
		Short:              "Not available: compiled out (no_migratecmd)",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("the migrate command is compiled out of this build (no_migratecmd); use a build without the no_migratecmd tag")
		},
	})
}
