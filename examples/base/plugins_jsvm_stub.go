//go:build no_jsvm

package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/tokibase/tokibase"
	"github.com/tokibase/tokibase/kernel"
)

func init() {
	// pb_hooks / pb_migrations next to the data dir mean the operator expects
	// JS hooks or JS migrations; this build would silently ignore them.
	kernel.RegisterModule(kernel.ModuleMarker{
		Name:    "jsvm",
		Tag:     "no_jsvm",
		Files:   []string{"../pb_hooks", "../pb_migrations"},
		Stubbed: true,
	})
}

// registerJSVM refuses to start in builds with the no_jsvm tag when
// --hooksDir/--migrationsDir are set (they would be silently ignored), unless
// TOKI_ALLOW_STUBBED_MODULES=1. The pb_hooks/pb_migrations dirs next to the
// data dir are checked by the stubbed module guard (marker above).
func registerJSVM(app *tokibase.PocketBase, migrationsDir, hooksDir string, hooksWatch bool, hooksPool int) error {
	var set []string
	if hooksDir != "" {
		set = append(set, "--hooksDir="+hooksDir)
	}
	if migrationsDir != "" {
		set = append(set, "--migrationsDir="+migrationsDir)
	}
	if len(set) == 0 {
		return nil
	}

	msg := "module \"jsvm\" is compiled out (built with no_jsvm): " + strings.Join(set, ", ") + " would be ignored"
	if strings.TrimSpace(os.Getenv(kernel.EnvAllowStubbedModules)) == "1" {
		fmt.Fprintln(os.Stderr, "ERROR: "+msg+" (guards are NOT active)")
		return nil
	}

	return fmt.Errorf("refusing to start: %s; rebuild without no_jsvm or set %s=1 to start anyway", msg, kernel.EnvAllowStubbedModules)
}
