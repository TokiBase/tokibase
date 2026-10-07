package jobs

import (
	"context"
	"errors"
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
