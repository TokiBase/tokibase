# Module `lockout`

Progressive per-identity lockout for failed authentication (password and OTP). Package `modules/lockout`. No REST contract change except a `Retry-After` header on locked identities.

- Enabled by default: `tokibase.go` calls `lockout.Register(app)` and adds the `lockout` command.
- Disable with env `TOKI_LOCKOUT=off` (also `false`, `0`, `disabled`). Then no hooks and no table; the CLI command stays available to inspect or clear an existing table.

## Why per identity

Upstream rate limits (`Settings.RateLimits`, path and IP based) throttle one client. Credential stuffing spreads attempts over many IPs, each below the limit, against one account. This module counts failures per identity, whatever the IP, so the attack on that account slows down and the owner's account stops accepting guesses. The two layers are independent: upstream limits stay as they are; the lockout is not an IP feature and never reads the client IP.

## Identity and scope

| Flow | Hook point | Identity key |
| --- | --- | --- |
| password | `OnRecordAuthWithPasswordRequest` | `<collection>:<lowercase trimmed submitted identity>` (email or username as typed; unknown identities lock too) |
| OTP | route middleware on `POST /api/collections/{c}/auth-with-otp` | `<collection>:<auth record id>` resolved from the submitted `otpId` |

`OnRecordAuthWithOTPRequest` only fires after the OTP password was already validated, so wrong OTPs never reach it; that is why OTP uses a router middleware. An `otpId` that resolves to no record cannot be keyed and is passed through to upstream (which already limits 5 tries per 180 s per record).

Superusers (`_superusers`) are covered by the same hooks: their failures lock just the same. Recovery path for a locked superuser is the CLI on the server (`toki lockout unlock _superusers admin@example.com`; `toki superuser` manages the credentials themselves).

A password submitted as email and the same account submitted as username are two keys. Failures count only for HTTP 400 results; a correct password that only needs the MFA step (`401` + `mfaId`) counts as success and resets the record.

## Policy

| Env | Default | Meaning |
| --- | --- | --- |
| `TOKI_LOCKOUT` | `on` | `off` disables |
| `TOKI_LOCKOUT_THRESHOLD` | `5` | failures per lock |
| `TOKI_LOCKOUT_WINDOW` | `15m` | failures are forgotten after this much quiet time (measured from the last failure, or from the end of a lock) |
| `TOKI_LOCKOUT_BASE` | `1m` | first lock length |
| `TOKI_LOCKOUT_MAX` | `1h` | cap |

Invalid values fall back to the default. Every `THRESHOLD`-th failure engages a lock of `BASE * 2^(locks so far in the window)`, capped at `MAX`. Attempts made while locked are rejected and do not count. A success resets the record.

Example with the defaults (consecutive failures, no quiet gap longer than 15 min):

| Failure # | Lock engaged | Length |
| --- | --- | --- |
| 1-4 | no | |
| 5 | 1st | 1 m |
| 10 | 2nd | 2 m |
| 15 | 3rd | 4 m |
| 20 | 4th | 8 m |
| 25 | 5th | 16 m |
| 30 | 6th | 32 m |
| 35 and later | 7th+ | 1 h (cap) |

## Enumeration-safe behavior

A locked identity gets the same status and body as a wrong credential (`400`, `Failed to authenticate.` for password, `Invalid or expired OTP` for OTP), also when the identity does not exist and also when the submitted password is correct. The only difference is `Retry-After: <seconds>`, sent only while locked. Because unknown identities lock the same way, the header does not reveal whether an account exists. Anyone can lock a known identity by failing on purpose (the usual trade-off of account lockout); the lock is short at first and the owner can still use OAuth2/other flows, and operators can unlock via CLI.

## Storage

Table `_lockout` in `auxiliary.db`, created with `CREATE TABLE IF NOT EXISTS` at bootstrap (same approach as `_audit`): `key` (pk), `failures`, `first_failure`, `locked_until` (nullable), `updated`. Number of locks in the window is derived from `failures / threshold`.

An in-memory front (`tools/store`, capped at 20000 entries) avoids one DB write per failure: a row is written on every 5th failure and whenever a lock engages. A crash loses at most 4 uncounted failures, never a lock. A check for a locked identity re-reads its row, so `unlock`/`clear` run from the CLI (another process) take effect immediately. Single writer process, like `audit`.

## Audit and logs

When a lock engages: a `Warn` log `lockout: identity locked ...` with `collection` and `identity` = first 16 hex chars of `sha256(<collection>:<identity>)` (never the raw email), and, if the audit module is enabled, an `auth.lockout` entry (`collection`, `record` = the same hash, `after` = failures and lock end). The wiring is `lockout.SetAuditSink(...)` in `tokibase.go`; modules do not import each other.

## CLI

```
toki lockout list [--json]
toki lockout unlock <collection> <identity>
toki lockout clear
```

`list` and `unlock` show/use the raw key (operators need it); `unlock` lowercases the identity.

## Not covered

OAuth2 sign-in (the provider authenticates), the second MFA step, and cluster-wide state (one process per `pb_data`).
