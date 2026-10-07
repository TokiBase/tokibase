// Test guest for record hooks, behavior selected by the MODE env of the sidecar:
//
//	echo     writes the received request_info (JSON) into the "title" field
//	save     saves a record in "audit" (host call inside the hook)
//	pingpong saves a record in the opposite collection of ping/pong
//	sys      tries to read and write system collections, reports the errors in "title"
package main

import (
	"encoding/json"
	"os"

	toki "github.com/tokibase/tokibase/modules/wasm/sdk/go"
)

func main() {
	toki.Run(func(ev *toki.Event) (*toki.Result, error) {
		if ev.Kind != "record" {
			return toki.Ok(), nil
		}
		title, _ := ev.Record["title"].(string)
		switch os.Getenv("MODE") {
		case "echo":
			b, _ := json.Marshal(ev.RequestInfo)
			return toki.Ok().Set("title", string(b)), nil
		case "save":
			if _, err := toki.RecordsSave("audit", "", map[string]any{"note": title}); err != nil {
				return toki.Reject(500, "save failed: "+err.Error(), nil), nil
			}
		case "pingpong":
			other := "pong"
			if ev.Collection == "pong" {
				other = "ping"
			}
			if _, err := toki.RecordsSave(other, "", map[string]any{"note": "x"}); err != nil {
				toki.Log(2, "pingpong: "+err.Error())
			}
		case "sys":
			out := map[string]string{}
			if _, err := toki.RecordsFind(toki.FindRequest{Collection: "_superusers"}); err != nil {
				out["find"] = err.Error()
			}
			if _, err := toki.RecordsSave("_superusers", "", map[string]any{"email": "evil@example.com", "password": "12345678"}); err != nil {
				out["save"] = err.Error()
			}
			if _, err := toki.RecordsSave("audit", "", map[string]any{"id": "x", "note": "n"}); err != nil {
				out["reserved"] = err.Error()
			}
			b, _ := json.Marshal(out)
			return toki.Ok().Set("title", string(b)), nil
		}
		return toki.Ok(), nil
	})
}
