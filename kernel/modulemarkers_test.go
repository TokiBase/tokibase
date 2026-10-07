package kernel_test

import (
	"strings"
	"testing"

	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tests"
)

func envMap(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestStubbedModuleGuard(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	crypto := kernel.ModuleMarker{Name: "crypto", Collections: []string{"_crypto_fields", "_crypto_keys"}, Envs: []string{"TOKI_CRYPTO_MASTER_KEY"}, Stubbed: true}
	real := kernel.ModuleMarker{Name: "fieldperm", Collections: []string{"_crypto_fields"}, Stubbed: false}
	admin := kernel.ModuleMarker{Name: "adminlock", Envs: []string{"TOKI_ADMIN_UI"}, Stubbed: true, OffIsActive: true}
	none := envMap(nil)

	// fresh data dir: no findings, real markers never count
	if err := kernel.CheckStubbedModules(app, []kernel.ModuleMarker{crypto, real}, none, false); err != nil {
		t.Fatalf("clean db: %v", err)
	}

	// env only
	err = kernel.CheckStubbedModules(app, []kernel.ModuleMarker{crypto}, envMap(map[string]string{"TOKI_CRYPTO_MASTER_KEY": "x"}), false)
	if err == nil || !strings.Contains(err.Error(), "TOKI_CRYPTO_MASTER_KEY") {
		t.Fatalf("env: %v", err)
	}
	// "off" is unset by default, active for OffIsActive markers
	if err := kernel.CheckStubbedModules(app, []kernel.ModuleMarker{{Name: "x", Envs: []string{"E"}, Stubbed: true}}, envMap(map[string]string{"E": "off"}), false); err != nil {
		t.Fatalf("off: %v", err)
	}
	if err := kernel.CheckStubbedModules(app, []kernel.ModuleMarker{admin}, envMap(map[string]string{"TOKI_ADMIN_UI": "off"}), false); err == nil {
		t.Fatal("adminlock off must count")
	}

	// existing collection table
	if _, err := app.DB().NewQuery("CREATE TABLE `_crypto_fields` (id TEXT PRIMARY KEY)").Execute(); err != nil {
		t.Fatal(err)
	}
	err = kernel.CheckStubbedModules(app, []kernel.ModuleMarker{crypto, real}, none, false)
	if err == nil || !strings.Contains(err.Error(), "collection _crypto_fields") || strings.Contains(err.Error(), "fieldperm") {
		t.Fatalf("collection: %v", err)
	}
	if !strings.Contains(err.Error(), kernel.EnvAllowStubbedModules) {
		t.Fatalf("error must mention override: %v", err)
	}
	// override: no error
	if err := kernel.CheckStubbedModules(app, []kernel.ModuleMarker{crypto}, none, true); err != nil {
		t.Fatalf("allow: %v", err)
	}
}

func TestStubbedModuleGuardBootstrapHook(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()
	if _, err := app.DB().NewQuery("CREATE TABLE `_stubguard_probe` (id TEXT PRIMARY KEY)").Execute(); err != nil {
		t.Fatal(err)
	}
	kernel.RegisterModuleMarker("stubguardprobe", []string{"_stubguard_probe"}, nil, true)
	kernel.BindStubbedModuleGuard(app)

	if err := app.Bootstrap(); err == nil || !strings.Contains(err.Error(), "_stubguard_probe") {
		t.Fatalf("bootstrap must refuse, got %v", err)
	}
	t.Setenv(kernel.EnvAllowStubbedModules, "1")
	if err := app.Bootstrap(); err != nil {
		t.Fatalf("override: %v", err)
	}
}
