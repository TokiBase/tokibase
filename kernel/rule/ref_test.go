package rule_test

import (
	"errors"
	"testing"

	"github.com/tokibase/tokibase/kernel/rule"
)

type stubDialect struct{ rule.Dialect }

func (stubDialect) JSONExtract(column string, path []string) (string, error) {
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
		{rule.Ref{Kind: rule.RefJSON, Alias: "a", Column: "j", Path: []string{"x", "0", "y"}, Lower: true}, "LOWER(J(a.j|x[0].y))", false},
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

func TestJSONPathString(t *testing.T) {
	scenarios := map[string][]string{
		"":          nil,
		"a":         {"a"},
		"a.b.c":     {"a", "b", "c"},
		"[0]":       {"0"},
		"a[0].b":    {"a", "0", "b"},
		"[1].a[22]": {"1", "a", "22"},
		"a[0][1]":   {"a", "0", "1"},
	}

	for want, path := range scenarios {
		if got := rule.JSONPathString(path); got != want {
			t.Fatalf("%v: expected %q, got %q", path, want, got)
		}
	}
}
