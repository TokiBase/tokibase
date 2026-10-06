package kernel_test

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
)

const kernelPkg = "github.com/tokibase/tokibase/kernel"

// goList runs `go list` with the toolchain that runs the tests.
func goList(t *testing.T, args ...string) []string {
	t.Helper()

	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("go binary not found in PATH: %v", err)
	}

	out, err := exec.Command(goBin, append([]string{"list"}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("go list %v failed: %v\n%s", args, err, out)
	}

	return strings.Fields(string(out))
}

// The kernel must stay free from HTTP concerns: no net/http and no
// tools/router in its (transitive) dependencies.
//
// This is the CI counterpart of the depguard rule in golangci.yml.
func TestKernelHasNoHTTPDependencies(t *testing.T) {
	deps := goList(t, "-deps", kernelPkg)

	forbidden := []string{
		"net/http",
		"github.com/tokibase/tokibase/tools/router",
		"github.com/tokibase/tokibase/core",
		"github.com/tokibase/tokibase/apis",
	}

	for _, f := range forbidden {
		if slices.Contains(deps, f) {
			t.Errorf("the kernel package must not depend on %q (go list -deps %s)", f, kernelPkg)
		}
	}
}

// The kernel packages must not import os/exec directly.
func TestKernelDoesNotImportOSExec(t *testing.T) {
	imports := goList(t, "-f", `{{join .Imports "\n"}}`, kernelPkg+"/...")

	if slices.Contains(imports, "os/exec") {
		t.Errorf("the kernel packages must not import os/exec directly")
	}
}

// The SQLite driver lives in modules/store/sqlite: the kernel keeps only
// github.com/pocketbase/dbx and must not (transitively) depend on modernc.org/sqlite.
func TestKernelHasNoSQLiteDriverDependency(t *testing.T) {
	deps := goList(t, "-deps", kernelPkg)

	if slices.Contains(deps, "modernc.org/sqlite") {
		t.Errorf("the kernel package must not depend on modernc.org/sqlite (go list -deps %s)", kernelPkg)
	}

	if !slices.Contains(deps, "github.com/pocketbase/dbx") {
		t.Errorf("expected the kernel to keep using github.com/pocketbase/dbx")
	}
}
