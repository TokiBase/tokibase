// Package jobs implements a durable job queue on the auxiliary database:
// retry with exponential backoff, dead-letter, cron scheduling and a worker
// role. Consumers use the kernel.JobQueue interface (kernel.Jobs(app)).
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/hook"
	"github.com/tokibase/tokibase/tools/security"
)

const (
	hookId       = "__tokiJobs__"
	hookPriority = -1 << 20
	TableName    = "_jobs"
	timeLayout   = "2006-01-02 15:04:05.000Z"

	StateQueued  = "queued"
	StateRunning = "running"
	StateDone    = "done"
	StateFailed  = "failed" // failed, waiting for the next retry
	StateDead    = "dead"

	DefaultMaxAttempts = 10
	DefaultWorkers     = 4
	BackoffBase        = 5 * time.Second
	BackoffCap         = time.Hour
	StaleLockAfter     = 10 * time.Minute
	ShutdownGrace      = 30 * time.Second

	// AuditAction is the action name passed to the audit sink on dead-letter.
	AuditAction = "jobs.dead"
)

const createTableSQL = `CREATE TABLE IF NOT EXISTS {{_jobs}} (
	[[id]]           TEXT PRIMARY KEY NOT NULL,
	[[kind]]         TEXT NOT NULL,
	[[payload]]      TEXT NOT NULL DEFAULT 'null',
	[[state]]        TEXT NOT NULL DEFAULT 'queued',
	[[attempt]]      INTEGER NOT NULL DEFAULT 0,
	[[max_attempts]] INTEGER NOT NULL DEFAULT 10,
	[[run_at]]       TEXT NOT NULL,
	[[locked_by]]    TEXT NOT NULL DEFAULT '',
	[[locked_at]]    TEXT NOT NULL DEFAULT '',
	[[last_error]]   TEXT NOT NULL DEFAULT '',
	[[created]]      TEXT NOT NULL,
	[[updated]]      TEXT NOT NULL,
	[[unique_key]]   TEXT
);
CREATE INDEX IF NOT EXISTS {{idx__jobs_poll}} ON {{_jobs}} ([[state]], [[run_at]]);
CREATE UNIQUE INDEX IF NOT EXISTS {{idx__jobs_unique}} ON {{_jobs}} ([[unique_key]])
	WHERE [[unique_key]] IS NOT NULL AND [[state]] IN ('queued','running','failed');`

// Enabled reports whether the module is on (env TOKI_JOBS=off disables it).
func Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TOKI_JOBS"))) {
	case "off", "false", "0", "disabled":
		return false
	}
	return true
}

// Workers returns the in-process worker count (env TOKI_JOBS_WORKERS, default 4, 0 disables).
func Workers() int {
	if s := strings.TrimSpace(os.Getenv("TOKI_JOBS_WORKERS")); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n >= 0 {
			return n
		}
	}
	return DefaultWorkers
}

// WorkerRole reports whether the process runs in the worker role
// (env TOKI_ROLE=worker, or `serve --role worker`).
func WorkerRole() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("TOKI_ROLE")), "worker")
}

var (
	sinkMu     sync.RWMutex
	globalSink func(action, collection, record string, details map[string]any)
)

// SetAuditSink connects dead-letter transitions to an external audit log.
// Modules must not import each other, so the wiring happens in tokibase.go.
func SetAuditSink(fn func(action, collection, record string, details map[string]any)) {
	sinkMu.Lock()
	globalSink = fn
	sinkMu.Unlock()
}

// Module is the queue implementation (it satisfies kernel.JobQueue).
type Module struct {
	app core.App

	// tunables (tests override them)
	Now         func() time.Time
	Jitter      func() float64 // in [0,1); the backoff is scaled by 0.8+0.4*j
	PollEvery   time.Duration
	MaxAttempts int
	nodeID      string

	mu       sync.RWMutex
	handlers map[string]kernel.JobHandler

	wake chan struct{}

	runMu   sync.Mutex
	started bool
	stop    chan struct{}
	runCtx  context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

var _ kernel.JobQueue = (*Module)(nil)

// New creates a module (no hooks bound, no workers started).
func New(app core.App) *Module {
	host, _ := os.Hostname()
	return &Module{
		app:         app,
		Now:         time.Now,
		Jitter:      rand.Float64,
		PollEvery:   time.Second,
		MaxAttempts: DefaultMaxAttempts,
		nodeID:      fmt.Sprintf("%s:%d:%s", host, os.Getpid(), security.RandomString(4)),
		handlers:    map[string]kernel.JobHandler{},
		wake:        make(chan struct{}, 1),
	}
}

// Register creates the module, initializes the table, registers it as the
// app job queue (kernel.SetJobs), binds the serve/terminate hooks and the
// built-in `jobs.echo` handler.
func Register(app core.App) *Module {
	m := New(app)
	m.Handle("jobs.echo", EchoHandler)
	kernel.SetJobs(app, m)

	init := func() {
		if _, err := app.AuxDB().NewQuery(createTableSQL).Execute(); err != nil {
			app.Logger().Error("jobs: failed to initialize the _jobs table", "error", err)
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

	app.OnServe().Bind(&hook.Handler[*core.ServeEvent]{
		Id: hookId, Priority: hookPriority,
		Func: func(e *core.ServeEvent) error {
			if WorkerRole() {
				e.Router.Bind(workerRoleMiddleware())
				if Workers() == 0 {
					app.Logger().Warn("jobs: TOKI_ROLE=worker with TOKI_JOBS_WORKERS=0 processes nothing")
				}
			}
			m.Start(Workers())
			return e.Next()
		},
	})
	app.OnTerminate().Bind(&hook.Handler[*core.TerminateEvent]{
		Id: hookId, Priority: hookPriority,
		Func: func(e *core.TerminateEvent) error {
			m.Stop(ShutdownGrace)
			return e.Next()
		},
	})
	return m
}

// workerRoleMiddleware rejects everything except the health endpoint.
func workerRoleMiddleware() *hook.Handler[*core.RequestEvent] {
	return &hook.Handler[*core.RequestEvent]{
		Id: hookId + "role", Priority: hookPriority,
		Func: func(e *core.RequestEvent) error {
			if e.Request.URL.Path == "/api/health" {
				return e.Next()
			}
			return e.Error(http.StatusServiceUnavailable, "This node runs in the worker role.", nil)
		},
	}
}

// EchoHandler is the example handler of kind "jobs.echo": it logs the payload.
// A payload of {"fail":true} makes it fail (useful for trying retries).
func EchoHandler(ctx context.Context, app kernel.App, job *kernel.Job) error {
	var p struct {
		Fail bool `json:"fail"`
	}
	_ = json.Unmarshal(job.Payload, &p)
	app.Logger().Info("jobs.echo", "id", job.ID, "attempt", job.Attempt, "payload", string(job.Payload))
	if p.Fail {
		return errors.New("jobs.echo: requested failure")
	}
	return nil
}

// Handle registers a handler (same as Register on the queue interface).
func (m *Module) Handle(kind string, h kernel.JobHandler) { m.Register(kind, h) }

// Register implements kernel.JobQueue.
func (m *Module) Register(kind string, h kernel.JobHandler) {
	m.mu.Lock()
	m.handlers[kind] = h
	m.mu.Unlock()
}

func (m *Module) handler(kind string) kernel.JobHandler {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.handlers[kind]
}

func fmtTime(t time.Time) string { return t.UTC().Format(timeLayout) }

// ParseTime parses a stored timestamp (zero time on failure).
func ParseTime(s string) time.Time {
	t, err := time.Parse(timeLayout, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// Backoff returns the delay before the retry that follows the given failed
// attempt (1-based): base 5s, factor 2, cap 1h, scaled by jitter 0.8..1.2.
func (m *Module) Backoff(attempt int) time.Duration {
	d := BackoffBase
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= BackoffCap || d <= 0 {
			d = BackoffCap
			break
		}
	}
	if d > BackoffCap {
		d = BackoffCap
	}
	if m.Jitter != nil {
		d = time.Duration(float64(d) * (0.8 + 0.4*m.Jitter()))
	}
	return d
}

// Enqueue implements kernel.JobQueue.
func (m *Module) Enqueue(ctx context.Context, kind string, payload any, opts ...kernel.EnqueueOption) (string, error) {
	if strings.TrimSpace(kind) == "" {
		return "", errors.New("jobs: empty kind")
	}
	o := kernel.ResolveEnqueueOptions(opts...)
	var raw []byte
	switch p := payload.(type) {
	case nil:
		raw = []byte("null")
	case json.RawMessage:
		raw = p
	case []byte:
		raw = p
	default:
		var err error
		if raw, err = json.Marshal(payload); err != nil {
			return "", fmt.Errorf("jobs: payload: %w", err)
		}
	}
	if !json.Valid(raw) {
		return "", errors.New("jobs: payload is not valid JSON")
	}
	max := o.MaxAttempts
	if max <= 0 {
		max = m.MaxAttempts
	}
	now := m.Now()
	id := strings.ToLower(security.RandomString(15))
	var uniq any
	if o.UniqueKey != "" {
		uniq = o.UniqueKey
	}
	_, err := m.app.AuxDB().NewQuery(`INSERT INTO {{_jobs}}
		([[id]],[[kind]],[[payload]],[[state]],[[attempt]],[[max_attempts]],[[run_at]],[[created]],[[updated]],[[unique_key]])
		VALUES ({:id},{:kind},{:payload},'queued',0,{:max},{:run},{:now},{:now},{:uniq})`).
		WithContext(ctx).
		Bind(dbx.Params{"id": id, "kind": kind, "payload": string(raw), "max": max,
			"run": fmtTime(now.Add(o.Delay)), "now": fmtTime(now), "uniq": uniq}).Execute()
	if err != nil {
		if o.UniqueKey != "" {
			var existing string
			qerr := m.app.AuxDB().NewQuery(`SELECT [[id]] FROM {{_jobs}} WHERE [[unique_key]]={:k} AND [[state]] IN ('queued','running','failed') LIMIT 1`).
				WithContext(ctx).Bind(dbx.Params{"k": o.UniqueKey}).Row(&existing)
			if qerr == nil && existing != "" {
				return existing, nil
			}
		}
		return "", err
	}
	select {
	case m.wake <- struct{}{}:
	default:
	}
	return id, nil
}

// Stats implements kernel.JobQueue.
func (m *Module) Stats(ctx context.Context) (kernel.JobStats, error) {
	var rows []struct {
		State string `db:"state"`
		N     int64  `db:"n"`
	}
	if err := m.app.AuxDB().NewQuery(`SELECT [[state]], COUNT(*) AS [[n]] FROM {{_jobs}} GROUP BY [[state]]`).
		WithContext(ctx).All(&rows); err != nil {
		return kernel.JobStats{}, err
	}
	var s kernel.JobStats
	for _, r := range rows {
		switch r.State {
		case StateQueued:
			s.Queued = r.N
		case StateRunning:
			s.Running = r.N
		case StateDone:
			s.Done = r.N
		case StateFailed:
			s.Failed = r.N
		case StateDead:
			s.Dead = r.N
		}
	}
	return s, nil
}

type jobRow struct {
	ID          string  `db:"id" json:"id"`
	Kind        string  `db:"kind" json:"kind"`
	Payload     string  `db:"payload" json:"payload"`
	State       string  `db:"state" json:"state"`
	Attempt     int     `db:"attempt" json:"attempt"`
	MaxAttempts int     `db:"max_attempts" json:"maxAttempts"`
	RunAt       string  `db:"run_at" json:"runAt"`
	LockedBy    string  `db:"locked_by" json:"lockedBy,omitempty"`
	LockedAt    string  `db:"locked_at" json:"lockedAt,omitempty"`
	LastError   string  `db:"last_error" json:"lastError,omitempty"`
	Created     string  `db:"created" json:"created"`
	Updated     string  `db:"updated" json:"updated"`
	UniqueKey   *string `db:"unique_key" json:"uniqueKey,omitempty"`
}

// Row is a stored job (CLI/JSON view).
type Row = jobRow

// ReclaimStale puts jobs whose lock is older than StaleLockAfter back in the
// queue (or dead-letters them when they used all their attempts). The attempt
// is counted at claim time, so a job that crashes its worker cannot loop forever.
func (m *Module) ReclaimStale(ctx context.Context) (int, error) {
	now := m.Now()
	cutoff := fmtTime(now.Add(-StaleLockAfter))
	var rows []jobRow
	if err := m.app.AuxDB().NewQuery(`SELECT * FROM {{_jobs}} WHERE [[state]]='running' AND [[locked_at]]<{:c}`).
		WithContext(ctx).Bind(dbx.Params{"c": cutoff}).All(&rows); err != nil {
		return 0, err
	}
	n := 0
	for _, r := range rows {
		state, run := StateFailed, fmtTime(now)
		if r.Attempt >= r.MaxAttempts {
			state = StateDead
		}
		res, err := m.app.AuxDB().NewQuery(`UPDATE {{_jobs}} SET [[state]]={:s}, [[run_at]]={:r}, [[locked_by]]='', [[locked_at]]='',
			[[last_error]]='lock expired (worker presumed dead)', [[updated]]={:u}
			WHERE [[id]]={:id} AND [[state]]='running' AND [[locked_at]]={:la}`).WithContext(ctx).
			Bind(dbx.Params{"s": state, "r": run, "u": fmtTime(now), "id": r.ID, "la": r.LockedAt}).Execute()
		if err != nil {
			return n, err
		}
		if c, _ := res.RowsAffected(); c == 1 {
			n++
			if state == StateDead {
				m.announceDead(r, "lock expired (worker presumed dead)")
			}
		}
	}
	return n, nil
}

// claim atomically claims one runnable job (nil when none).
func (m *Module) claim(ctx context.Context) (*kernel.Job, error) {
	now := m.Now()
	var ids []string
	if err := m.app.AuxDB().NewQuery(`SELECT [[id]] FROM {{_jobs}} WHERE [[state]] IN ('queued','failed') AND [[run_at]]<={:now}
		ORDER BY [[run_at]] LIMIT 8`).WithContext(ctx).Bind(dbx.Params{"now": fmtTime(now)}).Column(&ids); err != nil {
		return nil, err
	}
	for _, id := range ids {
		res, err := m.app.AuxDB().NewQuery(`UPDATE {{_jobs}} SET [[state]]='running', [[attempt]]=[[attempt]]+1,
			[[locked_by]]={:by}, [[locked_at]]={:now}, [[updated]]={:now}
			WHERE [[id]]={:id} AND [[state]] IN ('queued','failed')`).WithContext(ctx).
			Bind(dbx.Params{"by": m.nodeID, "now": fmtTime(now), "id": id}).Execute()
		if err != nil {
			return nil, err
		}
		if c, _ := res.RowsAffected(); c != 1 {
			continue // another worker won the race
		}
		var r jobRow
		if err := m.app.AuxDB().NewQuery(`SELECT * FROM {{_jobs}} WHERE [[id]]={:id}`).WithContext(ctx).
			Bind(dbx.Params{"id": id}).One(&r); err != nil {
			return nil, err
		}
		return &kernel.Job{
			ID: r.ID, Kind: r.Kind, Payload: json.RawMessage(r.Payload), Attempt: r.Attempt,
			MaxAttempts: r.MaxAttempts, RunAt: ParseTime(r.RunAt), CreatedAt: ParseTime(r.Created),
		}, nil
	}
	return nil, nil
}

// ProcessOnce reclaims stale locks, claims at most one due job and runs it.
// It reports whether a job was processed.
func (m *Module) ProcessOnce(ctx context.Context) (bool, error) {
	if _, err := m.ReclaimStale(ctx); err != nil {
		return false, err
	}
	job, err := m.claim(ctx)
	if err != nil || job == nil {
		return false, err
	}
	m.run(ctx, job)
	return true, nil
}

func (m *Module) run(ctx context.Context, job *kernel.Job) {
	var herr error
	if h := m.handler(job.Kind); h == nil {
		herr = fmt.Errorf("no handler registered for kind %q", job.Kind)
	} else {
		herr = safeCall(ctx, h, m.app, job)
	}
	now := m.Now()
	if herr == nil {
		m.exec(`UPDATE {{_jobs}} SET [[state]]='done', [[locked_by]]='', [[locked_at]]='', [[last_error]]='', [[updated]]={:u} WHERE [[id]]={:id}`,
			dbx.Params{"u": fmtTime(now), "id": job.ID})
		return
	}
	msg := herr.Error()
	if len(msg) > 2000 {
		msg = msg[:2000]
	}
	if job.Attempt >= job.MaxAttempts {
		m.exec(`UPDATE {{_jobs}} SET [[state]]='dead', [[locked_by]]='', [[locked_at]]='', [[last_error]]={:e}, [[updated]]={:u} WHERE [[id]]={:id}`,
			dbx.Params{"e": msg, "u": fmtTime(now), "id": job.ID})
		m.announceDead(jobRow{ID: job.ID, Kind: job.Kind, Attempt: job.Attempt, MaxAttempts: job.MaxAttempts}, msg)
		return
	}
	m.exec(`UPDATE {{_jobs}} SET [[state]]='failed', [[run_at]]={:r}, [[locked_by]]='', [[locked_at]]='', [[last_error]]={:e}, [[updated]]={:u} WHERE [[id]]={:id}`,
		dbx.Params{"r": fmtTime(now.Add(m.Backoff(job.Attempt))), "e": msg, "u": fmtTime(now), "id": job.ID})
}

func (m *Module) exec(sql string, p dbx.Params) {
	if _, err := m.app.AuxDB().NewQuery(sql).Bind(p).Execute(); err != nil {
		m.app.Logger().Error("jobs: failed to update job state", "error", err)
	}
}

func safeCall(ctx context.Context, h kernel.JobHandler, app kernel.App, job *kernel.Job) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return h(ctx, app, job)
}

func (m *Module) announceDead(r jobRow, lastErr string) {
	m.app.Logger().Warn("jobs: job dead-lettered", "id", r.ID, "kind", r.Kind, "attempts", r.Attempt, "error", lastErr)
	sinkMu.RLock()
	fn := globalSink
	sinkMu.RUnlock()
	if fn != nil {
		fn(AuditAction, TableName, r.ID, map[string]any{
			"kind": r.Kind, "attempts": r.Attempt, "maxAttempts": r.MaxAttempts, "error": lastErr,
		})
	}
}

// Start launches n polling workers (no-op when n <= 0 or already started).
func (m *Module) Start(n int) {
	m.runMu.Lock()
	defer m.runMu.Unlock()
	if n <= 0 || m.started {
		return
	}
	m.started = true
	m.stop = make(chan struct{})
	m.runCtx, m.cancel = context.WithCancel(context.Background())
	for i := 0; i < n; i++ {
		m.wg.Add(1)
		go m.loop()
	}
}

func (m *Module) loop() {
	defer m.wg.Done()
	for {
		select {
		case <-m.stop:
			return
		default:
		}
		worked, err := m.ProcessOnce(m.runCtx)
		if err != nil && m.runCtx.Err() == nil {
			m.app.Logger().Error("jobs: worker error", "error", err)
		}
		if worked {
			continue
		}
		t := time.NewTimer(m.PollEvery)
		select {
		case <-m.stop:
			t.Stop()
			return
		case <-m.wake:
		case <-t.C:
		}
		t.Stop()
	}
}

// Stop stops claiming new jobs and waits up to grace for running ones; the
// job context is canceled when the grace period expires.
func (m *Module) Stop(grace time.Duration) {
	m.runMu.Lock()
	if !m.started {
		m.runMu.Unlock()
		return
	}
	m.started = false
	close(m.stop)
	m.runMu.Unlock()

	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(grace):
		m.app.Logger().Warn("jobs: shutdown grace expired, canceling running jobs")
		m.cancel()
		<-done
	}
	m.cancel()
}

// Cron schedules kind/payload on a cron expression: every tick enqueues a job
// (persisted, retried) instead of running inline. The job is deduplicated per
// schedule slot, so several processes sharing the DB enqueue it once.
func Cron(app core.App, name, expr, kind string, payload any) error {
	return app.Cron().Add("__tokiJobs_"+name, expr, func() {
		slot := time.Now().UTC().Truncate(time.Minute).Format("200601021504")
		if _, err := kernel.Jobs(app).Enqueue(context.Background(), kind, payload,
			kernel.Unique("cron:"+name+":"+slot)); err != nil {
			app.Logger().Error("jobs: cron enqueue failed", "name", name, "error", err)
		}
	})
}

// --- admin helpers (CLI) -----------------------------------------------

// List returns jobs newest first, optionally filtered by state.
func List(app core.App, state string, limit int) ([]Row, error) {
	if !app.AuxHasTable(TableName) {
		return nil, nil
	}
	q := app.AuxDB().Select("*").From(TableName).OrderBy("created DESC", "id DESC")
	if state != "" {
		q.AndWhere(dbx.HashExp{"state": state})
	}
	if limit > 0 {
		q.Limit(int64(limit))
	}
	var out []Row
	return out, q.All(&out)
}

// Retry requeues one job (id) or all dead jobs (id == "") with a fresh attempt
// counter. Running jobs are never touched. It returns the number requeued.
func Retry(app core.App, id string, now time.Time) (int64, error) {
	sql := `UPDATE {{_jobs}} SET [[state]]='queued', [[attempt]]=0, [[run_at]]={:n}, [[locked_by]]='', [[locked_at]]='', [[updated]]={:n}
		WHERE [[state]] IN ('dead','failed','done')`
	p := dbx.Params{"n": fmtTime(now)}
	if id == "" {
		sql = strings.Replace(sql, "IN ('dead','failed','done')", "='dead'", 1)
	} else {
		sql += " AND [[id]]={:id}"
		p["id"] = id
	}
	res, err := app.AuxDB().NewQuery(sql).Bind(p).Execute()
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PurgeDone deletes done jobs last updated before the cutoff.
func PurgeDone(app core.App, before time.Time) (int64, error) {
	res, err := app.AuxDB().NewQuery(`DELETE FROM {{_jobs}} WHERE [[state]]='done' AND [[updated]]<{:b}`).
		Bind(dbx.Params{"b": fmtTime(before)}).Execute()
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
