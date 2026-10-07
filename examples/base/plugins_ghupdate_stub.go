//go:build no_ghupdate

package main

import "github.com/tokibase/tokibase"

// registerGHUpdate is a no-op in builds with the no_ghupdate tag (no update command).
func registerGHUpdate(app *tokibase.PocketBase) {}
