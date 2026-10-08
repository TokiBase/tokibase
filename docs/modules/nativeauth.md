# Module `nativeauth`

Sign in with an ID token that a mobile app got natively (`google_sign_in`, `sign_in_with_apple`). PocketBase's OAuth2 is browser/redirect based (`authWithOAuth2Code`, `/api/oauth2-redirect`); this endpoint verifies the token on the server and maps it to the same `_externalAuths` record the code flow would create, so both flows lead to one account. Package `modules/nativeauth`, reuses `github.com/golang-jwt/jwt/v5`, no new dependency.

## Endpoint

`POST /api/collections/{collection}/auth-with-native`

```json
{"provider": "google", "idToken": "eyJ...", "nonce": "optional", "createData": {"name": "optional"}}
```

Response: the standard auth response (`token`, `record`, `meta`). `meta` has the OAuth2 fields (`id`, `name`, `email`, `avatarURL`, `rawUser`, `expiry`) plus `meta.isNew`. `rawUser` is a subset of the claims: `sub`, `email`, `email_verified`, `name`, `picture`.

## Configuration

The collection needs `oauth2.enabled` and the provider (`google` and/or `apple`) in its OAuth2 providers, exactly as for the browser flow. The accepted token audiences are **per collection**: that provider's `clientId` on that collection, plus optional extra audiences (the Android/iOS client ids, or the Apple bundle id) given in one of two places:

- the provider config of the collection: `extra.audiences` (comma separated string or list);
- the env var `TOKI_NATIVEAUTH_<PROVIDER>_AUDIENCES_<COLLECTION>`, for example `TOKI_NATIVEAUTH_GOOGLE_AUDIENCES_USERS=android-cid,ios-cid` (collection name upper-cased, other characters become `_`).

A token minted for one app is therefore never accepted by another collection that enables the same provider (for example a staff collection). The former global `TOKI_NATIVEAUTH_GOOGLE_AUDIENCES` / `TOKI_NATIVEAUTH_APPLE_AUDIENCES` variables are ignored (a warning is logged at start).

| Env | Meaning |
| --- | --- |
| `TOKI_NATIVEAUTH=off` | Do not register the endpoint (404). |
| `TOKI_NATIVEAUTH_GOOGLE_AUDIENCES_<COLLECTION>` | Extra accepted Google `aud` values for that collection. |
| `TOKI_NATIVEAUTH_APPLE_AUDIENCES_<COLLECTION>` | Extra accepted Apple `aud` values (bundle id for native iOS) for that collection. |

Build tag `no_nativeauth` removes the module (stub). All profiles include it.

Rate limiting: 20 failed attempts per minute per client address answer `429` (`Retry-After: 60`). The address is `RealIP()`, so configure the trusted proxy headers correctly; behind a proxy without that setting every user shares the proxy address (one noisy client can then lock out everyone), and with a wrong header setting a client could choose its own key. IPv6 clients are keyed by their /64. When the table is full the oldest 10% of keys are dropped (never the whole table). The check and the count are not atomic, so the limit is approximate under concurrency; successful sign-ins are not limited.

## Verification

- Signature RS256 or ES256 (P-256) only, key chosen by `kid` from `https://www.googleapis.com/oauth2/v3/certs` / `https://appleid.apple.com/auth/keys`. Cached; a refresh happens for an unknown `kid` or after 6 h, at most once per minute, 5 s timeout, outside any lock and shared by concurrent requests. If refreshing keeps failing, cached keys are served for at most 24 h, then verification fails closed. This is the only network access.
- The token must be in canonical compact form: three non-empty unpadded base64url segments with zero trailing bits (a token with a re-encoded or padded signature segment is refused), so one token has one string form.
- `iss`: Google `accounts.google.com` or `https://accounts.google.com`; Apple `https://appleid.apple.com`. `aud` must match (see above).
- `exp` required, `iat`/`nbf` checked, 60 s clock skew.
- Nonce: the request `nonce` is the RAW value the app generated, and it is compared per provider. Google: the token `nonce` claim must equal it. Apple: the claim must be the lowercase hex SHA-256 of it (pass that hash to `sign_in_with_apple`). The request nonce is never accepted just because it equals the claim, so a stolen Apple token cannot be replayed by copying its payload claim. For Google the claim is the raw value, so there the nonce only binds the token to the app instance that created it. The nonce is a client binding, not a server-issued challenge. Apple tokens with `nonce_supported: false` are exempt. A token that carries a nonce while none was submitted is refused.
- Google `email_verified` is honoured: an unverified email is never used to match or create a record. Apple emails count as verified; an Apple private relay address (`...@privaterelay.appleid.com`, `is_private_email`) is stored as the account email like any other (it is stable per app; mail to it works only for sender domains registered with Apple).
- Replay: a token (`jti`, else the SHA-256 of its signed `header.payload`, so a malleable signature cannot create a second key) is accepted once until `exp`. The cache is a bounded LRU (20 000 entries) kept per process: when it is full the oldest entry is evicted even if unexpired, and a restart or a second instance (replica) starts empty. A sign-in that fails after verification (for example invalid `createData`) does not consume the token.

## Account mapping

Identical to `authWithOAuth2`: look up `_externalAuths` by (collection, provider, `sub`); else match an existing record by the verified email; else create one through the collection create rule with `createData` (superusers cannot sign up this way, but an existing `_superusers` record can sign in here when its OAuth2 is enabled, exactly as with the browser flow). Unverified pre-existing records get the same anti pre-hijacking treatment. The `OnRecordAuthWithOAuth2Request` hook fires, so existing hooks keep working. If MFA is enabled on the collection the response is the usual `401 {mfaId}`.

Failures answer `400 Failed to authenticate.` (a locked record too). They go to the lockout counter when the token verified and belongs to a record (linked `sub`, or found by verified email), except replay and nonce failures, which never count against the account owner, to the denylog like any 4xx, to the audit log as `auth.native_failed`, and 20 failed attempts per minute per IP answer `429`. Success is audited as `auth.native`.

## Flutter

```dart
final g = await GoogleSignIn(scopes: ['email'], serverClientId: '<web client id>').signIn();
final idToken = (await g!.authentication).idToken!;
final auth = await pb.send('/api/collections/users/auth-with-native',
    method: 'POST', body: {'provider': 'google', 'idToken': idToken});
pb.authStore.save(auth['token'], RecordModel.fromJson(auth['record']));

// Apple
final raw = generateNonce(); // random string
final cred = await SignInWithApple.getAppleIDCredential(
    scopes: [AppleIDAuthorizationScopes.email, AppleIDAuthorizationScopes.fullName],
    nonce: sha256.convert(utf8.encode(raw)).toString());
final auth = await pb.send('/api/collections/users/auth-with-native',
    method: 'POST',
    body: {'provider': 'apple', 'idToken': cred.identityToken, 'nonce': raw,
           'createData': {'name': '${cred.givenName ?? ''} ${cred.familyName ?? ''}'.trim()}});
```

Apple sends the name only on the first authorization, so pass it as `createData` (or map it yourself). With `serverClientId` set to the collection's Google client id, the token `aud` matches without extra audiences.
