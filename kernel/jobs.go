package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

// ErrNoJobQueue is returned by the fallback queue when no job queue
// implementation (modules/jobs) was registered for the app.
var ErrNoJobQueue = errors.New("no job queue registered")

// Job is a unit of durable background work.
type Job struct {
	ID          string
	Kind        string
	Payload     json.RawMessage
	Attempt     int // 1-based number of the current execution
	MaxAttempts int
	RunAt       time.Time
	CreatedAt   time.Time
}

// JobHandler executes a job. Returning an error schedules a retry
// (or dead-letters the job after MaxAttempts).
//
// Delivery is at-least-once: handlers must be idempotent.
type JobHandler func(ctx context.Context, app App, job *Job) error

// JobStats is a snapshot of the queue by state.
type JobStats struct {
	Queued  int64 `json:"queued"`
	Running int64 `json:"running"`
	Done    int64 `json:"done"`
	Failed  int64 `json:"failed"` // failed, waiting for the next retry
	Dead    int64 `json:"dead"`
}

// EnqueueOptions holds the resolved [EnqueueOption] values.
type EnqueueOptions struct {
	Delay       time.Duration
	MaxAttempts int           // 0 = queue default
	UniqueKey   string        // "" = no deduplication
	MaxRuntime  time.Duration // 0 = handler/queue default
}

// EnqueueOption customizes a single Enqueue call.
type EnqueueOption func(*EnqueueOptions)

// Delay postpones the first execution by d.
func Delay(d time.Duration) EnqueueOption { return func(o *EnqueueOptions) { o.Delay = d } }

// MaxAttempts overrides the queue default number of attempts (n > 0).
func MaxAttempts(n int) EnqueueOption { return func(o *EnqueueOptions) { o.MaxAttempts = n } }

// MaxRuntime caps one execution of the job (the handler context is canceled
// after d and the attempt counts as failed). 0 keeps the default.
func MaxRuntime(d time.Duration) EnqueueOption { return func(o *EnqueueOptions) { o.MaxRuntime = d } }

// Unique deduplicates by key: while a job with the same key is still pending
// (queued, running or waiting for a retry) Enqueue returns its id instead of
// creating a new one.
func Unique(key string) EnqueueOption { return func(o *EnqueueOptions) { o.UniqueKey = key } }

// ResolveEnqueueOptions applies opts (helper for queue implementations).
func ResolveEnqueueOptions(opts ...EnqueueOption) EnqueueOptions {
	var o EnqueueOptions
	for _, fn := range opts {
		if fn != nil {
			fn(&o)
		}
	}
	return o
}

// JobQueue is the consumer facing job queue contract. Modules depend only on
// this interface (never on modules/jobs).
type JobQueue interface {
	// Enqueue persists a job and returns its id.
	Enqueue(ctx context.Context, kind string, payload any, opts ...EnqueueOption) (string, error)
	// Register binds the handler of a job kind (call it at init time).
	Register(kind string, h JobHandler)
	// Stats returns the queue counters.
	Stats(ctx context.Context) (JobStats, error)
}

const (
	jobsStoreKey   = "__tokiJobQueue__"
	pendingJobsKey = "__tokiJobHandlers__"
)

var jobsMu sync.Mutex

type pendingJobHandlers map[string]JobHandler

// Jobs returns the job queue registered for app, or a fallback queue whose
// Enqueue and Stats return [ErrNoJobQueue]. Handlers registered on the
// fallback are kept and replayed into the real queue by [SetJobs], so the
// module init order does not matter.
func Jobs(app App) JobQueue {
	jobsMu.Lock()
	defer jobsMu.Unlock()
	if q, ok := app.Store().Get(jobsStoreKey).(JobQueue); ok && q != nil {
		return q
	}
	return noJobQueue{app: app}
}

// SetJobs registers q as the job queue of app and replays the handlers that
// were registered before it.
func SetJobs(app App, q JobQueue) {
	jobsMu.Lock()
	app.Store().Set(jobsStoreKey, q)
	pending, _ := app.Store().Get(pendingJobsKey).(pendingJobHandlers)
	app.Store().Remove(pendingJobsKey)
	jobsMu.Unlock()
	if q == nil {
		return
	}
	for kind, h := range pending {
		q.Register(kind, h)
	}
}

type noJobQueue struct{ app App }

func (noJobQueue) Enqueue(context.Context, string, any, ...EnqueueOption) (string, error) {
	return "", ErrNoJobQueue
}

func (noJobQueue) Stats(context.Context) (JobStats, error) { return JobStats{}, ErrNoJobQueue }

func (n noJobQueue) Register(kind string, h JobHandler) {
	jobsMu.Lock()
	defer jobsMu.Unlock()
	p, _ := n.app.Store().Get(pendingJobsKey).(pendingJobHandlers)
	if p == nil {
		p = pendingJobHandlers{}
	}
	p[kind] = h
	n.app.Store().Set(pendingJobsKey, p)
}
