package ruleguard_test

import (
	"os"
	"strings"
	"testing"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/modules/ruleguard"
	"github.com/tokibase/tokibase/modules/store/sqlite"
	"github.com/tokibase/tokibase/tests"
	"github.com/tokibase/tokibase/tools/types"
)

func addCollection(t testing.TB, app core.App, name string, list, view *string) {
	t.Helper()
	c := core.NewBaseCollection(name)
	c.ListRule = list
	c.ViewRule = view
	if err := app.Save(c); err != nil {
		t.Fatal(err)
	}
}

func find(fs []ruleguard.Finding, col, rule string) *ruleguard.Finding {
	for i := range fs {
		if fs[i].Collection == col && fs[i].Rule == rule {
			return &fs[i]
		}
	}
	return nil
}

func TestLint(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	addCollection(t, app, "rg_public", types.Pointer(""), types.Pointer(""))
	addCollection(t, app, "rg_allowed", types.Pointer(""), nil)
	addCollection(t, app, "rg_null", nil, nil)
	addCollection(t, app, "rg_expr", types.Pointer(`@request.auth.id != ""`), types.Pointer(`@request.auth.id != ""`))

	pol := ruleguard.Default()
	pol.Allow("rg_allowed", ruleguard.KindList)

	fs, err := ruleguard.Lint(app, pol)
	if err != nil {
		t.Fatal(err)
	}

	if f := find(fs, "rg_public", "list"); f == nil || f.Severity != "error" {
		t.Fatalf("rg_public.list: %+v", f)
	}
	if f := find(fs, "rg_public", "view"); f == nil || f.Severity != "error" {
		t.Fatalf("view rule should be an error: %+v", f)
	}
	if f := find(fs, "rg_allowed", "list"); f == nil || f.Severity != "info" {
		t.Fatalf("rg_allowed.list: %+v", f)
	}
	for _, name := range []string{"rg_null", "rg_expr"} {
		for _, f := range fs {
			if f.Collection == name {
				t.Fatalf("unexpected finding %+v", f)
			}
		}
	}
	for _, f := range fs {
		if strings.HasPrefix(f.Collection, "_") {
			t.Fatalf("system collection reported: %+v", f)
		}
	}
}

func TestPolicyLoadSave(t *testing.T) {
	dir := t.TempDir()
	p, err := ruleguard.Load(dir)
	if err != nil || p.Policy != "warn" || len(p.Public) != 0 {
		t.Fatalf("default: %+v %v", p, err)
	}
	p.Policy = "strict"
	p.Allow("posts", "view", "list", "view")
	if err := ruleguard.Save(dir, p); err != nil {
		t.Fatal(err)
	}
	got, err := ruleguard.Load(dir)
	if err != nil || got.Policy != "strict" || strings.Join(got.Public["posts"], ",") != "list,view" {
		t.Fatalf("roundtrip: %+v %v", got, err)
	}
	os.WriteFile(ruleguard.Path(dir), []byte(`{"policy":"nope"}`), 0o644)
	if _, err := ruleguard.Load(dir); err == nil {
		t.Fatal("expected invalid policy error")
	}
}

func newApp(dir string) core.App {
	app := core.NewBaseApp(core.BaseAppConfig{DataDir: dir, DBOpener: sqlite.NewOpener()})
	ruleguard.Register(app)
	return app
}

func TestStrictBootstrapFails(t *testing.T) {
	dir := t.TempDir()

	app := newApp(dir)
	if err := app.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	addCollection(t, app, "rg_boot", types.Pointer(""), nil)
	app.ResetBootstrapState()

	// warn (default): starts
	app = newApp(dir)
	if err := app.Bootstrap(); err != nil {
		t.Fatalf("warn must not fail: %v", err)
	}
	app.ResetBootstrapState()

	// strict: refuses
	if err := ruleguard.Save(dir, ruleguard.Policy{Policy: "strict"}); err != nil {
		t.Fatal(err)
	}
	app = newApp(dir)
	err := app.Bootstrap()
	if err == nil || !strings.Contains(err.Error(), "rg_boot.list") {
		t.Fatalf("expected strict failure listing rg_boot.list, got %v", err)
	}
	app.ResetBootstrapState()

	// strict + allowlisted: starts
	pol := ruleguard.Policy{Policy: "strict"}
	pol.Allow("rg_boot", "list")
	pol.Allow("users", "create", "auth") // PocketBase default users collection
	ruleguard.Save(dir, pol)
	app = newApp(dir)
	if err := app.Bootstrap(); err != nil {
		t.Fatalf("allowlisted strict must start: %v", err)
	}
	app.ResetBootstrapState()

	// off: starts even if not allowlisted
	ruleguard.Save(dir, ruleguard.Policy{Policy: "off"})
	app = newApp(dir)
	if err := app.Bootstrap(); err != nil {
		t.Fatalf("off must not fail: %v", err)
	}
	app.ResetBootstrapState()
}
