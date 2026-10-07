// Test guest: misbehaviors and host calls selected by ?cmd= on a route.
package main

import (
	"os"
	"strings"

	toki "github.com/tokibase/tokibase/modules/wasm/sdk/go"
)

func main() {
	toki.Run(func(ev *toki.Event) (*toki.Result, error) {
		if ev.Route == nil { // cron / job / record events: just succeed
			return toki.Ok(), nil
		}
		q := ev.Route.Query
		switch q["cmd"] {
		case "loop":
			for {
			}
		case "alloc":
			b := make([]byte, 512<<20)
			for i := 0; i < len(b); i += 4096 {
				b[i] = 1
			}
			return toki.JSON(200, len(b)), nil
		case "panic":
			var m map[string]int
			m["x"] = 1
		case "exit":
			os.Exit(3)
		case "stdout":
			os.Stdout.WriteString(strings.Repeat("x", 2<<20))
		case "http":
			r, err := toki.HTTPFetch(toki.HTTPRequest{URL: q["u"]})
			if err != nil {
				return toki.JSON(200, map[string]any{"error": err.Error()}), nil
			}
			return toki.JSON(200, map[string]any{"status": r.Status, "body": r.Body}), nil
		case "kv":
			if err := toki.KVSet("counter", q["v"], 0); err != nil {
				return toki.JSON(200, map[string]any{"error": err.Error()}), nil
			}
			v, found, err := toki.KVGet("counter")
			if err != nil {
				return toki.JSON(200, map[string]any{"error": err.Error()}), nil
			}
			return toki.JSON(200, map[string]any{"value": v, "found": found}), nil
		case "records":
			rec, err := toki.RecordsSave("posts", "", map[string]any{"title": q["t"]})
			if err != nil {
				return toki.JSON(200, map[string]any{"error": err.Error()}), nil
			}
			list, err := toki.RecordsFind(toki.FindRequest{Collection: "posts", Filter: "title = {:t}", Params: map[string]any{"t": q["t"]}})
			if err != nil {
				return toki.JSON(200, map[string]any{"error": err.Error()}), nil
			}
			return toki.JSON(200, map[string]any{"saved": rec["title"], "found": len(list)}), nil
		}
		return toki.Ok(), nil
	})
}
