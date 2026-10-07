# Module `timelint`

Detects date values submitted without a time zone. Package `modules/timelint`. Default policy only logs; `strict` adds one validation error code.

- Enabled by default (policy `warn`): `tokibase.go` calls `timelint.Register(app)` and adds the `time` command.
- Env `TOKI_TIMELINT`: `warn` (default), `strict`, `off` (also `false`, `0`, `disabled`; then nothing is registered).

## Why

A client that sends `2026-10-07 10:00:00` means local time, the server reads it as UTC. If the client later reads the stored value back and re-sends it as local time, each sync moves the value by the offset ("+7 hours per sync" at UTC+7).

## What is flagged

A string submitted for a field of type `date` in `POST` or `PATCH /api/collections/{c}/records[/{id}]` is flagged only when all hold:

- it parses successfully with the app datetime parsing today (`types.ParseDateTime`),
- it has a time part (contains `:`),
- it has no zone designator at the end of the time part: no `Z`, `UTC`, `GMT`, `+hh:mm`, `-hh:mm`, `+hhmm`, `-hhmm`, `+hh`, `-hh`.

Not flagged: date-only `YYYY-MM-DD`, empty values, non-string values (numbers), strings that do not parse, and anything with a zone, including the stored layout `2006-01-02 15:04:05.000Z`.

The raw string is captured by a router middleware before the upstream handler runs (the record hooks see the body after it was normalized into a datetime and the zone is gone). Not covered: `/api/batch` sub-requests, Go/JS hooks that call `Record.Set` directly, and the admin import endpoints.

## Policies

| Policy | Effect |
| --- | --- |
| `warn` | request proceeds; `Logger.Warn` `timelint: date value submitted without a time zone (parsed as UTC)` with `collection`, `field`, `sample`, at most once per collection+field per hour |
| `strict` | request is rejected with 400 before any write |
| `off` | nothing |

Strict response (same shape as upstream field validation errors; message is `Failed to create record.` or `Failed to update record.`):

```json
{"status":400,"message":"Failed to create record.","data":{"at":{"code":"validation_invalid_timezone","message":"Must include a time zone (Z or +hh:mm) or be a date only (YYYY-MM-DD)."}}}
```

## CLI

```
toki time lint [--json]
```

Scans all non-view collections and prints, per date field, the number of stored values and how many are exactly `00:00:00` (info only; a hint at date-only inputs). Upstream normalizes stored values to UTC, so a missing zone cannot be seen in storage. JSON rows: `collection`, `field`, `total`, `midnight`. `toki rule lint` is unchanged.
