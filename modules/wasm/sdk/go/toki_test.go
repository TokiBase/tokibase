package toki

import (
	"encoding/json"
	"testing"
)

func TestBatchHelpers(t *testing.T) {
	var ev Event
	in := `{"kind":"batch","batch":{"requests":[
		{"index":0,"method":"POST","collection":"items","body":{"qty":2}},
		{"index":1,"method":"POST","collection":"items","body":{"qty":"3.5"}},
		{"index":2,"method":"DELETE","collection":"items","id":"x","deleted":true},
		{"index":3,"method":"POST","collection":"other","body":{"qty":100}}],
		"auth":{"id":"u1","collection":"users","superuser":false}}}`
	if err := json.Unmarshal([]byte(in), &ev); err != nil {
		t.Fatal(err)
	}
	if got := ev.Batch.Sum("items", "qty"); got != 5.5 {
		t.Fatalf("sum %v", got)
	}
	if n := len(ev.Batch.For("items")); n != 3 {
		t.Fatalf("for %d", n)
	}
	if ev.Batch.Auth == nil || ev.Batch.Auth.ID != "u1" {
		t.Fatal("auth")
	}
	var nilBatch *Batch
	if nilBatch.Sum("x", "y") != 0 {
		t.Fatal("nil batch")
	}
}
