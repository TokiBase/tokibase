//go:build !no_wasm

// Package wasm runs business logic compiled to WASI (any language) as
// sandboxed record, cron, route and job hooks on top of wazero. See
// docs/modules/wasm.md for the toki/1 guest ABI.
package wasm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	validation "github.com/pocketbase/ozzo-validation/v4"
	"github.com/spf13/cobra"
	"github.com/tetratelabs/wazero"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/hook"
	"github.com/tokibase/tokibase/tools/router"
)

const (
	hookID      = "__tokiWasm__"
	storeKey    = "__tokiWasmHost__"
	jobKindCron = "wasm.cron"
	jobKindJob  = "wasm.job"
	// DefaultDirName is the hooks directory next to the data dir.
	DefaultDirName = "pb_hooks_wasm"
)

// Enabled reports whether the module is on (TOKI_WASM=off disables it).
func Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TOKI_WASM"))) {
	case "off", "false", "0", "disabled":
		return false
	}
	return true
}

// Config configures a Host (the CLI flags fill it in Register).
type Config struct {
	Dir   string // hooks dir; "" = <dataDir>/../pb_hooks_wasm
	Watch bool   // reload on file change
}

type registry struct {
	mods   []*Module
	byName map[string]*Module
}

// Host owns the loaded modules and the hook bindings of one app.
type Host struct {
	app core.App
	cfg Config
	// flag pointers (set by Register, read lazily after flag parsing)
	dirFlag   *string
	watchFlag *bool

	cache wazero.CompilationCache
	reg   atomic.Pointer[registry]

	mu         sync.Mutex // serializes Reload
	errs       map[string]error
	cronIDs    []string
	routes     map[string]bool
	served     bool
	stash      sync.Map // *core.Record -> *core.RequestEvent
	inlineCron sync.Map
	batchBound bool // guarded by mu
	syncBound  bool // guarded by mu
	stopOnce   sync.Once
	stop       chan struct{}
}

// Register wires the module into app. It adds the --wasmHooksDir and
// --wasmHooksWatch flags to root (when non-nil). It is a no-op when
// TOKI_WASM=off.
func Register(app core.App, root *cobra.Command) *Host {
	if !Enabled() {
		return nil
	}
	h := newHost(app, Config{})
	if root != nil {
		h.dirFlag = new(string)
		h.watchFlag = new(bool)
		root.PersistentFlags().StringVar(h.dirFlag, "wasmHooksDir", "", "the directory with the WASM app hooks (default <dataDir>/../pb_hooks_wasm)")
		root.PersistentFlags().BoolVar(h.watchFlag, "wasmHooksWatch", false, "reload WASM hooks when a file in wasmHooksDir changes")
	}
	h.install()
	return h
}

// RegisterWithConfig is Register with an explicit Config (tests, embedding).
func RegisterWithConfig(app core.App, cfg Config) *Host {
	h := newHost(app, cfg)
	h.install()
	return h
}

// HostOf returns the Host registered on app, or nil.
func HostOf(app core.App) *Host {
	h, _ := app.Store().Get(storeKey).(*Host)
	return h
}

func newHost(app core.App, cfg Config) *Host {
	cache := openCompilationCache(strings.TrimSpace(os.Getenv("TOKI_WASM_CACHE_DIR")), app.Logger())
	h := &Host{app: app, cfg: cfg, cache: cache, routes: map[string]bool{}, errs: map[string]error{}, stop: make(chan struct{})}
	h.reg.Store(&registry{byName: map[string]*Module{}})
	app.Store().Set(storeKey, h)
	return h
}

// Dir returns the effective hooks directory.
func (h *Host) Dir() string {
	switch {
	case h.dirFlag != nil && *h.dirFlag != "":
		return *h.dirFlag
	case h.cfg.Dir != "":
		return h.cfg.Dir
	}
	return filepath.Join(h.app.DataDir(), "..", DefaultDirName)
}

func (h *Host) watch() bool { return h.cfg.Watch || (h.watchFlag != nil && *h.watchFlag) }

// Modules returns the currently loaded modules.
func (h *Host) Modules() []*Module { return h.reg.Load().mods }

// Module returns a loaded module by name.
func (h *Host) Module(name string) *Module { return h.reg.Load().byName[name] }

// LoadErrors returns problems found by the last (re)load, by module name.
func (h *Host) LoadErrors() map[string]error {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[string]error, len(h.errs))
	for k, v := range h.errs {
		out[k] = v
	}
	return out
}

const createSQL = `
CREATE TABLE IF NOT EXISTS {{_wasm_kv}} (
	[[module]]  TEXT NOT NULL,
	[[key]]     TEXT NOT NULL,
	[[value]]   TEXT NOT NULL DEFAULT '',
	[[expires]] INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY ([[module]], [[key]])
);
CREATE TABLE IF NOT EXISTS {{_wasm_stats}} (
	[[module]]    TEXT PRIMARY KEY NOT NULL,
	[[calls]]     INTEGER NOT NULL DEFAULT 0,
	[[errors]]    INTEGER NOT NULL DEFAULT 0,
	[[total_ms]]  REAL NOT NULL DEFAULT 0,
	[[last_error]] TEXT NOT NULL DEFAULT '',
	[[last_call]] TEXT NOT NULL DEFAULT ''
);`

func (h *Host) initSchema() error {
	for _, stmt := range strings.Split(createSQL, ");") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := h.app.AuxNonconcurrentDB().NewQuery(stmt + ")").Execute(); err != nil {
			return err
		}
	}
	return nil
}

// install binds bootstrap, hooks, serve and terminate handlers.
func (h *Host) install() {
	app := h.app
	boot := func() {
		if err := h.initSchema(); err != nil {
			app.Logger().Error("wasm: failed to initialize tables", "error", err)
		}
		h.Reload()
	}
	if app.IsBootstrapped() {
		boot()
	}
	app.OnBootstrap().Bind(&hook.Handler[*core.BootstrapEvent]{
		Id: hookID,
		Func: func(e *core.BootstrapEvent) error {
			if err := e.Next(); err != nil {
				return err
			}
			boot()
			return nil
		},
	})
	h.bindRecordHooks()
	kernel.Jobs(app).Register(jobKindCron, h.cronJob)
	kernel.Jobs(app).Register(jobKindJob, h.jobJob)

	app.OnServe().Bind(&hook.Handler[*core.ServeEvent]{
		Id: hookID,
		Func: func(e *core.ServeEvent) error {
			h.bindRoutes(e.Router)
			h.startBackground()
			return e.Next()
		},
	})
	app.OnTerminate().Bind(&hook.Handler[*core.TerminateEvent]{
		Id: hookID,
		Func: func(e *core.TerminateEvent) error {
			h.Close()
			return e.Next()
		},
	})
}

// Close stops background work and flushes metrics.
func (h *Host) Close() {
	h.stopOnce.Do(func() { close(h.stop) })
	h.FlushStats()
	// the cache is released once the last call finished (calls are bounded by their timeout)
	time.AfterFunc(MaxTimeoutMS*time.Millisecond+10*time.Second, func() { _ = h.cache.Close(context.Background()) })
}

// Reload rescans the hooks directory and atomically swaps the module set;
// in-flight calls finish on the old instances. Modules that fail to compile
// are skipped (logged) and do not affect the others.
func (h *Host) Reload() {
	h.mu.Lock()
	defer h.mu.Unlock()
	prev := h.reg.Load()
	if _, err := os.Stat(h.Dir()); err != nil && len(prev.mods) > 0 {
		h.app.Logger().Error("wasm: hooks directory unreadable, keeping the loaded modules", "dir", h.Dir(), "error", err)
		return
	}
	mans, errs := discover(h.Dir())
	next := &registry{byName: map[string]*Module{}}
	kept := map[*Module]bool{}
	// fail closed: a module that no longer compiles or parses keeps its previous
	// good version (a broken file must never silently drop a validation hook)
	keepPrev := func(name string, err error) {
		if pm := prev.byName[name]; pm != nil {
			h.app.Logger().Error("wasm: reload failed, keeping the previous version of the module", "module", name, "error", err)
			next.mods = append(next.mods, pm)
			next.byName[name] = pm
			kept[pm] = true
		}
	}
	for _, m := range mans {
		cm, err := h.compileModule(m)
		if err != nil {
			errs[m.Name] = err
			keepPrev(m.Name, err)
			continue
		}
		next.mods = append(next.mods, cm)
		next.byName[cm.Name] = cm
	}
	for name, err := range errs {
		if next.byName[name] == nil {
			keepPrev(name, err)
		}
	}
	sort.Slice(next.mods, func(i, j int) bool { return next.mods[i].Name < next.mods[j].Name })
	for name, err := range errs {
		h.app.Logger().Error("wasm: module not loaded", "module", name, "error", err)
	}
	h.errs = errs
	old := h.reg.Swap(next)
	h.flushRegistry(old)
	for _, m := range old.mods {
		if !kept[m] {
			m.retire()
		}
	}
	// warn about duplicate route claims and routes that need a restart
	seen := map[string]string{}
	for _, m := range next.mods {
		for _, e := range m.Parsed {
			if e.Kind != KindRoute {
				continue
			}
			k := e.Method + " " + e.Path
			if o, dup := seen[k]; dup {
				h.app.Logger().Warn("wasm: route claimed by two modules, first wins", "route", k, "first", o, "second", m.Name)
				continue
			}
			seen[k] = m.Name
			if h.served && !h.routes[k] {
				h.app.Logger().Warn("wasm: new route requires a restart to be served", "route", k, "module", m.Name)
			}
		}
	}
	h.syncCron(next)
	h.syncBatch(next)
	h.syncSyncConflict(next)
	if len(next.mods) > 0 {
		h.app.Logger().Info("wasm: hooks loaded", "dir", h.Dir(), "modules", len(next.mods))
	}
}

// ---- hot reload + background ----

func (h *Host) startBackground() {
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-h.stop:
				return
			case <-t.C:
				h.FlushStats()
				h.purgeKV()
			}
		}
	}()
	if h.watch() {
		if err := h.startWatcher(); err != nil {
			h.app.Logger().Warn("wasm: file watcher not started", "error", err)
		}
	}
}

// ReloadDebounce is the quiet period after the last file event before a reload.
const ReloadDebounce = time.Second

// reloadWhenStable waits until the size of every .wasm/.toml file of the hooks
// directory is the same across two reads (a copy or scp in progress changes
// it), then reloads. A reload of a still broken file fails closed anyway.
func (h *Host) reloadWhenStable() {
	if !waitStable(h.Dir(), 250*time.Millisecond, 40) {
		h.app.Logger().Warn("wasm: hooks directory still changing, reloading anyway", "dir", h.Dir())
	}
	h.Reload()
}

func dirSizes(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var b strings.Builder
	for _, e := range entries {
		if ext := filepath.Ext(e.Name()); ext != ".wasm" && ext != ".toml" {
			continue
		}
		if fi, err := e.Info(); err == nil {
			fmt.Fprintf(&b, "%s:%d;", e.Name(), fi.Size())
		}
	}
	return b.String()
}

// waitStable polls the file sizes every interval until two consecutive reads
// agree (true) or tries is exhausted (false).
func waitStable(dir string, interval time.Duration, tries int) bool {
	prev := dirSizes(dir)
	for i := 0; i < tries; i++ {
		time.Sleep(interval)
		cur := dirSizes(dir)
		if cur == prev {
			return true
		}
		prev = cur
	}
	return false
}

func (h *Host) startWatcher() error {
	dir := h.Dir()
	if _, err := os.Stat(dir); err != nil {
		return err
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	if err := w.Add(dir); err != nil {
		w.Close()
		return err
	}
	go func() {
		defer w.Close()
		var timer *time.Timer
		for {
			select {
			case <-h.stop:
				return
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				if ext := filepath.Ext(ev.Name); ext != ".wasm" && ext != ".toml" {
					continue
				}
				if timer != nil {
					timer.Stop()
				}
				timer = time.AfterFunc(ReloadDebounce, h.reloadWhenStable)
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				h.app.Logger().Warn("wasm: watch error", "error", err)
			}
		}
	}()
	return nil
}

// ---- stats ----

func (h *Host) flushRegistry(r *registry) {
	for _, m := range r.mods {
		sn := m.Stats.drain(m.Name)
		if sn.Calls == 0 {
			continue
		}
		last := ""
		if !sn.LastCall.IsZero() {
			last = sn.LastCall.Format(time.RFC3339)
		}
		_, err := h.app.AuxNonconcurrentDB().NewQuery(`INSERT INTO {{_wasm_stats}} ([[module]],[[calls]],[[errors]],[[total_ms]],[[last_error]],[[last_call]])
			VALUES ({:m},{:c},{:e},{:t},{:le},{:lc})
			ON CONFLICT([[module]]) DO UPDATE SET [[calls]]=[[calls]]+excluded.[[calls]], [[errors]]=[[errors]]+excluded.[[errors]],
			[[total_ms]]=[[total_ms]]+excluded.[[total_ms]], [[last_error]]=CASE WHEN excluded.[[last_error]]<>'' THEN excluded.[[last_error]] ELSE [[last_error]] END,
			[[last_call]]=excluded.[[last_call]]`).
			Bind(map[string]any{"m": sn.Module, "c": sn.Calls, "e": sn.Errors, "t": sn.TotalMS, "le": sn.LastErr, "lc": last}).Execute()
		if err != nil {
			h.app.Logger().Warn("wasm: failed to flush stats", "module", m.Name, "error", err)
			m.Stats.restore(sn)
		}
	}
}

// FlushStats persists the in-memory counters of the loaded modules.
func (h *Host) FlushStats() { h.flushRegistry(h.reg.Load()) }

func (h *Host) purgeKV() {
	h.app.AuxNonconcurrentDB().NewQuery(`DELETE FROM {{_wasm_kv}} WHERE [[expires]]>0 AND [[expires]]<{:n}`).
		Bind(map[string]any{"n": time.Now().Unix()}).Execute()
}

// StoredStats reads the persisted counters (what `toki wasm stats` prints).
func (h *Host) StoredStats() ([]Snapshot, error) {
	h.FlushStats()
	var rows []struct {
		Module  string  `db:"module"`
		Calls   int64   `db:"calls"`
		Errors  int64   `db:"errors"`
		TotalMS float64 `db:"total_ms"`
		LastErr string  `db:"last_error"`
		LastCal string  `db:"last_call"`
	}
	if err := h.app.AuxDB().NewQuery(`SELECT * FROM {{_wasm_stats}} ORDER BY [[module]]`).All(&rows); err != nil {
		return nil, err
	}
	out := make([]Snapshot, 0, len(rows))
	for _, r := range rows {
		t, _ := time.Parse(time.RFC3339, r.LastCal)
		out = append(out, Snapshot{Module: r.Module, Calls: r.Calls, Errors: r.Errors, TotalMS: r.TotalMS, LastErr: r.LastErr, LastCall: t})
	}
	return out, nil
}

func (h *Host) logCallError(m *Module, ev *EventIn, err error) {
	l := h.app.Logger().With("wasm_module", m.Name, "event", ev.Event)
	var ce *CallError
	if errors.As(err, &ce) {
		l.Error("wasm: hook call failed", "kind", ce.Kind, "detail", ce.Detail, "stderr", trunc(ce.Stderr, 2000))
		return
	}
	l.Error("wasm: hook call failed", "error", err)
}

// ---- record hooks ----

func (h *Host) matchRecord(after bool, action, collection string) []*Module {
	var out []*Module
	for _, m := range h.reg.Load().mods {
		for _, e := range m.Parsed {
			if e.matchRecord(after, action, collection) {
				out = append(out, m)
				break
			}
		}
	}
	return out
}

func (h *Host) actorOf(rec *core.Record) (Actor, *core.RequestEvent) {
	if v, ok := h.stash.Load(rec); ok {
		re := v.(*core.RequestEvent)
		return actorFromAuth(re.Auth), re
	}
	return Actor{Kind: "system"}, nil
}

func actorFromAuth(a *core.Record) Actor {
	switch {
	case a == nil:
		return Actor{Kind: "guest"}
	case a.IsSuperuser():
		return Actor{Kind: "superuser", ID: a.Id, Collection: a.Collection().Name}
	}
	return Actor{Kind: "auth", ID: a.Id, Collection: a.Collection().Name}
}

var reservedFields = map[string]bool{"id": true, "collectionId": true, "collectionName": true, "expand": true, "tokenKey": true, "passwordHash": true}

func (h *Host) bindRecordHooks() {
	app := h.app
	type pair struct {
		action string
		before *hook.TaggedHook[*core.RecordEvent]
		after  *hook.TaggedHook[*core.RecordEvent]
		req    *hook.TaggedHook[*core.RecordRequestEvent]
	}
	for _, p := range []pair{
		{"create", app.OnRecordCreate(), app.OnRecordAfterCreateSuccess(), app.OnRecordCreateRequest()},
		{"update", app.OnRecordUpdate(), app.OnRecordAfterUpdateSuccess(), app.OnRecordUpdateRequest()},
		{"delete", app.OnRecordDelete(), app.OnRecordAfterDeleteSuccess(), app.OnRecordDeleteRequest()},
	} {
		action := p.action
		// remember the HTTP request that triggered the write (actor + request_info)
		p.req.Bind(&hook.Handler[*core.RecordRequestEvent]{Id: hookID, Priority: -1000, Func: func(e *core.RecordRequestEvent) error {
			if len(h.reg.Load().mods) == 0 {
				return e.Next()
			}
			h.stash.Store(e.Record, e.RequestEvent)
			defer h.stash.Delete(e.Record)
			return e.Next()
		}})
		p.before.Bind(&hook.Handler[*core.RecordEvent]{Id: hookID, Func: func(e *core.RecordEvent) error {
			if err := h.runRecord(false, action, e); err != nil {
				return err
			}
			return e.Next()
		}})
		p.after.Bind(&hook.Handler[*core.RecordEvent]{Id: hookID, Func: func(e *core.RecordEvent) error {
			if err := e.Next(); err != nil {
				return err
			}
			// read-only: failures are logged by Invoke and never fail the write
			_ = h.runRecord(true, action, e)
			return nil
		}})
	}
}

func (h *Host) runRecord(after bool, action string, e *core.RecordEvent) error {
	if after && kernel.IsSyncReplica(e.Context) {
		// sync pull/snapshot/bundle apply: the after-handlers ran once, on the hub
		return nil
	}
	mods := h.matchRecord(after, action, e.Record.Collection().Name)
	if len(mods) == 0 {
		return nil
	}
	if _, internal := internalSaves.Load(e.Record); internal {
		return nil
	}
	ctx := e.Context
	if ctx == nil {
		ctx = context.Background()
	}
	actor, re := h.actorOf(e.Record)
	phase, name := "before", "record."+action+"."+e.Record.Collection().Name
	if after {
		phase, name = "after", "record.after."+action+"."+e.Record.Collection().Name
	}
	for _, m := range mods {
		ev := &EventIn{
			Event: name, Kind: "record", Phase: phase, Action: action, Collection: e.Record.Collection().Name,
			Record: e.Record.PublicExport(), Actor: actor,
		}
		if action != "create" {
			if orig := e.Record.Original(); orig != nil {
				ev.Original = orig.PublicExport()
			}
		}
		if re != nil {
			if info, err := re.RequestInfo(); err == nil {
				ev.RequestInfo = sanitizeRequestInfo(info)
			}
		}
		res, err := h.Invoke(ctx, m, ev, CallOpts{App: e.App})
		if err != nil {
			if after {
				return nil
			}
			var ce *CallError
			if errors.As(err, &ce) {
				return router.NewApiError(http.StatusInternalServerError, ce.PublicMessage(), nil)
			}
			return router.NewApiError(http.StatusInternalServerError, "Hook failed.", nil)
		}
		if after {
			if !res.OK {
				h.app.Logger().Warn("wasm: after-hook reported failure", "module", m.Name, "event", name, "message", res.Message)
			}
			continue
		}
		if !res.OK {
			status := res.Status
			if status < 400 || status > 599 {
				status = http.StatusBadRequest
			}
			msg := res.Message
			if msg == "" {
				msg = "Rejected by hook."
			}
			return router.NewApiError(status, msg, safeData(res.Data))
		}
		if action != "delete" {
			for k, v := range res.Record {
				if !reservedFields[k] {
					e.Record.Set(k, v)
				}
			}
		}
	}
	return nil
}

// ---- cron + jobs ----

type jobPayload struct {
	Module  string          `json:"module"`
	Name    string          `json:"name,omitempty"`
	Expr    string          `json:"expr,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

func (h *Host) syncCron(r *registry) {
	cr := h.app.Cron()
	for _, id := range h.cronIDs {
		cr.Remove(id)
	}
	h.cronIDs = nil
	for _, m := range r.mods {
		for i, e := range m.Parsed {
			if e.Kind != KindCron {
				continue
			}
			mod, expr := m.Name, e.Cron
			id := fmt.Sprintf("__tokiWasm__%s__%d", mod, i)
			err := cr.Add(id, expr, func() { h.cronTick(mod, expr) })
			if err != nil {
				h.app.Logger().Error("wasm: cannot schedule cron", "module", mod, "expr", expr, "error", err)
				continue
			}
			h.cronIDs = append(h.cronIDs, id)
		}
	}
}

// cronTick enqueues one durable job per schedule slot. The slot key is the
// jobs cron_key (a FULL unique index, kept after the job finished), so several
// processes sharing the DB run a slot once even when the first run already
// completed. MaxAttempts is 1: a failed or timed out cron guest is NOT retried,
// because its side effects (mail, http POST) may already have happened. Cron
// guests must still be idempotent (a crashed worker re-delivers the job).
// Without a job queue the guest runs inline on its own goroutine (never on the
// shared cron goroutine) and overlapping runs of the same schedule are skipped.
func (h *Host) cronTick(mod, expr string) {
	slot := time.Now().UTC().Truncate(time.Minute).Format("200601021504")
	_, err := kernel.Jobs(h.app).Enqueue(context.Background(), jobKindCron, jobPayload{Module: mod, Expr: expr},
		kernel.CronKey("wasm.cron:"+mod+":"+expr+":"+slot), kernel.MaxAttempts(1))
	if errors.Is(err, kernel.ErrNoJobQueue) {
		key := mod + "\x00" + expr
		if _, running := h.inlineCron.LoadOrStore(key, struct{}{}); running {
			h.app.Logger().Warn("wasm: previous inline cron run still active, tick skipped", "module", mod, "expr", expr)
			return
		}
		go func() {
			defer h.inlineCron.Delete(key)
			_ = h.cronJob(context.Background(), h.app, &kernel.Job{Payload: mustJSON(jobPayload{Module: mod, Expr: expr})})
		}()
		return
	}
	if err != nil {
		h.app.Logger().Error("wasm: cannot enqueue cron job", "module", mod, "error", err)
	}
}

func mustJSON(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

func (h *Host) cronJob(ctx context.Context, _ kernel.App, job *kernel.Job) error {
	var p jobPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return err
	}
	m := h.Module(p.Module)
	if m == nil {
		return fmt.Errorf("wasm module %q not loaded", p.Module)
	}
	ev := &EventIn{Event: "cron:" + p.Expr, Kind: "cron", Actor: Actor{Kind: "system"}}
	ev.Cron = &struct {
		Expr string `json:"expr"`
	}{Expr: p.Expr}
	return h.runSimple(ctx, m, ev)
}

func (h *Host) jobJob(ctx context.Context, _ kernel.App, job *kernel.Job) error {
	var p jobPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return err
	}
	m := h.Module(p.Module)
	if m == nil {
		return fmt.Errorf("wasm module %q not loaded", p.Module)
	}
	ev := &EventIn{Event: "job:" + p.Name, Kind: "job", Actor: Actor{Kind: "system"}}
	ev.Job = &struct {
		Name    string          `json:"name"`
		Payload json.RawMessage `json:"payload"`
	}{Name: p.Name, Payload: p.Payload}
	return h.runSimple(ctx, m, ev)
}

func (h *Host) runSimple(ctx context.Context, m *Module, ev *EventIn) error {
	res, err := h.Invoke(ctx, m, ev, CallOpts{})
	if err != nil {
		return err
	}
	if !res.OK {
		return fmt.Errorf("guest reported failure: %s", res.Message)
	}
	return nil
}

// ---- routes ----

var pathParamRe = regexp.MustCompile(`\{([^}.]+)(\.\.\.)?\}`)

func (h *Host) bindRoutes(r *router.Router[*core.RequestEvent]) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.served = true
	for _, m := range h.reg.Load().mods {
		for _, e := range m.Parsed {
			if e.Kind != KindRoute {
				continue
			}
			k := e.Method + " " + e.Path
			if h.routes[k] {
				continue
			}
			if r.HasRoute(e.Method, e.Path) {
				h.app.Logger().Warn("wasm: route already exists, skipped", "route", k, "module", m.Name)
				continue
			}
			h.routes[k] = true
			method, path := e.Method, e.Path
			h.registerRoute(r, m.Name, method, path)
		}
	}
}

// registerRoute binds one route; a ServeMux pattern conflict panics, which must
// not crash the server because of a sidecar file.
func (h *Host) registerRoute(r *router.Router[*core.RequestEvent], mod, method, path string) {
	defer func() {
		if rec := recover(); rec != nil {
			h.app.Logger().Error("wasm: route could not be registered", "module", mod, "route", method+" "+path, "panic", fmt.Sprint(rec))
		}
	}()
	r.Route(method, path, func(re *core.RequestEvent) error { return h.serveRoute(method, path, re) })
}

func (h *Host) findRoute(method, path string) *Module {
	for _, m := range h.reg.Load().mods {
		for _, e := range m.Parsed {
			if e.Kind == KindRoute && e.Method == method && e.Path == path {
				return m
			}
		}
	}
	return nil
}

func (h *Host) serveRoute(method, path string, re *core.RequestEvent) error {
	m := h.findRoute(method, path)
	if m == nil {
		return router.NewNotFoundError("", nil)
	}
	body, err := io.ReadAll(io.LimitReader(re.Request.Body, MaxRouteBodyBytes+1))
	if err != nil {
		return router.NewBadRequestError("Failed to read the request body.", nil)
	}
	if len(body) > MaxRouteBodyBytes {
		return router.NewApiError(http.StatusRequestEntityTooLarge, "Request body too large.", nil)
	}
	hdr := map[string]string{}
	hsize := 0
	for k := range re.Request.Header {
		v := re.Request.Header.Get(k)
		if hsize += len(k) + len(v); hsize > MaxRouteHeaderBytes {
			return router.NewApiError(http.StatusRequestHeaderFieldsTooLarge, "Request headers too large.", nil)
		}
		hdr[strings.ToLower(k)] = v
	}
	q := map[string]string{}
	for k, v := range re.Request.URL.Query() {
		if len(v) > 0 {
			q[k] = v[0]
		}
	}
	params := map[string]string{}
	for _, mt := range pathParamRe.FindAllStringSubmatch(path, -1) {
		params[mt[1]] = re.Request.PathValue(mt[1])
	}
	ev := &EventIn{
		Event: "route:" + method + " " + path, Kind: "route", Actor: actorFromAuth(re.Auth),
		Route: &RouteIn{Method: re.Request.Method, Path: re.Request.URL.Path, PathParams: params, Query: q, Headers: hdr, Body: string(body)},
	}
	res, err := h.Invoke(re.Request.Context(), m, ev, CallOpts{})
	if err != nil {
		return router.NewApiError(http.StatusInternalServerError, "Hook failed.", nil)
	}
	if !res.OK {
		status := res.Status
		if status < 400 || status > 599 {
			status = http.StatusBadRequest
		}
		msg := res.Message
		if msg == "" {
			msg = "Request rejected."
		}
		return router.NewApiError(status, msg, safeData(res.Data))
	}
	status := res.Status
	if status < 100 || status > 599 {
		status = http.StatusOK
	}
	for k, v := range res.Headers {
		re.Response.Header().Set(k, v)
	}
	var out []byte
	if len(res.Body) > 0 && string(res.Body) != "null" {
		var s string
		if json.Unmarshal(res.Body, &s) == nil {
			out = []byte(s)
		} else {
			out = res.Body
			if re.Response.Header().Get("Content-Type") == "" {
				re.Response.Header().Set("Content-Type", "application/json")
			}
		}
	}
	re.Response.WriteHeader(status)
	_, err = re.Response.Write(out)
	return err
}

// safeData converts the guest's `data` into the public error shape of the
// API: {"field":{"code":"...","message":"..."}}. A string value becomes the
// message with code "validation_hook_rejected".
func safeData(in map[string]any) any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]validation.Error, len(in))
	for k, v := range in {
		switch x := v.(type) {
		case string:
			out[k] = validation.NewError("validation_hook_rejected", x)
		case map[string]any:
			code, _ := x["code"].(string)
			msg, _ := x["message"].(string)
			if code == "" {
				code = "validation_hook_rejected"
			}
			if msg == "" {
				msg = "Rejected by hook."
			}
			out[k] = validation.NewError(code, msg)
		default:
			out[k] = validation.NewError("validation_hook_rejected", "Rejected by hook.")
		}
	}
	return out
}
