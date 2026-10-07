# Module `passkey`

WebAuthn/FIDO2 passkeys for any auth collection (including `_superusers`): registration for logged-in users, passwordless login with discoverable credentials, and management. Package `modules/passkey`, library `github.com/go-webauthn/webauthn` v0.18.2. Additive endpoints only; the login result is the standard PocketBase auth response, so `pb.authStore.save(token, record)` works.

## Enabling

The module is inactive until the relying party is configured. Inactive means: no routes (every `/passkeys` path answers 404), no `_passkeys` collection, one `Info` log line at boot.

| Env | Meaning |
| --- | --- |
| `TOKI_PASSKEY_RP_ID` | Required. Registrable domain the passkeys are bound to, for example `fitgymrun.com`. Cannot be changed later without invalidating every passkey. |
| `TOKI_PASSKEY_RP_NAME` | Display name shown by the authenticator. Default: the RP ID. |
| `TOKI_PASSKEY_ORIGINS` | Comma list of allowed origins. Default: `https://<RP ID>`. |
| `TOKI_PASSKEY_CLONE_POLICY` | `warn` (default) or `deny`, see "Clone detection". |
| `TOKI_PASSKEY_ALLOW_IDENTITY_HINT` | `1` makes `login/options` honour `identity` and return `allowCredentials` (account enumeration oracle, off by default). |

`toki passkey config` prints the effective values and reports an invalid configuration.

### Origins for web, Android and iOS

- Web: every site that calls the WebAuthn API, as `https://app.fitgymrun.com`. The RP ID must be a registrable suffix of each web origin.
- Android (Credential Manager): the origin is `android:apk-key-hash:<base64url sha256 of the signing certificate>`. List one per signing key (Play app signing key and the upload key if you test with it). Android also needs `https://<RP ID>/.well-known/assetlinks.json` with `delegate_permission/common.get_login_creds` for the package.
- iOS (ASAuthorization): native apps send the web origin `https://<RP ID>`; add the `webcredentials:<RP ID>` associated domain to the app and serve `https://<RP ID>/.well-known/apple-app-site-association` with the app id under `webcredentials.apps`.

Example: `TOKI_PASSKEY_ORIGINS=https://fitgymrun.com,android:apk-key-hash:AbC...`.

## Endpoints

All under `/api/collections/{collection}/passkeys/`. JSON bodies, standard PocketBase error shape.

| Method and path | Auth | Purpose |
| --- | --- | --- |
| `POST register/options` | record of `{collection}` | `PublicKeyCredentialCreationOptions` (JSON form), `residentKey: required`, `userVerification: preferred`, existing credentials excluded; rate limited |
| `POST register/verify` | same + fresh auth | body `{credential, name, password?}` (`name` max 64, default `Passkey`); stores the passkey, returns its view (with `uvCapable`) |
| `GET` | same | `{items: [...]}` own passkeys (`id`, `name`, `created`, `lastUsed`, `aaguid`, `transports`, `backupEligible`, `backupState`, `cloneSuspected`; never key material) |
| `DELETE {id}` | same + fresh auth | optional JSON body `{password}`; delete an own passkey (404 for anyone else's), 204 |
| `POST login/options` | none | body optional `{identity}` (ignored unless the hint env is set); `PublicKeyCredentialRequestOptions`, always `userVerification: required`; rate limited |
| `POST login/verify` | none | body `{credential, mfaId?}`; returns `{token, record, meta}` |

At most 20 passkeys per record.

`login/options` always answers a discoverable ceremony without `allowCredentials`, whatever `identity` is sent (or whether the account exists), so it cannot be used to find accounts that have passkeys. The authenticator offers the user's passkeys and the user handle (the record id) selects the record. Only with `TOKI_PASSKEY_ALLOW_IDENTITY_HINT=1` is `identity` honoured: an identity that has passkeys then gets `allowCredentials` (unknown ones still get the discoverable answer, which is the enumeration oracle).

### User verification

Login always requires user verification (`userVerification: required` in the options, and the UV flag of the authenticator data is enforced on verify, also for sessions created before an upgrade), because a passkey replaces the password: presence alone (a PIN-less key, a device with biometrics off) is not accepted. Registration keeps `preferred` so such authenticators can still enrol; the result is stored as `uv_capable` (returned as `uvCapable`). A passkey with `uv_capable=false` cannot log in; remove it. Existing installs get the `uv_capable` field added on boot (absent means false).

### Fresh authentication (register/verify, DELETE)

Managing passkeys needs proof of recent authentication, otherwise a stolen session token could plant a backdoor that survives password resets and session revocation. The request passes if either:

- the auth token is a refreshable session token issued less than 10 minutes ago (issue time is `exp` minus the collection token duration; impersonation and static tokens never qualify), or
- the body carries `password`, verified against the record (superuser and OAuth-only records without a password need a fresh token). A wrong password answers 403, counts a lockout failure and writes the audit action `auth.passkey_reauth_failed`; a locked record is refused.

Answer is `403 Recent authentication required...`. `auth-refresh` resets `exp`, so a refreshed stolen token looks fresh: use the `password` field for high-risk apps. Audit entries for register/delete carry `reauth: token|password`.

### Rate limits and challenge budget

`login/options` and `register/options`: 10 per minute per client IP and 10 per minute per identity (login, the submitted identity; register, the record id), then `429` with `Retry-After`. Pending challenges: at most 20 per client IP (the oldest are evicted) and 200000 per collection. The limiter is in memory and per process.

Challenges live in `_passkey_challenges` (`auxiliary.db`), keyed by the challenge value, valid 5 minutes, single use (consumed atomically on verify, even when verification then fails), bound to the collection and, for registration, to the user who requested them. Expired rows are purged on each new challenge; see the caps above.

## Login semantics

`login/verify` validates the assertion (origin, RP ID hash, signature, user verification flags), then calls `apis.RecordAuthResponse(e, record, "passkey", {"passkey": "<id>"})`. That gives, without extra code:

- `authRule` of the collection (a false rule answers 403, as for password login),
- MFA: with MFA enabled the response is `401 {mfaId}`; finish with another method (for example `auth-with-password` plus `mfaId`) or repeat `login/verify` with `mfaId` after a different first factor. The `_mfas` method is `passkey`. Upstream validation still requires two of password/OTP/OAuth2 to be enabled to turn MFA on (passkey is not counted),
- `OnRecordAuthRequest` hooks with `AuthMethod == "passkey"`, the sessions module (`sid`), superuser IP allow-list, login alerts.

`@request.context` is the default value (no `passkey` context exists upstream).

Any verification failure answers `400 Failed to authenticate.` (details only in the server log).

## Clone detection

If the authenticator's counter does not increase (both values non-zero; authenticators that always report 0, such as Apple passkeys, are never flagged), the passkey gets `clone_suspected=true`, its stored counter is kept, a `Warn` log is written and the audit action `auth.passkey_clone_suspected` is recorded. With `warn` (default) the login proceeds; with `deny` it answers 400 (not counted as a lockout failure). Synced passkeys report counter 0, so clones of them cannot be detected.

## Lockout and audit

- Lockout (wired in `tokibase.go` with `passkey.SetFailureSink(lockout.RecordFailureFor)` and `passkey.SetLockedSink(lockout.IsLocked)`): the key is the same as for password login, `<collection>:<lowercased email, else username>` (`lockout.IdentityKey`), so password and passkey failures add up. A passkey login for a locked record answers the same `400 Failed to authenticate.` and does not extend the lock; the password re-auth of the previous section is refused too. To stop an attacker locking a victim with public identifiers, a failure counts only when the credential is known, its user handle matches the owner and the signature is well-formed but invalid. Unknown credential ids, wrong origin/RP ID, wrong or reused challenge, missing UV and a user handle mismatch never count. Residual risk: someone who knows a credential id and the record id can still trigger counted failures; the per-IP limits bound the rate.
- Audit (`passkey.SetAuditSink`, needs `TOKI_AUDIT` on): `auth.passkey_register`, `auth.passkey_delete` (`by` owner or cli), `auth.passkey_login` (`method: passkey`, `mfa`), `auth.passkey_clone_suspected`.

## Storage

Collection `_passkeys` (main DB, system, all rules null so only superusers read it): `collection` (collection id), `record` (user id), `credential_id` (base64url, unique), `public_key` (base64url, hidden), `aaguid`, `sign_count`, `transports`, `backup_eligible`, `backup_state`, `clone_suspected`, `uv_capable`, `name`, `last_used`, `created`. Deleting an auth record deletes its passkeys.

## CLI

```
toki passkey list <collection> <user>    # user = record id or email
toki passkey rm <id>
toki passkey config
```

## Client flows (JS SDK)

```js
const base = (c) => `/api/collections/${c}/passkeys`;

// register (logged in)
async function addPasskey(pb, name) {
  const options = await pb.send(`${base('users')}/register/options`, { method: 'POST' });
  const cred = await navigator.credentials.create({
    publicKey: PublicKeyCredential.parseCreationOptionsFromJSON(options),
  });
  return pb.send(`${base('users')}/register/verify`, {
    method: 'POST',
    body: { credential: cred.toJSON(), name },
  });
}

// passwordless login (discoverable; pass an identity to restrict it)
async function loginWithPasskey(pb, identity) {
  const options = await pb.send(`${base('users')}/login/options`, {
    method: 'POST',
    body: identity ? { identity } : {},
  });
  const cred = await navigator.credentials.get({
    publicKey: PublicKeyCredential.parseRequestOptionsFromJSON(options),
    // mediation: 'conditional' enables passkey autofill on an <input autocomplete="username webauthn">
  });
  const res = await pb.send(`${base('users')}/login/verify`, {
    method: 'POST',
    body: { credential: cred.toJSON() },
  });
  pb.authStore.save(res.token, res.record);
  return res;
}
```

`parseCreationOptionsFromJSON` / `parseRequestOptionsFromJSON` / `toJSON` need a current browser (Chrome 129, Safari 18.4, Firefox 133); for older ones convert the base64url fields by hand. Handle the MFA case (`ClientResponseError` with `status 401` and `response.mfaId`) like for other methods.

### Flutter / native

Use a passkey plugin (for example `passkeys` on pub.dev, or `credential_manager`) and feed it the JSON returned by `register/options` / `login/options` (its fields follow the WebAuthn JSON format); post the plugin's JSON result as `credential`, then `pb.authStore.save(token, RecordModel.fromJson(record))`. Configure the RP ID with the associated domains (iOS) and Digital Asset Links (Android) described above, and list the Android apk-key-hash origin in `TOKI_PASSKEY_ORIGINS`.

## Limits

- No attestation verification: registration asks for `none` attestation and does not check the authenticator against a metadata service; any authenticator that passes the ceremony is accepted. No enterprise attestation.
- The authenticator stores the account email as its user name (shown in the OS picker).
- No cross-origin (iframe) ceremonies and no related origins.
- Credentials are discoverable only (`residentKey: required`); non-resident security keys cannot register.
- Single process per `pb_data`, like the other auxiliary-DB modules.
