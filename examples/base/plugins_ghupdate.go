//go:build !no_ghupdate

package main

import (
	"github.com/tokibase/tokibase"
	"github.com/tokibase/tokibase/plugins/ghupdate"
)

// registerGHUpdate adds the GitHub selfupdate command.
func registerGHUpdate(app *tokibase.PocketBase) {
	ghupdate.MustRegister(app, app.RootCmd, ghupdate.Config{})
}
