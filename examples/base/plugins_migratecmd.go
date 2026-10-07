//go:build !no_migratecmd

package main

import (
	"github.com/tokibase/tokibase"
	"github.com/tokibase/tokibase/plugins/migratecmd"
)

// registerMigrateCmd adds the migrate command (with js templates).
func registerMigrateCmd(app *tokibase.PocketBase, migrationsDir string, automigrate bool) {
	migratecmd.MustRegister(app, app.RootCmd, migratecmd.Config{
		TemplateLang: migratecmd.TemplateLangJS,
		Automigrate:  automigrate,
		Dir:          migrationsDir,
	})
}
