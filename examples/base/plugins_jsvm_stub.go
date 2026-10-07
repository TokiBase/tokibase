//go:build no_jsvm

package main

import "github.com/tokibase/tokibase"

// registerJSVM is a no-op in builds with the no_jsvm tag (the --hooks* flags stay accepted but have no effect).
func registerJSVM(app *tokibase.PocketBase, migrationsDir, hooksDir string, hooksWatch bool, hooksPool int) {
}
