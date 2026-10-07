// Test guest: route that echoes the actor id and the request body.
package main

import toki "github.com/tokibase/tokibase/modules/wasm/sdk/go"

func main() {
	toki.Run(func(ev *toki.Event) (*toki.Result, error) {
		return toki.JSON(201, map[string]any{
			"actor":   ev.Actor.ID,
			"kind":    ev.Actor.Kind,
			"body":    ev.Route.Body,
			"method":  ev.Route.Method,
			"query_x": ev.Route.Query["x"],
		}), nil
	})
}
