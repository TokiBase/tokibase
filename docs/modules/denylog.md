# Module `denylog`

Structured log entries for denied requests. Package `modules/denylog`. Adds log rows only; the REST contract is unchanged.

- Enabled by default: `tokibase.go` calls `denylog.Register(app)` and adds the `deny` command.
- Env `TOKI_DENYLOG=on|off` (default `on`; also `false`, `0`, `disabled`).

## What is logged

A router middleware (priority just outside the upstream activity logger, so rate limit and auth errors are seen) inspects the finished response. When the status is 401, 403 or 429 it writes one Warn entry, message `denylog: request denied`, in the ordinary logs (`_logs`, retention and `Settings.Logs.MinLevel` apply) with these attributes:

| Attribute | Value |
| --- | --- |
| `toki.deny` | `true` (marker; the dotted key is literal) |
| `status` | 401, 403 or 429 |
| `method`, `path` | request method and URL path (no query string, so tokens in query params are not stored) |
| `ip` | `RealIP()` (honors the trusted proxy headers) |
| `auth_kind` | `guest`, `user` or `superuser` |
| `auth_id` | auth record id, empty for guests |
| `collection` | route param `collection`, only when present |
| `reason` | the upstream error message |
| `rule_kind` | `list`, `view`, `create`, `update` or `delete`; only for 403 on `/api/collections/{c}/records[/{id}]`, derived from path and method |
| `rate_limited` | `true` only for 429 |

`rule_kind` is derived, not read from the engine: a 403 on those routes can also come from a non-rule check (for example a manage-only field), so treat it as the operation that was denied.

The default activity logger still writes its own request line; the denial entry is separate and filterable.

## Sampling

At most 60 entries per minute per (status, route pattern). The 61st denial in a window writes one notice (`denylog: sampling, ...`), the rest are dropped and counted. When the window has ended, the next written denial is preceded by one summary line (`denylog: suppressed denials in the previous window`, with `suppressed`). Notice and summary carry `toki.deny=true` but not the request fields. Keying by route pattern keeps the table bounded; the sampler holds at most about 5000 keys. If traffic stops completely, a pending summary is only written at the next denial (it is not lost, just late).

## CLI

```
toki deny tail [--since 1h] [--limit 50] [--json]
```

Reads entries with `toki.deny=true` from `_logs` (`json_extract(data, '$."toki.deny"') = 1`), newest first. Logs are written in batches about every 3 seconds, so the newest denials can lag a few seconds. `--json` prints `id`, `created`, `message`, `data`.

Equivalent SQL for other tools: `SELECT created, data FROM _logs WHERE json_extract(data, '$."toki.deny"') = 1 ORDER BY created DESC LIMIT 50;`

## Not covered

Responses that never reach the router (connection-level errors, TLS failures), and denials written by custom code that bypasses the router middleware chain. If `Settings.Logs.MaxDays` is 0 (logs disabled) nothing is stored.
