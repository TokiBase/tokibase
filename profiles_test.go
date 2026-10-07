package tokibase_test

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

type profile struct {
	name   string
	budget int // MiB
	tags   []string
}

// readProfiles parses profiles.txt (name, budget, tags...).
func readProfiles(t *testing.T) []profile {
	t.Helper()
	f, err := os.Open("profiles.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var out []profile
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			t.Fatalf("profiles.txt: bad line %q", line)
		}
		budget, err := strconv.Atoi(fields[1])
		if err != nil {
			t.Fatalf("profiles.txt: bad budget in %q: %v", line, err)
		}
		out = append(out, profile{name: fields[0], budget: budget, tags: fields[2:]})
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// removableTags returns the no_<x> tag of every module that ships a stub.go.
func removableTags(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("modules/*/stub.go")
	if err != nil {
		t.Fatal(err)
	}
	var tags []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		first, _, _ := strings.Cut(string(b), "\n")
		tag, ok := strings.CutPrefix(first, "//go:build ")
		if !ok || !strings.HasPrefix(tag, "no_") {
			t.Fatalf("%s: first line must be //go:build no_<module>, got %q", f, first)
		}
		tags = append(tags, tag)
	}
	return tags
}

func goRun(t *testing.T, tags []string, args ...string) {
	t.Helper()
	full := append([]string{args[0], "-tags", strings.Join(tags, ",")}, args[1:]...)
	cmd := exec.Command("go", full...)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go %s: %v\n%s", strings.Join(full, " "), err, out)
	}
}

// TestProfilesDefined checks that profiles.txt only uses known tags.
func TestProfilesDefined(t *testing.T) {
	known := map[string]bool{"replica_s3": true, "no_ui": true, "no_mcp": true}
	for _, tag := range removableTags(t) {
		known[tag] = true
	}
	seen := map[string]bool{}
	for _, p := range readProfiles(t) {
		seen[p.name] = true
		for _, tag := range p.tags {
			if !known[tag] {
				t.Errorf("profile %s: unknown tag %q", p.name, tag)
			}
		}
	}
	for _, want := range []string{"solo", "team", "cluster", "edge", "nano"} {
		if !seen[want] {
			t.Errorf("profiles.txt: missing profile %q", want)
		}
	}
}

// TestProfileBuilds compiles (go build) and vets (go vet, tests included)
// every profile, plus one build with every removable module stubbed out, to
// catch stub files that drifted from the real exported surface.
func TestProfileBuilds(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping profile builds in -short mode")
	}
	cases := map[string][]string{"all-stubs": append(removableTags(t), "no_ui")}
	for _, p := range readProfiles(t) {
		cases[p.name] = p.tags
	}
	for name, tags := range cases {
		t.Run(name, func(t *testing.T) {
			goRun(t, tags, "build", "./...")
			goRun(t, tags, "vet", "-structtag=false", "./...")
		})
	}
}

// TestEachStubBuilds builds with every single no_<module> tag on its own.
func TestEachStubBuilds(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping per-module stub builds in -short mode")
	}
	for _, tag := range removableTags(t) {
		t.Run(tag, func(t *testing.T) {
			goRun(t, []string{tag}, "build", "./...")
		})
	}
}
