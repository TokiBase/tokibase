//go:build !no_jsvm

package main

import (
	"github.com/tokibase/tokibase"
	"github.com/tokibase/tokibase/plugins/jsvm"
)

// registerJSVM loads jsvm (pb_hooks and pb_migrations).
func registerJSVM(app *tokibase.PocketBase, migrationsDir, hooksDir string, hooksWatch bool, hooksPool int) error {
	jsvm.MustRegister(app, jsvm.Config{
		MigrationsDir: migrationsDir,
		HooksDir:      hooksDir,
		HooksWatch:    hooksWatch,
		HooksPoolSize: hooksPool,
	})
	return nil
}
