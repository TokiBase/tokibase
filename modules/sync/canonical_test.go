//go:build !no_sync

package sync

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestCanonicalJSON(t *testing.T) {
	v := map[string]any{
		"z": 1.0, "a": []any{true, nil, "x<>&", 1e6, 0.5, 100.0},
		"m": map[string]any{"y": "é", "b": false},
	}
	got := string(canonicalJSON(v))
	want := `{"a":[true,null,"x<>&",1e+06,0.5,100],"m":{"b":false,"y":"é"},"z":1}`
	if got != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
}

func TestCanonicalHashStable(t *testing.T) {
	f1 := map[string]any{"title": "T", "qty": 5.0, "tags": []any{"a", "b"}, "meta": map[string]any{"b": 1.0, "a": 2.0}}
	f2 := map[string]any{"meta": map[string]any{"a": 2.0, "b": 1.0}, "tags": []any{"a", "b"}, "qty": 5, "title": "T"} // other order, int
	h1 := canonicalHash("col1", "rec1", f1)
	for i := 0; i < 50; i++ {
		if hex.EncodeToString(h1) != hex.EncodeToString(canonicalHash("col1", "rec1", f2)) {
			t.Fatal("hash depends on map order or number type")
		}
	}
	// the input is exactly {"c":..,"id":..,"f":{sorted}}
	sum := sha256.Sum256([]byte(`{"c":"col1","id":"rec1","f":{"meta":{"a":2,"b":1},"qty":5,"tags":["a","b"],"title":"T"}}`))
	if hex.EncodeToString(sum[:]) != hex.EncodeToString(h1) {
		t.Fatal("canonical form changed")
	}
	if hex.EncodeToString(canonicalHash("col1", "rec2", f1)) == hex.EncodeToString(h1) {
		t.Fatal("id must be part of the hash")
	}
	if hex.EncodeToString(canonicalHash("col2", "rec1", f1)) == hex.EncodeToString(h1) {
		t.Fatal("collection must be part of the hash")
	}
	f1["title"] = "U"
	if hex.EncodeToString(canonicalHash("col1", "rec1", f1)) == hex.EncodeToString(h1) {
		t.Fatal("value must be part of the hash")
	}
}

func TestTypedDelta(t *testing.T) {
	jsonEq(t, typedDelta(TypeCounter, 5.0, 8.0), `{"$inc":3}`)
	jsonEq(t, typedDelta(TypeCounter, 8.0, 5.0), `{"$inc":-3}`)
	jsonEq(t, typedDelta(TypeCounter, nil, 4.0), `{"$inc":4}`)
	jsonEq(t, typedDelta(TypeSet, []any{"a", "b"}, []any{"b", "c"}), `{"$add":["c"],"$rm":["a"]}`)
	jsonEq(t, typedDelta(TypeSet, []any{}, []any{"x"}), `{"$add":["x"]}`)
	jsonEq(t, typedDelta(TypeSet, []any{"x"}, []any{}), `{"$rm":["x"]}`)
	jsonEq(t, typedDelta("", "a", "b"), `"b"`)
	jsonEq(t, typedDelta(TypeCounter, "x", "y"), `"y"`)
}
