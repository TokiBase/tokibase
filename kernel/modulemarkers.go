package kernel

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/tokibase/tokibase/tools/hook"
)

// EnvAllowStubbedModules downgrades the stubbed module boot guard from a fatal
// error to an ERROR log line. Value "1" enables it.
const EnvAllowStubbedModules = "TOKI_ALLOW_STUBBED_MODULES"

const stubGuardHookID = "__tokiStubbedModuleGuard__"

// ModuleMarker declares what a module owns: the system collections/tables it
// creates and the environment variables that configure it. Every real module
// registers its marker with Stubbed=false and its no_<module> stub registers
// the same marker with Stubbed=true. At bootstrap the guard refuses to start
// when a stubbed module still owns data or configuration (see
// [BindStubbedModuleGuard]).
type ModuleMarker struct {
	Name string
	// Tag is the build tag of the stub when it is not "no_"+Name (walreplica: no_replica).
	Tag         string
	Collections []string
	Envs        []string
	// Files are paths relative to the data dir that belong to the module
	// (for example "ruleguard.json").
	Files   []string
	Stubbed bool
	// OffIsActive makes the value "off" of the Envs count as a set variable
	// (needed by TOKI_ADMIN_UI, where "off" is a restrictive mode). By default
	// the values "", "off", "0", "false" and "no" count as unset.
	OffIsActive bool
}

var (
	markersMu sync.Mutex
	markers   = map[string]ModuleMarker{}
)

// RegisterModuleMarker registers (or replaces) the marker of a module.
// Call it from init() of the module package (real implementation and stub).
func RegisterModuleMarker(name string, collections, envs []string, stubbed bool) {
	RegisterModule(ModuleMarker{Name: name, Collections: collections, Envs: envs, Stubbed: stubbed})
}

// RegisterModule is the struct form of [RegisterModuleMarker].
func RegisterModule(m ModuleMarker) {
	markersMu.Lock()
	defer markersMu.Unlock()
	markers[m.Name] = m
}

// ModuleMarkers returns the registered markers sorted by name.
func ModuleMarkers() []ModuleMarker {
	markersMu.Lock()
	defer markersMu.Unlock()
	out := make([]ModuleMarker, 0, len(markers))
	for _, m := range markers {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (m ModuleMarker) tag() string {
	if m.Tag != "" {
		return m.Tag
	}
	return "no_" + m.Name
}

func envActive(m ModuleMarker, getenv func(string) string, name string) bool {
	v := strings.ToLower(strings.TrimSpace(getenv(name)))
	switch v {
	case "":
		return false
	case "off", "0", "false", "no":
		return m.OffIsActive
	}
	return true
}

// StubbedModuleFindings lists, per stubbed module, the owned collections that
// exist in the database and the owned env vars that are set.
func StubbedModuleFindings(app App, ms []ModuleMarker, getenv func(string) string) []string {
	var out []string
	for _, m := range ms {
		if !m.Stubbed {
			continue
		}
		var found []string
		for _, c := range m.Collections {
			if app.HasTable(c) {
				found = append(found, "collection "+c)
			}
		}
		for _, f := range m.Files {
			if app.DataDir() == "" {
				break
			}
			if _, err := os.Stat(filepath.Join(app.DataDir(), f)); err == nil {
				found = append(found, "file "+f)
			}
		}
		for _, e := range m.Envs {
			if envActive(m, getenv, e) {
				found = append(found, "env "+e)
			}
		}
		if len(found) > 0 {
			out = append(out, fmt.Sprintf("module %q (built with %s): %s", m.Name, m.tag(), strings.Join(found, ", ")))
		}
	}
	return out
}

// CheckStubbedModules returns an error naming every stubbed module that still
// owns data or configuration, unless allow is true (then it logs at ERROR and
// returns nil).
func CheckStubbedModules(app App, ms []ModuleMarker, getenv func(string) string, allow bool) error {
	f := StubbedModuleFindings(app, ms, getenv)
	if len(f) == 0 {
		return nil
	}
	if allow {
		for _, line := range f {
			app.Logger().Error("compiled-out module still has data or configuration (guards are NOT active): "+line,
				"override", EnvAllowStubbedModules)
		}
		return nil
	}
	return fmt.Errorf("refusing to start: this binary was built without modules whose data or configuration is present:\n  - %s\nrebuild without the listed no_<module> tags, remove the data/env, or set %s=1 to start anyway (guards stay OFF)",
		strings.Join(f, "\n  - "), EnvAllowStubbedModules)
}

// BindStubbedModuleGuard binds the boot check on app (after a successful bootstrap).
func BindStubbedModuleGuard(app App) {
	app.OnBootstrap().Bind(&hook.Handler[*BootstrapEvent]{
		Id:       stubGuardHookID,
		Priority: -100000, // outermost: runs after every module bootstrapped
		Func: func(e *BootstrapEvent) error {
			if err := e.Next(); err != nil {
				return err
			}
			allow := strings.TrimSpace(os.Getenv(EnvAllowStubbedModules)) == "1"
			return CheckStubbedModules(e.App, ModuleMarkers(), os.Getenv, allow)
		},
	})
}
