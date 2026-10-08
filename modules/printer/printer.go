//go:build !no_printer

package printer

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/hook"
)

const (
	hookId       = "__tokiPrinter__"
	hookPriority = 1 << 20
)

var (
	sinkMu     sync.Mutex
	globalSink func(action, collection, record string, details map[string]any)
)

// SetAuditSink connects print.job and print.dead events to an external audit
// log. Modules must not import each other, so the wiring is in tokibase.go.
func SetAuditSink(fn func(action, collection, record string, details map[string]any)) {
	sinkMu.Lock()
	globalSink = fn
	sinkMu.Unlock()
}

func audit(action, record string, details map[string]any) {
	sinkMu.Lock()
	fn := globalSink
	sinkMu.Unlock()
	if fn != nil {
		fn(action, JobsCollection, record, details)
	}
}

// PrinterStatus is the last known state of a printer.
type PrinterStatus struct {
	State  string    `json:"state"` // unknown|ok|paper|offline|error
	Detail string    `json:"detail,omitempty"`
	At     time.Time `json:"at,omitzero"`
}

// Module is the registered printer module.
type Module struct {
	app core.App

	// tunables (tests override them)
	WaitDelay time.Duration
	Now       func() time.Time
	// Open opens the transport of a printer (default: devio).
	Open func(ctx context.Context, p *Printer) (io.ReadWriteCloser, error)

	mu     sync.Mutex
	locks  map[string]*sync.Mutex
	status map[string]PrinterStatus
}

// New creates a module without binding any hook (CLI and tests).
func New(app core.App) *Module {
	m := &Module{app: app, WaitDelay: DefaultWaitDelay, Now: time.Now,
		locks: map[string]*sync.Mutex{}, status: map[string]PrinterStatus{}}
	m.Open = m.openTransport
	return m
}

// Register creates the collections on bootstrap, binds the validation and
// guard hooks, the routes, the prune cron, the health block and the
// `print.send` job handler.
func Register(app core.App) *Module {
	m := New(app)

	init := func() {
		if err := ensureCollections(app); err != nil {
			app.Logger().Error("printer: failed to initialize the collections", "error", err)
		}
	}
	if app.IsBootstrapped() {
		init()
	}
	app.OnBootstrap().Bind(&hook.Handler[*core.BootstrapEvent]{
		Id: hookId, Priority: hookPriority,
		Func: func(e *core.BootstrapEvent) error {
			if err := e.Next(); err != nil {
				return err
			}
			init()
			return nil
		},
	})

	app.OnRecordValidate(PrintersCollection).Bind(&hook.Handler[*core.RecordEvent]{
		Id: hookId + "val",
		Func: func(e *core.RecordEvent) error {
			if err := validatePrinter(printerOf(e.Record)); err != nil {
				return err
			}
			return e.Next()
		},
	})
	// the version counts the saves of a template; a reprint never re-renders
	app.OnRecordCreate(TemplatesCollection).Bind(&hook.Handler[*core.RecordEvent]{
		Id: hookId + "tplc",
		Func: func(e *core.RecordEvent) error {
			e.Record.Set("version", 1)
			return e.Next()
		},
	})
	app.OnRecordUpdate(TemplatesCollection).Bind(&hook.Handler[*core.RecordEvent]{
		Id: hookId + "tplu",
		Func: func(e *core.RecordEvent) error {
			prev := 0
			if old, err := e.App.FindRecordById(TemplatesCollection, e.Record.Id); err == nil {
				prev = old.GetInt("version")
			}
			e.Record.Set("version", prev+1)
			return e.Next()
		},
	})
	// a hub row applied on a spoke must never become a print
	app.OnRecordCreate(JobsCollection).Bind(&hook.Handler[*core.RecordEvent]{
		Id: hookId + "guard", Priority: -1000,
		Func: func(e *core.RecordEvent) error {
			if kernel.IsSyncReplica(e.Context) {
				return ErrSyncReplica
			}
			return e.Next()
		},
	})

	kernel.Jobs(app).Register(JobKind, m.handle)

	app.OnServe().Bind(&hook.Handler[*core.ServeEvent]{
		Id: hookId + "routes", Priority: hookPriority,
		Func: func(e *core.ServeEvent) error {
			m.bindRoutes(e)
			if err := app.Cron().Add("__tokiPrint_prune", "37 3 * * *", func() {
				if _, err := Prune(app, time.Now(), time.Duration(RetentionDays())*24*time.Hour); err != nil {
					app.Logger().Warn("printer: prune failed", "error", err)
				}
			}); err != nil {
				app.Logger().Error("printer: failed to schedule the prune", "error", err)
			}
			return e.Next()
		},
	})

	apis.SetHealthExtra(app, "printer", func(core.App) any { return m.Health() })
	return m
}

func (m *Module) lockFor(name string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.locks[name]
	if l == nil {
		l = &sync.Mutex{}
		m.locks[name] = l
	}
	return l
}

func (m *Module) setStatus(name, state, detail string) {
	m.mu.Lock()
	m.status[name] = PrinterStatus{State: state, Detail: detail, At: m.Now()}
	m.mu.Unlock()
}

// LastStatus returns the last known status of a printer.
func (m *Module) LastStatus(name string) PrinterStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.status[name]; ok {
		return s
	}
	return PrinterStatus{State: "unknown"}
}

// Health is the `printer` block of GET /api/health (superuser only).
func (m *Module) Health() any {
	list, err := ListPrinters(m.app)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	type row struct {
		Name      string        `json:"name"`
		Transport string        `json:"transport"`
		Enabled   bool          `json:"enabled"`
		Status    PrinterStatus `json:"status"`
	}
	rows := make([]row, 0, len(list))
	for _, p := range list {
		rows = append(rows, row{p.Name, p.Transport, p.Enabled, m.LastStatus(p.Name)})
	}
	return map[string]any{"printers": rows, "queue": queueCounts(m.app)}
}
