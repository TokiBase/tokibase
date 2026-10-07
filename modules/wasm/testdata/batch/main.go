// Test guest for batch events, behavior selected by the MODE env of the sidecar:
//
//	maxqty  rejects the batch when the total qty of order_items exceeds MAX (default 5)
//	allow   accepts everything
//	spin    loops forever (timeout, the batch must fail closed)
//	echo    rejects with the received event as the message (payload inspection)
//	trap    exits non-zero
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	toki "github.com/tokibase/tokibase/modules/wasm/sdk/go"
)

func main() {
	toki.Run(func(ev *toki.Event) (*toki.Result, error) {
		if ev.Kind != "batch" || ev.Batch == nil {
			return toki.Reject(500, "not a batch event", nil), nil
		}
		switch os.Getenv("MODE") {
		case "maxqty":
			max := 5.0
			if v, err := strconv.ParseFloat(os.Getenv("MAX"), 64); err == nil {
				max = v
			}
			if total := ev.Batch.Sum("order_items", "qty"); total > max {
				return toki.Reject(422, fmt.Sprintf("too many items: %v > %v (%s)", total, max, ev.Phase), nil), nil
			}
		case "spin":
			for {
			}
		case "echo":
			b, _ := json.Marshal(map[string]any{"phase": ev.Phase, "actor": ev.Actor, "batch": ev.Batch})
			return toki.Reject(418, string(b), nil), nil
		case "trap":
			os.Exit(3)
		}
		return toki.Ok(), nil
	})
}
