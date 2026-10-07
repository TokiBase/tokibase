//go:build !no_embed_jsvm

package embed

import (
	"github.com/tokibase/tokibase"
	"github.com/tokibase/tokibase/plugins/jsvm"
)

func registerHooks(app *tokibase.PocketBase, hooksDir string) error {
	if hooksDir == "" {
		return nil
	}
	return jsvm.Register(app, jsvm.Config{HooksDir: hooksDir, HooksWatch: false, HooksPoolSize: 5})
}
