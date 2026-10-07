# Module `totp`

TOTP (RFC 6238, SHA-1, 6 digits, 30 s) as an additional MFA method on any auth collection (including `_superusers`). It plugs into PocketBase's existing `mfaId` flow: the first method (password, OTP, OAuth2, passkey) answers `401 {mfaId}`, then `POST /api/collections/{c}/auth-with-totp` finishes the login with the standard auth response (`AuthMethod` `totp`). Also: single-use recovery codes, per-role enforcement, a break-glass CLI. Package `modules/totp`, no third-party OTP library (HMAC is about 30 lines); the QR code uses `github.com/skip2/go-qrcode` (pure Go).

## Enabling

The module is always registered. Enrolment needs a key to encrypt secrets at rest:

| Env | Meaning |
| --- | --- |
| `TOKI_TOTP_KEY` | base64 of 32 random bytes (`openssl rand -base64 32`). Takes precedence. An invalid value disables enrolment (no silent fallback). |
| (fallback) | sha256 of the value of the settings encryption env (`--encryptionEnv`), when set. |
| `TOKI_TOTP_ISSUER` | Issuer shown in the authenticator app. Default: app name from settings. |
| `TOKI_TOTP_REQUIRED_ROLES` | Comma list of role values; users whose `roles` or `role` field contains one must have TOTP. |
| `TOKI_TOTP_REQUIRE_SUPERUSERS` | `1` requires TOTP for `_superusers`. |
| `TOKI_TOTP_GRACE_DAYS` | Days after enforcement started during which users without TOTP can still log in (default 0). |

With neither key set, `setup` answers `503` and one `Info` line is logged at boot; the `_totp` collection is then not created. Losing the key makes every stored secret unreadable (users must re-enrol); keep it with your backups' secrets, not inside `pb_data`.

To use MFA at all, the collection needs `mfa.enabled` (upstream additionally requires two of password/OTP/OAuth2 to be enabled to turn it on). TOTP counts as a second method for users who have it enabled; users without TOTP keep using another second method. `auth-with-totp` answers `403` when the collection has MFA disabled.

## Endpoints

| Method and path | Auth | Purpose |
| --- | --- | --- |
| `POST /api/collections/{c}/totp/setup` | record of `{c}` + fresh auth | body `{password?}`; creates a pending secret, returns `{secret, otpauth_url, qr_svg, recovery_codes}` (codes shown once). Calling it again before `confirm` replaces the pending secret. `400` if already enabled. |
| `POST .../totp/confirm` | same | body `{code}`; enables TOTP after a valid code. 5 attempts per minute per record. |
| `DELETE .../totp` | same + fresh auth | body `{password?}`; removes secret and recovery codes, 204. |
| `POST .../totp/recovery/regenerate` | same + fresh auth | body `{password?}`; replaces all recovery codes, returns `{recovery_codes}`. |
| `POST .../auth-with-totp` | none | body `{mfaId, code}`; `code` is the 6-digit TOTP or a recovery code. Returns `{token, record, meta}` with `meta.totp` = `totp` or `recovery`. |

Fresh auth is the same rule as passkeys: a refreshable session token issued less than 10 minutes ago, or the current `password` in the body (wrong password: 403, counted as lockout failure, audit `auth.totp_reauth_failed`).

### Login

```js
// 1. first factor
let mfaId;
try {
  await pb.collection('users').authWithPassword(email, password);
} catch (err) {
  if (err.status !== 401 || !err.response?.mfaId) throw err;
  mfaId = err.response.mfaId;
}
// 2. second factor: TOTP code (or a recovery code)
const res = await pb.send('/api/collections/users/auth-with-totp', {
  method: 'POST',
  body: { mfaId, code },
});
pb.authStore.save(res.token, res.record);
```

The `_mfas` record must exist, be unexpired (`mfa.duration`), belong to this collection and its user, and not have been created by `totp` itself; it is deleted on success (by the upstream `RecordAuthResponse`). Any failure answers `400` (`Failed to authenticate.` for a bad code).

### Enrolment

```js
const s = await pb.send('/api/collections/users/totp/setup', { method: 'POST', body: {} });
showQr(s.qr_svg);            // or give s.secret / s.otpauth_url
showOnce(s.recovery_codes);  // 10 codes of 10 characters
await pb.send('/api/collections/users/totp/confirm', { method: 'POST', body: { code } });
```

Flutter: render `otpauth_url` with a QR package, or open it with the platform (`otpauth://` is handled by most authenticator apps).

## Security properties

- Secrets: 160-bit random, stored AES-256-GCM encrypted (`v1:` prefix, field hidden); never returned after `setup`.
- Window +-1 step; constant-time comparison; a counter at or below the last accepted one is rejected (a code works once, also across `confirm` and login).
- Rate limit: 5 code attempts per minute per `mfaId` (429 with `Retry-After`), plus 50 per minute per client IP. Every wrong login code counts a lockout failure through `lockout.RecordFailureFor` (same key as password login) and a locked record is refused (wired in `tokibase.go`).
- Recovery codes: 10 codes of 10 characters from an unambiguous alphabet, stored as sha256 hashes, single use, accepted instead of `code` (case and spaces ignored). Use audits `auth.recovery_code_used`.
- Verification and consumption are serialized per process.

## Enforcement

Users matching `TOKI_TOTP_REQUIRED_ROLES` (field `roles` or `role`, string or multi-select) or, with `TOKI_TOTP_REQUIRE_SUPERUSERS=1`, superusers, must have TOTP enabled. Otherwise any login answers `403` with `data.totp.code = "totp_required"` and instructions in `message`. Users inside the grace window pass. The start of enforcement is `_params` key `totp_enforced_at` (written at boot or by the CLI the first time enforcement is configured); the window ends `TOKI_TOTP_GRACE_DAYS` later. Users must therefore enrol during the grace period; to let a blocked user enrol, extend it with `toki totp enforce --roles ... --restart-grace`. Enforcement applies to the first method; `auth-refresh` is not affected. Each block is audited as `auth.totp_required_blocked`.

## CLI

```
toki totp status <collection> <user>      # user = record id or email
toki totp disable <collection> <user>     # break-glass, audited (by: cli)
toki totp enforce --roles admin --superusers [--restart-grace]
toki totp enforce --off                    # clear the stored set (env settings remain)
toki totp enforce                          # show the effective set
```

## Audit

With `TOKI_AUDIT` on: `auth.totp_enabled`, `auth.totp_disabled` (`by` owner or cli), `auth.recovery_code_used`, `auth.recovery_codes_regenerated`, `auth.totp_enforcement_changed`, `auth.totp_required_blocked`, `auth.totp_reauth_failed`.

## Storage

Collection `_totp` (main DB, system, all rules null): `collection`, `record`, `secret` (hidden, encrypted), `issuer`, `enabled`, `recovery_codes` (hidden JSON list of sha256 hashes), `last_used_at`, `last_counter`, `created`; unique per (collection, record). Deleting an auth record deletes its row.

## Limits

- SHA-1, 6 digits, 30 s only (what authenticator apps universally support).
- Not a first factor: `auth-with-totp` only completes an existing challenge.
- Rate-limit state is in memory and per process; single process per `pb_data`.
- No per-device trust ("remember this browser").
