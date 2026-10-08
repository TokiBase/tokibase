// Test guest for sync.conflict events, behavior selected by the MODE env of the sidecar:
//
//	accept | reject | merge | park   answer with that resolution
//	double_payment                    the §4.6 example: a second payment for a paid invoice is merged
//	                                  into a note, an unpaid one is accepted
//	none                              ok without a resolution (no opinion)
//	bogus                             an unknown resolution
//	fail                              ok:false
//	echo                              parks with the received event as the message
//	spin                              loops forever (timeout)
//	trap                              exits non-zero
package main

import (
	"encoding/json"
	"os"

	toki "github.com/tokibase/tokibase/modules/wasm/sdk/go"
)

func main() {
	toki.Run(func(ev *toki.Event) (*toki.Result, error) {
		if ev.Kind != "sync" || ev.Sync == nil {
			return toki.Reject(500, "not a sync event", nil), nil
		}
		s := ev.Sync
		switch os.Getenv("MODE") {
		case "accept":
			return toki.ConflictAccept("ok"), nil
		case "reject":
			return toki.ConflictReject("no thanks"), nil
		case "merge":
			return toki.ConflictMerge(map[string]any{"note": "merged by guest"}, "merged"), nil
		case "park":
			return toki.ConflictPark("look at this"), nil
		case "double_payment":
			if s.Current["status"] == "paid" && s.Incoming.Patch["status"] == "paid" {
				patch := map[string]any{
					"status":       "paid",
					"provider_ref": s.Current["provider_ref"],
					"note":         "double payment " + asString(s.Incoming.Patch["provider_ref"]) + ", refund",
				}
				return toki.ConflictMerge(patch, "double payment"), nil
			}
			return toki.ConflictAccept(""), nil
		case "none":
			return toki.Ok(), nil
		case "bogus":
			return &toki.Result{OK: true, Resolution: "overwrite"}, nil
		case "fail":
			return toki.Reject(500, "cannot decide", nil), nil
		case "echo":
			b, _ := json.Marshal(map[string]any{"event": ev.Event, "kind": ev.Kind, "collection": ev.Collection, "sync": s})
			return toki.ConflictPark(string(b)), nil
		case "spin":
			for {
			}
		case "trap":
			os.Exit(3)
		}
		return toki.Ok(), nil
	})
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}
