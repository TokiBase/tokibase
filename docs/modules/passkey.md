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
| `POST register/options` | record of `{collection}` | `PublicKeyCredentialCreationOptions` (JSON form), `residentKey: required`, `userVerification: preferred`, existing credentials excluded |
| `POST register/verify` | same | body `{credential, name}` (`name` max 64, default `Passkey`); stores the passkey, returns its view |
| `GET` | same | `{items: [...]}` own passkeys (`id`, `name`, `created`, `lastUsed`, `aaguid`, `transports`, `backupEligible`, `backupState`, `cloneSuspected`; never key material) |
| `DELETE {id}` | same | delete an own passkey (404 for anyone else's), 204 |
| `POST login/options` | none | body optional `{identity}`; `PublicKeyCredentialRequestOptions` |
| `POST login/verify` | none | body `{credential, mfaId?}`; returns `{token, record, meta}` |

At most 20 passkeys per record.

`login/options` with an `identity` (an identity field of the collection's password auth, usually email) restricts `allowCredentials` to that record's passkeys. An unknown identity, or one without passkeys, gets a discoverable ceremony instead, so the response does not reveal whether the account exists; an identity that does have passkeys does list its credential ids (accepted trade-off of identifier-first flows). Without `identity` the ceremony is discoverable: the authenticator offers the user's passkeys and the user handle (the record id) selects the record.

Challenges live in `_passkey_challenges` (`auxiliary.db`), keyed by the challenge value, valid 5 minutes, single use (consumed atomically on verify, even when verification then fails), bound to the collection and, for registration, to the user who requested them. Expired rows are purged on each new challenge; more than 50000 pending challenges answer 429.

## Login semantics

`login/verify` validates the assertion (origin, RP ID hash, signature, user verification flags), then calls `apis.RecordAuthResponse(e, record, "passkey", {"passkey": "<id>"})`. That gives, without extra code:

- `authRule` of the collection (a false rule answers 403, as for password login),
- MFA: with MFA enabled the response is `401 {mfaId}`; finish with another method (for example `auth-with-password` plus `mfaId`) or repeat `login/verify` with `mfaId` after a different first factor. The `_mfas` method is `passkey`. Upstream validation still requires two of password/OTP/OAuth2 to be enabled to turn MFA on (passkey is not counted),
- `OnRecordAuthRequest` hooks with `AuthMethod == "passkey"`, the sessions module (`sid`), superuser IP allow-list, login alerts.

`@request.context` is the default value (no `passkey` context exists upstream).

Any verification failure answers `400 Failed to authenticate.` (details only in the server log).

## Clone detection

If the authenticator's counter does not increase (both values non-zero; authenticators that always report 0, such as Apple passkeys, are never flagged), the passkey gets `clone_suspected=true`, its stored counter is kept, a `Warn` log is written and the audit action `auth.passkey_clone_suspected` is recorded. With `warn` (default) the login proceeds; with `deny` it answers 400 and counts as a lockout failure.

## Lockout and audit

- Lockout: `passkey.SetFailureSink(lockout.RecordFailure)` in `tokibase.go`. A failed assertion whose credential resolved to a record counts one failure under key `<collection>:<record id>` (same key as OTP). Unknown credentials cannot be attributed and are not counted. The passkey flow itself is not blocked by an existing lock (a signature cannot be guessed).
- Audit (`passkey.SetAuditSink`, needs `TOKI_AUDIT` on): `auth.passkey_register`, `auth.passkey_delete` (`by` owner or cli), `auth.passkey_login` (`method: passkey`, `mfa`), `auth.passkey_clone_suspected`.

## Storage

Collection `_passkeys` (main DB, system, all rules null so only superusers read it): `collection` (collection id), `record` (user id), `credential_id` (base64url, unique), `public_key` (base64url, hidden), `aaguid`, `sign_count`, `transports`, `backup_eligible`, `backup_state`, `clone_suspected`, `name`, `last_used`, `created`. Deleting an auth record deletes its passkeys.

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
- No cross-origin (iframe) ceremonies and no related origins.
- Credentials are discoverable only (`residentKey: required`); non-resident security keys cannot register.
- Single process per `pb_data`, like the other auxiliary-DB modules.
