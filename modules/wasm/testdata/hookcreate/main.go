// Test guest: before-create hook that rejects an empty title and otherwise
// uppercases it. Built with GOOS=wasip1 GOARCH=wasm by the tests.
package main

import (
	"strings"

	toki "github.com/tokibase/tokibase/modules/wasm/sdk/go"
)

// suffix is overridden in the hot reload test: -ldflags "-X main.suffix=-v2".
var suffix = ""

func main() {
	toki.Run(func(ev *toki.Event) (*toki.Result, error) {
		title, _ := ev.Record["title"].(string)
		if strings.TrimSpace(title) == "" {
			return toki.Reject(400, "title is required", map[string]any{
				"title": map[string]any{"code": "validation_required", "message": "Cannot be blank."},
			}), nil
		}
		return toki.Ok().Set("title", strings.ToUpper(title)+suffix), nil
	})
}
