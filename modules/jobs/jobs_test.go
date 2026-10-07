package jobs

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tests"
)

type env struct {
	app *tests.TestApp
	m   *Module
	now time.Time
}

func setup(t *testing.T) *env {
	t.Helper()
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)
	e := &env{app: app, now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	e.m = Register(app)
	e.m.Now = func() time.Time { return e.now }
	e.m.Jitter = func() float64 { return 0.5 } // factor 1.0
	return e
}

// waitRow polls until the row reaches state or the deadline passes, then returns the last row seen.
func (e *env) waitRow(t *testing.T, id, state string, d time.Duration) Row {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		r := e.row(t, id)
		if r.State == state || time.Now().After(deadline) {
			return r
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func (e *env) row(t *testing.T, id string) Row {
	t.Helper()
	rows, err := List(e.app, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("job %s not found", id)
	return Row{}
}

func TestEnqueueAndProcess(t *testing.T) {
	e := setup(t)
	var got atomic.Value
	kernel.Jobs(e.app).Register("t.ok", func(ctx context.Context, app kernel.App, j *kernel.Job) error {
		got.Store(string(j.Payload))
		if j.Attempt != 1 {
			t.Errorf("attempt = %d", j.Attempt)
		}
		return nil
	})
	id, err := kernel.Jobs(e.app).Enqueue(context.Background(), "t.ok", map[string]int{"a": 1})
	if err != nil {
		t.Fatal(err)
	}
	ok, err := e.m.ProcessOnce(context.Background())
	if err != nil || !ok {
		t.Fatalf("ProcessOnce = %v, %v", ok, err)
	}
	if got.Load() != `{"a":1}` {
		t.Fatalf("payload = %v", got.Load())
	}
	if r := e.row(t, id); r.State != StateDone || r.Attempt != 1 {
		t.Fatalf("row = %+v", r)
	}
	if ok, _ := e.m.ProcessOnce(context.Background()); ok {
		t.Fatal("nothing left to process")
	}
}

func TestDelayNotDueYet(t *testing.T) {
	e := setup(t)
	e.m.Register("t.ok", func(context.Context, kernel.App, *kernel.Job) error { return nil })
	if _, err := e.m.Enqueue(context.Background(), "t.ok", nil, kernel.Delay(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if ok, _ := e.m.ProcessOnce(context.Background()); ok {
		t.Fatal("job must not be due")
	}
	e.now = e.now.Add(61 * time.Second)
	if ok, _ := e.m.ProcessOnce(context.Background()); !ok {
		t.Fatal("job must be due")
	}
}

func TestRetryBackoffAndDeadLetter(t *testing.T) {
	e := setup(t)
	var dead []map[string]any
	SetAuditSink(func(action, collection, record string, d map[string]any) {
		if action != AuditAction || collection != TableName {
			t.Errorf("sink %s %s", action, collection)
		}
		dead = append(dead, d)
	})
	t.Cleanup(func() { SetAuditSink(nil) })

	e.m.Register("t.fail", func(context.Context, kernel.App, *kernel.Job) error { return errors.New("boom") })
	id, err := e.m.Enqueue(context.Background(), "t.fail", nil, kernel.MaxAttempts(3))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// attempt 1 fails, retry in 5s
	if ok, _ := e.m.ProcessOnce(ctx); !ok {
		t.Fatal("attempt 1")
	}
	r := e.row(t, id)
	if r.State != StateFailed || r.LastError != "boom" || ParseTime(r.RunAt) != e.now.Add(5*time.Second) {
		t.Fatalf("after 1: %+v", r)
	}
	// not due at +4s
	e.now = e.now.Add(4 * time.Second)
	if ok, _ := e.m.ProcessOnce(ctx); ok {
		t.Fatal("must wait for backoff")
	}
	e.now = e.now.Add(time.Second)
	if ok, _ := e.m.ProcessOnce(ctx); !ok {
		t.Fatal("attempt 2")
	}
	r = e.row(t, id)
	if r.State != StateFailed || ParseTime(r.RunAt) != e.now.Add(10*time.Second) {
		t.Fatalf("after 2: %+v", r)
	}
	e.now = e.now.Add(10 * time.Second)
	if ok, _ := e.m.ProcessOnce(ctx); !ok {
		t.Fatal("attempt 3")
	}
	r = e.row(t, id)
	if r.State != StateDead || r.Attempt != 3 {
		t.Fatalf("after 3: %+v", r)
	}
	if len(dead) != 1 || dead[0]["kind"] != "t.fail" {
		t.Fatalf("audit sink = %v", dead)
	}
	if s, _ := e.m.Stats(ctx); s.Dead != 1 {
		t.Fatalf("stats = %+v", s)
	}

	// retry --dead requeues with a fresh counter
	n, err := Retry(e.app, "", e.now)
	if err != nil || n != 1 {
		t.Fatalf("retry = %d, %v", n, err)
	}
	if r = e.row(t, id); r.State != StateQueued || r.Attempt != 0 {
		t.Fatalf("after retry: %+v", r)
	}
}

func TestBackoffCap(t *testing.T) {
	e := setup(t)
	if d := e.m.Backoff(1); d != 5*time.Second {
		t.Fatal(d)
	}
	if d := e.m.Backoff(30); d != time.Hour {
		t.Fatal(d)
	}
	e.m.Jitter = func() float64 { return 0 }
	if d := e.m.Backoff(1); d != 4*time.Second {
		t.Fatal(d)
	}
}

func TestPanicIsRetried(t *testing.T) {
	e := setup(t)
	e.m.Register("t.panic", func(context.Context, kernel.App, *kernel.Job) error { panic("oops") })
	id, _ := e.m.Enqueue(context.Background(), "t.panic", nil)
	e.m.ProcessOnce(context.Background())
	if r := e.row(t, id); r.State != StateFailed || r.LastError != "panic: oops" {
		t.Fatalf("%+v", r)
	}
}

func TestUniqueKeyDedup(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.m.Register("t.ok", func(context.Context, kernel.App, *kernel.Job) error { return nil })
	a, err := e.m.Enqueue(ctx, "t.ok", nil, kernel.Unique("k1"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.m.Enqueue(ctx, "t.ok", nil, kernel.Unique("k1"))
	if err != nil || a != b {
		t.Fatalf("dedup: %s %s %v", a, b, err)
	}
	if s, _ := e.m.Stats(ctx); s.Queued != 1 {
		t.Fatalf("stats = %+v", s)
	}
	// once done the key is released
	e.m.ProcessOnce(ctx)
	c, err := e.m.Enqueue(ctx, "t.ok", nil, kernel.Unique("k1"))
	if err != nil || c == a {
		t.Fatalf("re-enqueue: %s %v", c, err)
	}
}

func TestStaleLockReclaim(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	var runs int
	e.m.Register("t.ok", func(context.Context, kernel.App, *kernel.Job) error { runs++; return nil })
	id, _ := e.m.Enqueue(ctx, "t.ok", nil)
	// simulate a crashed worker: claim without running
	job, err := e.m.claim(ctx)
	if err != nil || job == nil {
		t.Fatal(job, err)
	}
	if n, _ := e.m.ReclaimStale(ctx); n != 0 {
		t.Fatal("lock is fresh")
	}
	e.now = e.now.Add(StaleLockAfter + time.Second)
	if ok, err := e.m.ProcessOnce(ctx); err != nil || !ok {
		t.Fatalf("ProcessOnce = %v %v", ok, err)
	}
	r := e.row(t, id)
	if r.State != StateDone || r.Attempt != 2 || runs != 1 {
		t.Fatalf("%+v runs=%d", r, runs)
	}
}

func TestStaleLockDeadLettersExhausted(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	id, _ := e.m.Enqueue(ctx, "t.x", nil, kernel.MaxAttempts(1))
	if j, _ := e.m.claim(ctx); j == nil {
		t.Fatal("claim")
	}
	e.now = e.now.Add(StaleLockAfter + time.Second)
	if n, _ := e.m.ReclaimStale(ctx); n != 1 {
		t.Fatal(n)
	}
	if r := e.row(t, id); r.State != StateDead {
		t.Fatalf("%+v", r)
	}
}

func TestCronEnqueues(t *testing.T) {
	e := setup(t)
	ran := make(chan struct{}, 1)
	e.m.Register("t.cron", func(context.Context, kernel.App, *kernel.Job) error { ran <- struct{}{}; return nil })
	if err := Cron(e.app, "every", "* * * * *", "t.cron", map[string]string{"x": "y"}); err != nil {
		t.Fatal(err)
	}
	// fire the registered job directly instead of waiting for the ticker
	for _, j := range e.app.Cron().Jobs() {
		if j.Id() == "__tokiJobs_every" {
			j.Run()
			j.Run() // same slot: deduplicated
		}
	}
	if s, _ := e.m.Stats(context.Background()); s.Queued != 1 {
		t.Fatalf("stats = %+v", s)
	}
	e.m.Now = time.Now
	if ok, _ := e.m.ProcessOnce(context.Background()); !ok {
		t.Fatal("cron job not processed")
	}
	select {
	case <-ran:
	default:
		t.Fatal("handler did not run")
	}
}

func TestStats(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.m.Register("t.ok", func(context.Context, kernel.App, *kernel.Job) error { return nil })
	e.m.Enqueue(ctx, "t.ok", nil)
	e.m.Enqueue(ctx, "t.ok", nil)
	e.m.Enqueue(ctx, "t.nohandler", nil, kernel.MaxAttempts(1))
	e.m.ProcessOnce(ctx)
	e.m.ProcessOnce(ctx)
	e.m.ProcessOnce(ctx)
	s, err := e.m.Stats(ctx)
	if err != nil || s.Done != 2 || s.Dead != 1 || s.Queued != 0 {
		t.Fatalf("stats = %+v %v", s, err)
	}
	n, err := PurgeDone(e.app, e.now.Add(time.Hour))
	if err != nil || n != 2 {
		t.Fatalf("purge = %d %v", n, err)
	}
}

func TestWorkersProcessInBackground(t *testing.T) {
	e := setup(t)
	e.m.Now = time.Now
	e.m.PollEvery = 20 * time.Millisecond
	done := make(chan struct{})
	e.m.Register("t.bg", func(context.Context, kernel.App, *kernel.Job) error { close(done); return nil })
	e.m.Start(2)
	defer e.m.Stop(5 * time.Second)
	if _, err := e.m.Enqueue(context.Background(), "t.bg", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not pick the job")
	}
}

func TestNoQueueFallbackReplaysHandlers(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()
	if _, err := kernel.Jobs(app).Enqueue(context.Background(), "x", nil); !errors.Is(err, kernel.ErrNoJobQueue) {
		t.Fatalf("err = %v", err)
	}
	kernel.Jobs(app).Register("early", func(context.Context, kernel.App, *kernel.Job) error { return nil })
	m := Register(app)
	if m.handler("early") == nil || m.handler("jobs.echo") == nil {
		t.Fatal("handlers not replayed")
	}
}

func TestEchoHandler(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	a, _ := e.m.Enqueue(ctx, "jobs.echo", map[string]string{"hi": "there"})
	b, _ := e.m.Enqueue(ctx, "jobs.echo", map[string]bool{"fail": true})
	e.m.ProcessOnce(ctx)
	e.m.ProcessOnce(ctx)
	if e.row(t, a).State != StateDone || e.row(t, b).State != StateFailed {
		t.Fatal("echo states")
	}
}

// lockedClock is a goroutine-safe fake clock for tests that advance time while
// handlers run.
type lockedClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *lockedClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *lockedClock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

// J1: a late completion of a reclaimed job must not overwrite its new state.
func TestJ1_FencedCompletionAfterReclaim(t *testing.T) {
	e := setup(t)
	clk := &lockedClock{t: e.now}
	e.m.Now = clk.Now
	ctx := context.Background()
	started, release := make(chan struct{}), make(chan struct{})
	e.m.Register("t.slow", func(context.Context, kernel.App, *kernel.Job) error {
		close(started)
		<-release
		return nil
	})
	id, _ := e.m.Enqueue(ctx, "t.slow", nil)
	finished := make(chan struct{})
	go func() { _, _ = e.m.ProcessOnce(ctx); close(finished) }()
	<-started
	clk.Add(StaleLockAfter + time.Minute)
	if n, err := e.m.ReclaimStale(ctx); err != nil || n != 1 {
		t.Fatalf("reclaim = %d %v", n, err)
	}
	close(release)
	<-finished
	r := e.row(t, id)
	if r.State != StateFailed || !strings.Contains(r.LastError, "lock expired") {
		t.Fatalf("late completion overwrote the row: %+v", r)
	}
}

// J1: the heartbeat keeps a long running job from being reclaimed.
func TestJ1_HeartbeatPreventsReclaim(t *testing.T) {
	e := setup(t)
	clk := &lockedClock{t: e.now}
	e.m.Now = clk.Now
	e.m.Heartbeat = 10 * time.Millisecond
	ctx := context.Background()
	started, release := make(chan struct{}), make(chan struct{})
	e.m.Register("t.slow", func(context.Context, kernel.App, *kernel.Job) error {
		close(started)
		<-release
		return nil
	})
	id, _ := e.m.Enqueue(ctx, "t.slow", nil)
	finished := make(chan struct{})
	go func() { _, _ = e.m.ProcessOnce(ctx); close(finished) }()
	<-started
	clk.Add(StaleLockAfter + time.Minute)
	want := fmtTime(clk.Now())
	waitFor(t, "heartbeat", func() bool { return e.row(t, id).LockedAt == want })
	if n, _ := e.m.ReclaimStale(ctx); n != 0 {
		t.Fatal("job with a live heartbeat was reclaimed")
	}
	close(release)
	<-finished
	if r := e.row(t, id); r.State != StateDone {
		t.Fatalf("%+v", r)
	}
}

// J2: a non-cooperative handler is cut off at MaxRuntime and counts as failed.
func TestJ2_EnqueueMaxRuntimeTimesOut(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	e.m.Register("t.hang", func(context.Context, kernel.App, *kernel.Job) error { <-block; return nil })
	id, _ := e.m.Enqueue(ctx, "t.hang", nil, kernel.MaxRuntime(50*time.Millisecond))
	start := time.Now()
	if ok, err := e.m.ProcessOnce(ctx); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("ProcessOnce was not released by the timeout")
	}
	r := e.row(t, id)
	if r.State != StateFailed || r.Attempt != 1 || !strings.Contains(r.LastError, "timeout") {
		t.Fatalf("%+v", r)
	}
}

func TestJ2_RegisterWithMaxRuntimeCancelsContext(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.m.RegisterWith("t.coop", func(c context.Context, _ kernel.App, _ *kernel.Job) error {
		<-c.Done()
		return c.Err()
	}, kernel.MaxRuntime(30*time.Millisecond))
	id, _ := e.m.Enqueue(ctx, "t.coop", nil, kernel.MaxAttempts(1))
	_, _ = e.m.ProcessOnce(ctx)
	r := e.row(t, id)
	if r.State != StateDead || !strings.Contains(r.LastError, "timeout") {
		t.Fatalf("%+v", r)
	}
}

// J3: a cron slot enqueued by a second process after the first run finished
// must not create a second job.
func TestJ3_CronSlotDedupeSurvivesDone(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	runs := 0
	e.m.Register("t.cron", func(context.Context, kernel.App, *kernel.Job) error { runs++; return nil })
	other := New(e.app) // second "process" on the same DB
	other.Now = e.m.Now
	id1, err := e.m.Enqueue(ctx, "t.cron", nil, kernel.Unique("cron:x:202610011200"))
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := e.m.ProcessOnce(ctx); !ok {
		t.Fatal("not processed")
	}
	id2, err := other.Enqueue(ctx, "t.cron", nil, kernel.Unique("cron:x:202610011200"))
	if err != nil || id2 != id1 {
		t.Fatalf("second enqueue = %q %v, want %q", id2, err, id1)
	}
	if ok, _ := e.m.ProcessOnce(ctx); ok || runs != 1 {
		t.Fatalf("ran twice: runs=%d", runs)
	}
	// non-cron unique keys still free up once the job is done
	a, _ := e.m.Enqueue(ctx, "t.cron", nil, kernel.Unique("user-key"))
	_, _ = e.m.ProcessOnce(ctx)
	if b, _ := e.m.Enqueue(ctx, "t.cron", nil, kernel.Unique("user-key")); b == a {
		t.Fatal("user key must be reusable after done")
	}
}

// J3: tables created by an older version are migrated in place.
func TestJ3_SchemaMigration(t *testing.T) {
	e := setup(t)
	db := e.app.AuxDB()
	for _, q := range []string{`DROP INDEX IF EXISTS idx__jobs_cron`, `ALTER TABLE _jobs DROP COLUMN cron_key`, `ALTER TABLE _jobs DROP COLUMN max_runtime_ms`} {
		if _, err := db.NewQuery(q).Execute(); err != nil {
			t.Fatal(q, err)
		}
	}
	if err := initSchema(e.app); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Enqueue(context.Background(), "t.x", nil, kernel.Unique("cron:a:1"), kernel.MaxRuntime(time.Second)); err != nil {
		t.Fatal(err)
	}
}

// J4: automatic retention.
func TestJ4_PurgeRetention(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.m.Register("t.ok", func(context.Context, kernel.App, *kernel.Job) error { return nil })
	e.m.Register("t.bad", func(context.Context, kernel.App, *kernel.Job) error { return errors.New("x") })
	doneID, _ := e.m.Enqueue(ctx, "t.ok", nil)
	deadID, _ := e.m.Enqueue(ctx, "t.bad", nil, kernel.MaxAttempts(1))
	for i := 0; i < 2; i++ {
		_, _ = e.m.ProcessOnce(ctx)
	}
	if n, _ := e.m.Purge(ctx); n != 0 {
		t.Fatalf("fresh rows purged: %d", n)
	}
	e.now = e.now.Add(8 * 24 * time.Hour)
	if n, _ := e.m.Purge(ctx); n != 1 {
		t.Fatalf("done purge = %d", n)
	}
	rows, _ := List(e.app, "", 0)
	if len(rows) != 1 || rows[0].ID != deadID {
		t.Fatalf("rows after 8d: %+v (done=%s)", rows, doneID)
	}
	e.now = e.now.Add(23 * 24 * time.Hour)
	if n, _ := e.m.Purge(ctx); n != 1 {
		t.Fatalf("dead purge = %d", n)
	}
}

func TestJ4_RetentionEnv(t *testing.T) {
	t.Setenv("TOKI_JOBS_RETENTION_HOURS", "2")
	t.Setenv("TOKI_JOBS_DEAD_RETENTION_HOURS", "bogus")
	if DoneRetention() != 2*time.Hour || DeadRetention() != DefaultDeadRetention {
		t.Fatal(DoneRetention(), DeadRetention())
	}
}

// J5: Stop is bounded even when the handler ignores ctx, and the interrupted
// job returns to the queue without losing an attempt.
func TestJ5_StopBoundedAndAttemptNotBurned(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	started := make(chan struct{})
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	e.m.Register("t.deaf", func(context.Context, kernel.App, *kernel.Job) error {
		close(started)
		<-block
		return nil
	})
	id, _ := e.m.Enqueue(ctx, "t.deaf", nil, kernel.MaxAttempts(1))
	e.m.Start(1)
	<-started
	begin := time.Now()
	e.m.Stop(600 * time.Millisecond)
	if d := time.Since(begin); d > 3*time.Second {
		t.Fatalf("Stop took %s", d)
	}
	// the worker releases the row shortly after Stop returns; poll instead of a single read
	r := e.waitRow(t, id, StateQueued, 3*time.Second)
	if r.State != StateQueued || r.Attempt != 0 {
		t.Fatalf("interrupted job: %+v", r)
	}
}

func TestJ5_CooperativeCancelDoesNotFailJob(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	started := make(chan struct{})
	e.m.Register("t.coop", func(c context.Context, _ kernel.App, _ *kernel.Job) error {
		close(started)
		<-c.Done()
		return c.Err()
	})
	id, _ := e.m.Enqueue(ctx, "t.coop", nil, kernel.MaxAttempts(1))
	e.m.Start(1)
	<-started
	e.m.Stop(300 * time.Millisecond)
	r := e.waitRow(t, id, StateQueued, 3*time.Second)
	if r.State != StateQueued || r.Attempt != 0 {
		t.Fatalf("job was charged for the shutdown: %+v", r)
	}
}

// J6: Retry must not abort the batch on a unique key collision.
func TestJ6_RetryUniqueCollision(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.m.Register("t.bad", func(context.Context, kernel.App, *kernel.Job) error { return errors.New("x") })
	for i := 0; i < 2; i++ {
		if _, err := e.m.Enqueue(ctx, "t.bad", nil, kernel.MaxAttempts(1), kernel.Unique("k")); err != nil {
			t.Fatal(err)
		}
		_, _ = e.m.ProcessOnce(ctx) // dead, key free again
	}
	n, err := Retry(e.app, "", e.now)
	if err != nil || n != 1 {
		t.Fatalf("Retry = %d %v", n, err)
	}
}

// Multiple workers on two module instances execute each job exactly once.
func TestConcurrentWorkersExactlyOnce(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	var mu sync.Mutex
	seen := map[string]int{}
	h := func(_ context.Context, _ kernel.App, j *kernel.Job) error {
		mu.Lock()
		seen[j.ID]++
		mu.Unlock()
		return nil
	}
	e.m.Register("t.n", h)
	other := New(e.app)
	other.Now = e.m.Now
	other.Register("t.n", h)
	const N = 40
	for i := 0; i < N; i++ {
		_, _ = e.m.Enqueue(ctx, "t.n", i)
	}
	e.m.PollEvery, other.PollEvery = 10*time.Millisecond, 10*time.Millisecond
	e.m.Start(3)
	other.Start(3)
	waitFor(t, "all jobs", func() bool { s, _ := e.m.Stats(ctx); return s.Done == N })
	e.m.Stop(time.Second)
	other.Stop(time.Second)
	for id, c := range seen {
		if c != 1 {
			t.Fatalf("job %s ran %d times", id, c)
		}
	}
	if len(seen) != N {
		t.Fatalf("ran %d of %d", len(seen), N)
	}
}
