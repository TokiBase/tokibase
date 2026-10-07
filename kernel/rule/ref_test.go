package rule_test

import (
	"errors"
	"testing"

	"github.com/tokibase/tokibase/kernel/rule"
)

type stubDialect struct{ rule.Dialect }

func (stubDialect) TextOf(expr string) string { return expr }

func (stubDialect) JSONExtract(column string, path []rule.Segment) (string, error) {
	return "J(" + column + "|" + rule.JSONPathString(path) + ")", nil
}

func (stubDialect) JSONArrayLength(column string) (string, error) {
	return "", rule.ErrUnsupported
}

func TestRefEmit(t *testing.T) {
	d := stubDialect{}

	scenarios := []struct {
		ref      rule.Ref
		expected string
		err      bool
	}{
		{rule.Ref{Kind: rule.RefColumn, Alias: "a", Column: "b"}, "[[a.b]]", false},
		{rule.Ref{Kind: rule.RefColumn, Alias: "a", Column: "b", Lower: true}, "LOWER([[a.b]])", false},
		{rule.Ref{Kind: rule.RefJSON, Alias: "a", Column: "j"}, "J(a.j|)", false},
		{rule.Ref{Kind: rule.RefJSON, Alias: "a", Column: "j", Path: []rule.Segment{{Key: "x"}, {Key: "0", Index: true}, {Key: "y"}}, Lower: true}, "LOWER(J(a.j|x[0].y))", false},
		{rule.Ref{Kind: rule.RefArrayLength, Alias: "a", Column: "b"}, "", true},
		{rule.Ref{Kind: rule.RefKind(99)}, "", true},
	}

	for i, s := range scenarios {
		got, err := s.ref.Emit(d)
		if (err != nil) != s.err {
			t.Fatalf("[%d] unexpected error state: %v", i, err)
		}
		if err != nil && !errors.Is(err, rule.ErrUnsupported) {
			t.Fatalf("[%d] expected ErrUnsupported, got %v", i, err)
		}
		if got != s.expected {
			t.Fatalf("[%d] expected %q, got %q", i, s.expected, got)
		}
	}
}

func seg(parts ...string) []rule.Segment {
	out := make([]rule.Segment, len(parts))
	for i, p := range parts {
		out[i] = rule.SegmentFromRaw(p, p)
	}
	return out
}

func TestJSONPathString(t *testing.T) {
	scenarios := map[string][]rule.Segment{
		"":          nil,
		"a":         seg("a"),
		"a.b.c":     seg("a", "b", "c"),
		"[0]":       seg("0"),
		"a[0].b":    seg("a", "0", "b"),
		"[1].a[22]": seg("1", "a", "22"),
		"a[0][1]":   seg("a", "0", "1"),
	}

	for want, path := range scenarios {
		if got := rule.JSONPathString(path); got != want {
			t.Fatalf("%v: expected %q, got %q", path, want, got)
		}
	}
}

// Regression (PocketBase v0.40.4 compatibility): the index check runs on the
// RAW segment, the sanitized value is only the key. "1\u00e9" sanitizes to
// "1" but was never an index, so the SQLite path is "$.a.1" and not "$.a[1]".
func TestSegmentFromRawUpstreamOrder(t *testing.T) {
	scenarios := []struct {
		raw, sanitized, expected string
	}{
		{"1", "1", "a[1]"},
		{"1\u00e9", "1", "a.1"},
		{"1 ", "1", "a.1"},
		{"+1", "1", "a[1]"}, // Atoi accepts the sign, like upstream
		{"x", "x", "a.x"},
	}

	for _, s := range scenarios {
		path := []rule.Segment{{Key: "a"}, rule.SegmentFromRaw(s.raw, s.sanitized)}
		if got := rule.JSONPathString(path); got != s.expected {
			t.Fatalf("%q: expected %q, got %q", s.raw, s.expected, got)
		}
	}
}
