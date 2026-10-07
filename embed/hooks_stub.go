//go:build no_jsvm || no_embed_jsvm

package embed

import (
	"errors"

	"github.com/tokibase/tokibase"
)

func registerHooks(app *tokibase.PocketBase, hooksDir string) error {
	if hooksDir != "" {
		return errors.New("embed: built with no_jsvm or no_embed_jsvm, HooksDir is not supported")
	}
	return nil
}
