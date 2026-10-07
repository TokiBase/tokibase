# jobs

Durable job queue on the auxiliary database: retry with exponential backoff, dead-letter, cron scheduling and a worker role. It is the foundation for modules that need background work (push, webhooks, data jobs).

Disable with `TOKI_JOBS=off` (the table is then never created; an existing one is left as is).

## Consumer interface (kernel)

Modules never import `modules/jobs`; they use `kernel/jobs.go`:

```go
q := kernel.Jobs(app) // registered queue, or a fallback returning kernel.ErrNoJobQueue

q.Register("email.send", func(ctx context.Context, app kernel.App, job *kernel.Job) error {
    // job.ID, Kind, Payload (json.RawMessage), Attempt (1-based), MaxAttempts, RunAt, CreatedAt
    return nil // an error (or panic) schedules a retry
})

id, err := q.Enqueue(ctx, "email.send", payload,
    kernel.Delay(30*time.Second), kernel.MaxAttempts(5), kernel.Unique("welcome:"+userID))
stats, err := q.Stats(ctx)
```

`Register` may run before the queue exists: handlers registered on the fallback queue are replayed when `kernel.SetJobs` installs the real one, so init order does not matter. `Unique(key)` returns the id of the pending job with the same key instead of creating a second one; the key is released when the job is `done` or `dead`.

## Model

Table `_jobs` in `auxiliary.db`: `id, kind, payload (json), state, attempt, max_attempts, run_at, locked_by, locked_at, last_error, created, updated, unique_key`. A partial unique index on `unique_key` covers the pending states (`queued`, `running`, `failed`).

States: `queued` (waiting), `running` (claimed), `failed` (last attempt failed, waiting for the retry at `run_at`), `done`, `dead` (dead-letter, needs a human or `toki jobs retry`).

- Workers poll `state IN ('queued','failed') AND run_at <= now ORDER BY run_at` and claim a row with an atomic `UPDATE ... WHERE id=? AND state IN (...)`, so any number of workers/processes can share the database. The attempt counter is incremented at claim time.
- Backoff after failed attempt n: `5s * 2^(n-1)`, capped at 1h, jitter 0.8x to 1.2x.
- Dead-letter after `max_attempts` (default 10, `MaxAttempts(n)` per job). A job with no registered handler fails like any other.
- Locks older than 10 minutes (crashed worker) are reclaimed: requeued, or dead-lettered if all attempts were used.
- Dead-letter transitions are reported through `jobs.SetAuditSink` (action `jobs.dead`); `tokibase.go` forwards them to the audit log.
- Graceful shutdown (`OnTerminate`): workers stop claiming, running jobs get up to 30 s, then their context is canceled.

## Guarantees

At-least-once. A job can run more than once (retry after a timeout, a crash after the side effect but before the state update, a stale lock reclaim). Make handlers idempotent: use the job id or a business key as the idempotency key towards external systems, and check the target state before acting.

Jobs are not ordered across kinds; within one kind they start in `run_at` order but run concurrently.

## Cron

```go
jobs.Cron(app, "nightly-report", "0 3 * * *", "report.build", map[string]string{"scope": "all"})
```

Each tick enqueues a job (persisted, retried) instead of running inline. The job carries a `Unique` key per schedule slot, so several processes sharing the database enqueue it once. Cron ticks only run in processes that serve (`toki serve`).

## Configuration

| Env | Default | Meaning |
| --- | --- | --- |
| `TOKI_JOBS` | on | `off` disables the module |
| `TOKI_JOBS_WORKERS` | 4 | in-process workers; `0` runs none (use a separate worker process) |
| `TOKI_ROLE` | all | `worker`: run workers, answer only `/api/health` (everything else gets 503) |

## Worker role

```
toki serve --role worker --http 127.0.0.1:8091   # same as TOKI_ROLE=worker
```

The web processes run with `TOKI_JOBS_WORKERS=0`; one or more worker processes point at the same `pb_data` and process the queue. Workers still bind a port (health checks only). SQLite is single-host: workers must run on the same machine/volume as the data directory.

## CLI

```
toki jobs list [--state queued|running|done|failed|dead] [--limit N] [--json]
toki jobs retry <id>            # requeue one job (attempt counter reset)
toki jobs retry --dead          # requeue every dead job
toki jobs purge --done-before 7d
toki jobs stats [--json]
```

## Example handler

`jobs.echo` logs its payload; `{"fail":true}` makes it fail (to watch retry and dead-letter). It is registered by the module.
