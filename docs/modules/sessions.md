# Module `sessions`

Server-side sessions so auth tokens can be revoked per device or en masse, without changing the PocketBase token format or any SDK flow. Package `modules/sessions`.

- Enabled by default: `tokibase.go` calls `sessions.Register(app)` and adds the `sessions` command.
- `TOKI_SESSIONS=off` (also `false`, `0`, `disabled`) skips registration: no table, no `sid` claim, no validation.
- `TOKI_SESSIONS_ROTATE=on` turns on refresh rotation (default off, see below).

## Model

Table `_sessions` in the MAIN database (`data.db`, so it is backed up and replicated with user data), created with `CREATE TABLE IF NOT EXISTS` at bootstrap.

| Column | Notes |
| --- | --- |
| `id` | 15-char pk (used by `toki sessions revoke`) |
| `collection` | auth collection id |
| `record` | auth record id |
| `token_id` | unique 16-char id, embedded in the JWT as claim `sid` |
| `kind` | `auth` (password/OTP/OAuth2/...), `refresh` (issued by auth-refresh), `static` (non-refreshable: impersonation, `NewStaticAuthToken`) |
| `created`, `last_seen`, `expires` | PocketBase datetime layout, UTC; `expires` = token expiry |
| `ip`, `user_agent` | of the request that issued the token |
| `device` | header `X-Toki-Device` of the login/refresh request (max 128 chars); a refresh without the header inherits the previous session's device |
| `revoked`, `revoked_reason` | `NULL` while active. Reasons: `cli`, `password_changed`, `email_changed`, `rotated`, `reuse_detected`, or whatever the Go caller passed |

## Which tokens are covered

Every auth token built by `Record.NewAuthToken` / `NewStaticAuthToken` (all login flows, refresh, impersonation). File, verification, password-reset and email-change tokens are not sessions. Tokens issued before the module existed carry no `sid`: they stay valid until their natural expiry (or until the record's `tokenKey` changes, as in upstream). To cut them off early, change the user's password or rotate `tokenKey`.

A session row lost to `purge` or missing for another reason is treated like a token without `sid` (valid until its own expiry), so purge only expired rows.

## Seam

`kernel.OnAuthTokenIssue func(record *Record, kind string, claims jwt.MapClaims, duration time.Duration)` is called right before signing. The module adds `sid` and inserts the row. When the var is nil (default, module off) token creation is byte for byte upstream. It is process wide: one app per process. If the insert fails, the token is issued without `sid` and an error is logged. A future MFA or passkey flow creates sessions through the same seam, because it ends in `NewAuthToken`.

The JWT format only gains the `sid` claim; signature, key (`tokenKey + secret`), other claims and durations are unchanged.

## Validation

An `OnServe` middleware runs right after upstream `loadAuthToken`. If the loaded token has `sid` and its session is revoked or past `expires`, `e.Auth` is cleared: authed endpoints answer upstream's `401 {"status":401,"message":"The request requires valid record authorization token.","data":{}}`, public endpoints treat the caller as a guest. `last_seen` is updated at most once per minute per session. The check is one indexed read per authed request.

## Rotation (`TOKI_SESSIONS_ROTATE=on`)

On `auth-refresh` of a refreshable token, the new token gets a new session and the previous one is revoked (`rotated`). Presenting the rotated-out token again is treated as theft: every active session of that user and device (same `device` value, empty counts as one device) is revoked (`reuse_detected`) and `auth.reuse_detected` is audited.

Trade-off, why it is off by default: the JS SDK auto-refresh sends the same token from parallel tabs/processes; with rotation the second tab presents an already rotated token and is logged out (and trips reuse detection). Enable it only for clients with a single refresher per device, and send a distinct `X-Toki-Device` per device.

## Revoke without REST changes

```
toki sessions list <collection> <user> [--json]
toki sessions revoke <id>                 # session row id or token id
toki sessions revoke-all <collection> <user>
toki sessions purge --expired
```

Go: `sessions.RevokeUser(app, collection, userId, reason)`, `sessions.Revoke`, `sessions.List`, `sessions.PurgeExpired` (collection by name or id).

Automatic revoke-all (reason in brackets): record update with a changed password (`password_changed`) or email (`email_changed`) on an auth collection. Upstream also rotates `tokenKey` on a password change; this works even when the `tokenKey` is kept. Lockout engage does not revoke sessions (kept separate).

CLI revocations from another process take effect immediately (validation reads the DB).

## Audit

`sessions.SetAuditSink(fn)` (wired to `_audit` in `tokibase.go`): `auth.session_revoke`, `auth.session_revoke_all`, `auth.reuse_detected`. Automatic revokes via the update hook go through `RevokeUser` and are audited as `auth.session_revoke_all` with the reason.

## Limits

Single writer process per `pb_data`. Rows are only deleted by `purge`; schedule `toki sessions purge --expired` (cron) if login volume is high.
