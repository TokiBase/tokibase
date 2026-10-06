# Module `adminlock`

Runs a production server with the Admin UI read-only or not served at all, without a rebuild. Package `modules/adminlock`. No REST contract change for SDKs and the CLI.

Env `TOKI_ADMIN_UI` (read at process start):

| Value | Behavior |
| --- | --- |
| `on` (default, also unknown values) | upstream behavior |
| `readonly` | UI is served; schema, settings and superuser changes issued from the UI are rejected with 403 |
| `off` (`false`, `0`, `disabled`) | UI is not served: `/_/` returns 404 as in a `no_ui` build. All `/api/*` endpoints keep working |

## `off`

`Register` sets `ui.DistDirFS = nil` (process wide) before the router is built, so the `/_/` routes, `/_/extensions.js`, the installer's browser redirect and the OAuth2 redirect fallback behave exactly like a `no_ui` build. Side effects, same as `no_ui`: on first boot the installer prints `superuser upsert EMAIL PASS` instead of opening the browser, and the OAuth2 redirect shows a plain "you can close this window" page.

## `readonly`

Rejected (guards bound with priority `-1<<21`, before every other handler, including audit):

- collection create, update, delete, import (`OnCollection*Request`, `OnCollectionsImportRequest`)
- settings update (`OnSettingsUpdateRequest`)
- record create, update, delete on `_superusers`

Response:

```json
{"status":403,"message":"Admin UI is read-only on this server (TOKI_ADMIN_UI=readonly). Apply schema and settings changes through migrations or the CLI.","data":{}}
```

Regular record edits in the UI stay allowed (data edits are an operations task). Only `_superusers` records are guarded.

### Detection rule

A request counts as "from the Admin UI" when the caller has superuser auth AND (`Referer` or `Origin` parses to the same host as the request `Host` with path `/_` or `/_/...`, OR header `X-TokiBase-Admin-UI: 1`). The UI bundle does not send that header today (the frontend toolchain is not part of this change), so the Referer rule is what applies. Browsers send `Referer` for same-origin fetches by default; an `Origin` without path proves only the host and is not enough.

Limits: this is a guard against accidents, not a security boundary. Anyone with a superuser token can strip or forge `Referer`/`Origin` (curl, SDK) and change the schema; that is deliberate, since SDK/CLI/migration calls must never be blocked. Also not guarded: `/api/batch` sub-requests, collection truncate, backups and cron endpoints, Go/JS hooks and the `superuser` CLI command. A browser that suppresses `Referer` (strict `Referrer-Policy` set by a proxy) with a same-origin `Origin` of `/` will not be detected. To make the UI unusable for changes at the network level, use `off` or restrict `/_/` and write methods at the reverse proxy.

### Logging and audit

Every rejection is logged (Warn, `adminlock: blocked Admin UI request`, with action, collection, method, path, ip, actor). When the audit module is enabled, `tokibase.go` also appends an entry with action `admin.blocked`, `record = "<id or import>:<blocked action>"` (e.g. `import:collection.update`) and the request metadata, through `adminlock.SetAuditSink` (modules do not import each other). A boot warning states the mode and `toki serve` prints `Admin UI mode: <mode>` to stderr when it is not `on`.

## Recommended production setting

`TOKI_ADMIN_UI=readonly` (UI for inspecting data and logs), or `off` on hardened hosts. Apply schema changes through migration files from git:

```sh
toki migrate create "add_posts"     # develop locally, commit to git
toki migrate up                     # on the server (pending migrations also run at `serve` start)
```

(`migrate` is the `plugins/migratecmd` command; the app's `main` must register it, as in `examples/base`.)

```text
```

Settings: apply via migration (`app.Settings()` + `app.Save`) or the CLI/SDK with a token, without the UI Referer.

## Go API

`adminlock.Register(app)` (called by `tokibase.New*`), `adminlock.RegisterMode(app, mode)`, `adminlock.ModeFromEnv()`, `adminlock.FromAdminUI(e)`, `adminlock.SetAuditSink(fn)`.
