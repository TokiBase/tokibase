//go:build !no_ruleguard

package ruleguard

import (
	"fmt"
	"os"
	"strings"

	"github.com/fatih/color"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/hook"
)

const (
	bootstrapHookId = "__ruleguardBootstrap__"
	createHookId    = "__ruleguardCollectionCreate__"
	updateHookId    = "__ruleguardCollectionUpdate__"
)

// Register binds the boot check and the collection save warnings.
//
//   - off: nothing.
//   - warn: log one line per error finding plus a summary (also printed to stderr).
//   - strict: Bootstrap fails with the list of findings.
//
// Collection saves through the Admin UI/API are only ever logged, even in
// strict mode, so the REST contract is unchanged.
func Register(app core.App) {
	app.OnBootstrap().Bind(&hook.Handler[*core.BootstrapEvent]{
		Id:       bootstrapHookId,
		Priority: -1, // outer: runs its post-Next code after the collections cache is loaded
		Func: func(e *core.BootstrapEvent) error {
			if err := e.Next(); err != nil {
				return err
			}
			if isRuleCommand(os.Args[1:]) {
				return nil // `rule lint|allow` must work even with a strict policy
			}
			return bootCheck(e.App)
		},
	})

	app.OnCollectionCreateRequest().Bind(&hook.Handler[*core.CollectionRequestEvent]{
		Id:   createHookId,
		Func: saveCheck,
	})
	app.OnCollectionUpdateRequest().Bind(&hook.Handler[*core.CollectionRequestEvent]{
		Id:   updateHookId,
		Func: saveCheck,
	})
}

func isRuleCommand(args []string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "rule" && (args[i+1] == "lint" || args[i+1] == "allow") {
			return true
		}
	}
	return false
}

func bootCheck(app kernel.App) error {
	pol, err := Load(app.DataDir())
	if err != nil {
		return err
	}
	if pol.Policy == PolicyOff {
		return nil
	}

	findings, err := Lint(app, pol)
	if err != nil {
		return err
	}
	errs := Errors(findings)
	if len(errs) == 0 {
		return nil
	}

	if pol.Policy == PolicyStrict {
		var b strings.Builder
		fmt.Fprintf(&b, "ruleguard: refusing to start, %d public rule(s) not allowlisted in %s:", len(errs), FileName)
		for _, f := range errs {
			fmt.Fprintf(&b, "\n  - %s.%s", f.Collection, f.Rule)
		}
		return fmt.Errorf("%s", b.String())
	}

	for _, f := range errs {
		app.Logger().Warn("ruleguard: public rule not allowlisted", "collection", f.Collection, "rule", f.Rule)
	}
	summary := fmt.Sprintf("ruleguard: %d public rule(s) not allowlisted (run `rule lint` for details, `rule allow` to accept)", len(errs))
	app.Logger().Warn(summary)
	color.New(color.FgYellow, color.Bold).Fprintln(color.Error, "WARNING "+summary)
	return nil
}

func saveCheck(e *core.CollectionRequestEvent) error {
	type key struct{ collection, kind string }
	before := map[key]bool{}

	// the stored version (the event collection already carries the request changes)
	if e.Collection.Id != "" {
		if old, err := e.App.FindCollectionByNameOrId(e.Collection.Id); err == nil {
			for _, r := range collectionRules(old) {
				if isPublic(r.rule) {
					before[key{old.Name, r.kind}] = true
				}
			}
		}
	}

	if err := e.Next(); err != nil {
		return err
	}

	pol, err := Load(e.App.DataDir())
	if err != nil || pol.Policy == PolicyOff || e.Collection.System {
		return nil
	}

	for _, r := range collectionRules(e.Collection) {
		if !isPublic(r.rule) || before[key{e.Collection.Name, r.kind}] || pol.Allowed(e.Collection.Name, r.kind) {
			continue
		}
		e.App.Logger().Warn(
			"ruleguard: collection saved with a new public rule that is not allowlisted",
			"collection", e.Collection.Name,
			"rule", r.kind,
		)
	}
	return nil
}
