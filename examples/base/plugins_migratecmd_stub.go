//go:build no_migratecmd

package main

import "github.com/tokibase/tokibase"

// registerMigrateCmd is a no-op in builds with the no_migratecmd tag (no migrate command, no automigrate).
func registerMigrateCmd(app *tokibase.PocketBase, migrationsDir string, automigrate bool) {}
