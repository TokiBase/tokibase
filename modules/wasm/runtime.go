//go:build !no_wasm

package wasm

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	wsys "github.com/tetratelabs/wazero/sys"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
)

// Module is a compiled guest with its limits, runtime and metrics. It is
// immutable after load; hot reload builds a new Module.
type Module struct {
	Manifest
	Parsed []ParsedEvent
	Size   int64
	Hash   string

	rt       wazero.Runtime
	compiled wazero.CompiledModule
	sem      chan struct{} // instance pool: bounds concurrent instances
	Stats    *Stats
}

// Stats are cheap in-memory counters, flushed to `_wasm_stats`.
type Stats struct {
	calls, errors, nanos atomic.Int64
	mu                   sync.Mutex
	lastErr              string
	lastCall             time.Time
}

// Snapshot is a copy of the counters.
type Snapshot struct {
	Module   string
	Calls    int64
	Errors   int64
	TotalMS  float64
	LastErr  string
	LastCall time.Time
}

func (s *Stats) record(d time.Duration, err error) {
	s.calls.Add(1)
	s.nanos.Add(int64(d))
	s.mu.Lock()
	s.lastCall = time.Now().UTC()
	if err != nil {
		s.errors.Add(1)
		s.lastErr = trunc(err.Error(), 500)
	}
	s.mu.Unlock()
}

// drain returns and resets the counters (for flushing).
func (s *Stats) drain(name string) Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	sn := Snapshot{Module: name, Calls: s.calls.Swap(0), Errors: s.errors.Swap(0),
		TotalMS: float64(s.nanos.Swap(0)) / 1e6, LastErr: s.lastErr, LastCall: s.lastCall}
	return sn
}

// restore adds back counters whose flush failed.
func (s *Stats) restore(sn Snapshot) {
	s.calls.Add(sn.Calls)
	s.errors.Add(sn.Errors)
	s.nanos.Add(int64(sn.TotalMS * 1e6))
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// CallError is a failed guest execution (trap, timeout, bad output...).
// Details are for logs; Public is safe to show clients.
type CallError struct {
	Kind   string // timeout | trap | output | exit | memory
	Detail string
	Stderr string
}

func (e *CallError) Error() string { return e.Kind + ": " + e.Detail }

// PublicMessage is the generic text clients see.
func (e *CallError) PublicMessage() string { return "Hook failed." }

type callKey struct{}

// MaxCallDepth caps nested guest calls (a guest write triggers a hook of a
// guest that writes ...): the top level call has depth 0.
const MaxCallDepth = 2

// call is the per-invocation state the host functions consult.
type call struct {
	h     *Host
	mod   *Module
	actor Actor
	dry   bool
	// app is the app of the event that triggered the call: inside a
	// transaction (batch, cascades, RunInTransaction) it is the tx app, so
	// host functions must use it (never h.app) or they would wait forever for
	// the single write connection the transaction holds.
	app    kernel.App
	mu     sync.Mutex
	effect []map[string]any
	depth  int
}

// db returns the app host functions must use.
func (c *call) db() core.App {
	if c.app != nil {
		if a := core.AsApp(c.app); a != nil {
			return a
		}
	}
	return c.h.app
}

func (c *call) addEffect(kind string, v map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v["effect"] = kind
	c.effect = append(c.effect, v)
}

// Effects returns the dry-run side effects recorded so far.
func (c *call) Effects() []map[string]any { return c.effect }

// CallOpts carries invocation settings.
type CallOpts struct {
	DryRun bool
	Actor  Actor
	// App is the app of the triggering event (transaction aware); nil = the host app.
	App kernel.App
	// Effects receives the suppressed side effects of a dry run.
	Effects *[]map[string]any
}

type limitedWriter struct {
	buf   bytes.Buffer
	limit int
	over  bool
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if w.buf.Len()+len(p) > w.limit {
		w.over = true
		return 0, errors.New("stdout limit exceeded")
	}
	return w.buf.Write(p)
}

// compileModule compiles file with its own runtime (one per module because
// the memory limit is a runtime setting); the compilation cache is shared.
func (h *Host) compileModule(m *Manifest) (*Module, error) {
	wasmBytes, err := os.ReadFile(m.File)
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	cfg := wazero.NewRuntimeConfig().
		WithCloseOnContextDone(true).
		WithMemoryLimitPages(uint32(m.MemoryPages)).
		WithCompilationCache(h.cache)
	rt := wazero.NewRuntimeWithConfig(ctx, cfg)
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		rt.Close(ctx)
		return nil, err
	}
	if err := h.instantiateHost(ctx, rt); err != nil {
		rt.Close(ctx)
		return nil, err
	}
	compiled, err := rt.CompileModule(ctx, wasmBytes)
	if err != nil {
		rt.Close(ctx)
		return nil, fmt.Errorf("compile %s: %w", m.File, err)
	}
	if err := checkImports(compiled); err != nil {
		rt.Close(ctx)
		return nil, err
	}
	pool := runtime.GOMAXPROCS(0) * 2
	if pool < 4 {
		pool = 4
	}
	if pool > 16 {
		pool = 16
	}
	return &Module{
		Manifest: *m, Parsed: m.ParsedEvents(), Size: int64(len(wasmBytes)), Hash: hashBytes(wasmBytes),
		rt: rt, compiled: compiled, sem: make(chan struct{}, pool), Stats: &Stats{},
	}, nil
}

// retire closes the runtime once no call can still be running (every call is
// bounded by the module timeout).
func (m *Module) retire() {
	time.AfterFunc(time.Duration(m.TimeoutMS)*time.Millisecond+5*time.Second, func() {
		m.rt.Close(context.Background())
	})
}

// Invoke runs the module once with ev on stdin, in a fresh instance.
func (h *Host) Invoke(ctx context.Context, m *Module, ev *EventIn, opts CallOpts) (res *Result, err error) {
	start := time.Now()
	defer func() {
		if r := recover(); r != nil {
			err = &CallError{Kind: "trap", Detail: fmt.Sprintf("host panic: %v", r)}
		}
		m.Stats.record(time.Since(start), err)
		if err != nil {
			h.logCallError(m, ev, err)
		}
	}()

	ev.ABI, ev.Module = ABI, m.Name
	if ev.Time == "" {
		ev.Time = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if ev.Actor.Kind == "" {
		ev.Actor = opts.Actor
	}
	in, err := json.Marshal(ev)
	if err != nil {
		return nil, &CallError{Kind: "output", Detail: "cannot encode event: " + err.Error()}
	}

	timeout := time.Duration(m.TimeoutMS) * time.Millisecond
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// instance pool slot
	select {
	case m.sem <- struct{}{}:
		defer func() { <-m.sem }()
	case <-ctx.Done():
		return nil, &CallError{Kind: "timeout", Detail: "no free instance slot within the timeout"}
	}

	cs := &call{h: h, mod: m, actor: ev.Actor, dry: opts.DryRun, app: opts.App}
	if parent, ok := ctx.Value(callKey{}).(*call); ok {
		cs.depth = parent.depth + 1
	}
	if cs.depth > MaxCallDepth {
		return nil, &CallError{Kind: "exit", Detail: "hook call depth exceeded"}
	}
	ctx = context.WithValue(ctx, callKey{}, cs)

	stdout := &limitedWriter{limit: MaxStdoutBytes}
	stderr := &limitedWriter{limit: 64 << 10}
	mc := wazero.NewModuleConfig().
		WithName("").
		WithStdin(bytes.NewReader(in)).
		WithStdout(stdout).
		WithStderr(stderr).
		WithRandSource(rand.Reader).
		WithArgs(m.Name)
	for k, v := range m.Env {
		mc = mc.WithEnv(k, v)
	}

	inst, ierr := m.rt.InstantiateModule(ctx, m.compiled, mc)
	if inst != nil {
		inst.Close(context.Background())
	}
	if opts.Effects != nil {
		*opts.Effects = append(*opts.Effects, cs.Effects()...)
	}
	if ierr != nil {
		var ee *wsys.ExitError
		switch {
		case errors.As(ierr, &ee) && ee.ExitCode() == 0:
			// proc_exit(0): normal end
		case errors.As(ierr, &ee) && (ee.ExitCode() == wsys.ExitCodeDeadlineExceeded || ctx.Err() != nil):
			return nil, &CallError{Kind: "timeout", Detail: fmt.Sprintf("exceeded %s", timeout), Stderr: stderr.buf.String()}
		case errors.As(ierr, &ee):
			return nil, &CallError{Kind: "exit", Detail: fmt.Sprintf("guest exited with code %d", ee.ExitCode()), Stderr: stderr.buf.String()}
		case ctx.Err() != nil:
			return nil, &CallError{Kind: "timeout", Detail: fmt.Sprintf("exceeded %s", timeout), Stderr: stderr.buf.String()}
		default:
			k := "trap"
			if strings.Contains(ierr.Error(), "memory") {
				k = "memory"
			}
			return nil, &CallError{Kind: k, Detail: ierr.Error(), Stderr: stderr.buf.String()}
		}
	}
	if stdout.over {
		return nil, &CallError{Kind: "output", Detail: fmt.Sprintf("stdout exceeded %d bytes", MaxStdoutBytes), Stderr: stderr.buf.String()}
	}
	out := bytes.TrimSpace(stdout.buf.Bytes())
	if len(out) == 0 {
		return nil, &CallError{Kind: "output", Detail: "guest wrote no result to stdout", Stderr: stderr.buf.String()}
	}
	var r Result
	if err := json.Unmarshal(out, &r); err != nil {
		return nil, &CallError{Kind: "output", Detail: "invalid result JSON: " + err.Error(), Stderr: stderr.buf.String()}
	}
	return &r, nil
}

// checkImports rejects modules importing anything but WASI preview1 and toki/1.
func checkImports(cm wazero.CompiledModule) error {
	for _, f := range cm.ImportedFunctions() {
		mod, name, _ := f.Import()
		switch mod {
		case "wasi_snapshot_preview1":
		case "toki":
			if !tokiFuncs[name] {
				return fmt.Errorf("guest imports unknown host function toki.%s (ABI %s)", name, ABI)
			}
		default:
			return fmt.Errorf("guest imports %s.%s: only wasi_snapshot_preview1 and toki are provided", mod, name)
		}
	}
	return nil
}

var tokiFuncs = map[string]bool{
	"records_find": true, "records_save": true, "records_delete": true, "http_fetch": true,
	"mail_send": true, "log": true, "kv_get": true, "kv_set": true, "jobs_enqueue": true,
}
