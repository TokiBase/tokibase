//go:build !no_wasm

package wasm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/tetratelabs/wazero"
	"github.com/tokibase/tokibase/core"
)

// NewCommand returns the `wasm` cobra command.
func NewCommand(app core.App) *cobra.Command {
	host := func() *Host {
		if h := HostOf(app); h != nil {
			return h
		}
		h := newHost(app, Config{}) // TOKI_WASM=off: CLI still works, hooks are not bound
		_ = h.initSchema()
		h.Reload()
		return h
	}
	root := &cobra.Command{Use: "wasm", Short: "Inspect, test and validate WASM hook modules (toki/1)"}

	var asJSON bool
	list := &cobra.Command{
		Use: "list", Short: "List modules with events, capabilities and limits", SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			h := host()
			mods := h.Modules()
			errs := h.LoadErrors()
			if asJSON {
				out := []Manifest{}
				for _, m := range mods {
					out = append(out, m.Manifest)
				}
				enc := json.NewEncoder(c.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(out)
			}
			w := tabwriter.NewWriter(c.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "MODULE\tSIZE\tTIMEOUT\tMEMORY\tNEEDS\tEVENTS")
			for _, m := range mods {
				fmt.Fprintf(w, "%s\t%dK\t%dms\t%dp (%dMB)\t%s\t%s\n", m.Name, m.Size/1024, m.TimeoutMS, m.MemoryPages,
					m.MemoryPages*64/1024, orDash(strings.Join(m.Needs, ",")), orDash(strings.Join(m.Events, "; ")))
			}
			for name, err := range errs {
				fmt.Fprintf(w, "%s\t-\t-\t-\t-\tNOT LOADED: %v\n", name, err)
			}
			w.Flush()
			fmt.Fprintf(c.ErrOrStderr(), "dir: %s\n", h.Dir())
			return nil
		},
	}
	list.Flags().BoolVar(&asJSON, "json", false, "output JSON")

	stats := &cobra.Command{
		Use: "stats", Short: "Per-module call counters (calls, errors, average ms)", SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			snaps, err := host().StoredStats()
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(c.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "MODULE\tCALLS\tERRORS\tAVG MS\tLAST CALL\tLAST ERROR")
			for _, s := range snaps {
				avg := 0.0
				if s.Calls > 0 {
					avg = s.TotalMS / float64(s.Calls)
				}
				last := "-"
				if !s.LastCall.IsZero() {
					last = s.LastCall.Local().Format(time.DateTime)
				}
				fmt.Fprintf(w, "%s\t%d\t%d\t%.2f\t%s\t%s\n", s.Module, s.Calls, s.Errors, avg, last, orDash(s.LastErr))
			}
			return w.Flush()
		},
	}

	var event, payload string
	var commit bool
	run := &cobra.Command{
		Use: "run <module>", Short: "Invoke a module offline with an event payload (side effects are suppressed unless --commit)",
		Args: cobra.ExactArgs(1), SilenceUsage: true,
		RunE: func(c *cobra.Command, args []string) error {
			h := host()
			m := h.Module(args[0])
			if m == nil {
				return fmt.Errorf("module %q not loaded (see `wasm list`)", args[0])
			}
			ev := &EventIn{Kind: "record", Actor: Actor{Kind: "system"}}
			if payload != "" {
				b, err := os.ReadFile(payload)
				if err != nil {
					return err
				}
				if err := json.Unmarshal(b, ev); err != nil {
					return fmt.Errorf("payload: %w", err)
				}
			}
			if event != "" {
				ev.Event = event
			}
			if ev.Event == "" {
				return fmt.Errorf("--event is required (or set \"event\" in the payload)")
			}
			if ev.Kind == "" || (payload == "" || ev.Kind == "record") {
				switch pe, _ := ParseEvent(ev.Event); pe.Kind {
				case KindCron:
					ev.Kind = "cron"
				case KindRoute:
					ev.Kind = "route"
				case KindJob:
					ev.Kind = "job"
				default:
					ev.Kind = "record"
				}
			}
			var effects []map[string]any
			res, err := h.Invoke(context.Background(), m, ev, CallOpts{DryRun: !commit, Effects: &effects})
			out := map[string]any{"module": m.Name, "event": ev.Event, "commit": commit}
			if err != nil {
				out["error"] = err.Error()
				if ce, ok := err.(*CallError); ok && ce.Stderr != "" {
					out["stderr"] = ce.Stderr
				}
			} else {
				out["result"] = res
			}
			if len(effects) > 0 {
				out["suppressed_effects"] = effects
			}
			enc := json.NewEncoder(c.OutOrStdout())
			enc.SetIndent("", "  ")
			if e := enc.Encode(out); e != nil {
				return e
			}
			return err
		},
	}
	run.Flags().StringVar(&event, "event", "", "event name, e.g. record.create.posts or \"route:POST /api/hello\"")
	run.Flags().StringVar(&payload, "payload", "", "JSON file with the event document (record, actor, request_info, ...)")
	run.Flags().BoolVar(&commit, "commit", false, "execute record/kv/mail/jobs host calls for real (default: dry run)")

	validate := &cobra.Command{
		Use: "validate <file.wasm>", Short: "Check that a file compiles and follows the toki/1 ABI", Args: cobra.ExactArgs(1), SilenceUsage: true,
		RunE: func(c *cobra.Command, args []string) error {
			rep, err := Validate(args[0])
			for _, l := range rep {
				fmt.Fprintln(c.OutOrStdout(), l)
			}
			return err
		},
	}

	root.AddCommand(list, stats, run, validate)
	return root
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// Validate compiles file and checks imports/exports against the ABI. The
// returned lines are a human readable report; err is non-nil on any problem.
func Validate(file string) ([]string, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	rt := wazero.NewRuntime(ctx)
	defer rt.Close(ctx)
	cm, err := rt.CompileModule(ctx, b)
	if err != nil {
		return nil, fmt.Errorf("not a valid WebAssembly module: %w", err)
	}
	rep := []string{fmt.Sprintf("%s: %d bytes", file, len(b))}
	var problems []string
	needAlloc := false
	for _, f := range cm.ImportedFunctions() {
		mod, name, _ := f.Import()
		rep = append(rep, fmt.Sprintf("  import %s.%s", mod, name))
		switch {
		case mod == "toki":
			if !tokiFuncs[name] {
				problems = append(problems, fmt.Sprintf("unknown host function toki.%s", name))
			}
			if name != "log" {
				needAlloc = true
			}
		case mod != "wasi_snapshot_preview1":
			problems = append(problems, fmt.Sprintf("unsupported import module %q", mod))
		}
	}
	exp := map[string]bool{}
	for name := range cm.ExportedFunctions() {
		exp[name] = true
	}
	for name := range cm.ExportedMemories() {
		exp["memory:"+name] = true
	}
	for _, k := range []string{"_start", "toki_alloc", "toki_free", "memory:memory"} {
		rep = append(rep, fmt.Sprintf("  export %s: %v", strings.TrimPrefix(k, "memory:"), exp[k]))
	}
	if !exp["_start"] {
		problems = append(problems, "missing _start export (build a WASI command: GOOS=wasip1 for Go, wasm32-wasip1 for Rust)")
	}
	if !exp["memory:memory"] {
		problems = append(problems, "missing exported memory")
	}
	if needAlloc && !exp["toki_alloc"] {
		problems = append(problems, "guest imports host calls that return data but does not export toki_alloc")
	}
	if !exp["toki_free"] {
		rep = append(rep, "  warning: toki_free not exported (recommended)")
	}
	if len(problems) > 0 {
		for _, p := range problems {
			rep = append(rep, "  ERROR: "+p)
		}
		return rep, fmt.Errorf("%d problem(s) found", len(problems))
	}
	rep = append(rep, "  OK (toki/1)")
	return rep, nil
}
