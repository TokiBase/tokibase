package tokibase_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func firstLine(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("%s: %v (every stubbed module needs marker.go and stub_marker.go)", p, err)
	}
	return strings.TrimSpace(strings.SplitN(string(b), "\n", 2)[0])
}

// Every module with a stub registers a marker in both builds, under the same
// build tag as its stub (the boot guard depends on it).
func TestEveryStubHasMarkerFiles(t *testing.T) {
	stubs, _ := filepath.Glob("modules/*/stub.go")
	if len(stubs) == 0 {
		t.Fatal("no stubs found")
	}
	for _, stub := range stubs {
		dir := filepath.Dir(stub)
		tag := strings.TrimPrefix(firstLine(t, stub), "//go:build ")
		if got := firstLine(t, filepath.Join(dir, "stub_marker.go")); got != "//go:build "+tag {
			t.Errorf("%s/stub_marker.go: %q, want tag %q", dir, got, tag)
		}
		if got := firstLine(t, filepath.Join(dir, "marker.go")); got != "//go:build !"+tag {
			t.Errorf("%s/marker.go: %q, want tag !%s", dir, got, tag)
		}
		b, _ := os.ReadFile(filepath.Join(dir, "stub_marker.go"))
		if !strings.Contains(string(b), "true") {
			t.Errorf("%s/stub_marker.go must register stubbed=true", dir)
		}
	}
}
