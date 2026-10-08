# Module `nativeauth`

Sign in with an ID token that a mobile app got natively (`google_sign_in`, `sign_in_with_apple`). PocketBase's OAuth2 is browser/redirect based (`authWithOAuth2Code`, `/api/oauth2-redirect`); this endpoint verifies the token on the server and maps it to the same `_externalAuths` record the code flow would create, so both flows lead to one account. Package `modules/nativeauth`, reuses `github.com/golang-jwt/jwt/v5`, no new dependency.

## Endpoint

`POST /api/collections/{collection}/auth-with-native`

```json
{"provider": "google", "idToken": "eyJ...", "nonce": "optional", "createData": {"name": "optional"}}
```

Response: the standard auth response (`token`, `record`, `meta`). `meta` has the OAuth2 fields (`id`, `name`, `email`, `avatarURL`, `rawUser`, `expiry`) plus `meta.isNew`. `rawUser` is a subset of the claims: `sub`, `email`, `email_verified`, `name`, `picture`.

## Configuration

The collection needs `oauth2.enabled` and the provider (`google` and/or `apple`) in its OAuth2 providers, exactly as for the browser flow. The accepted token audience is that provider's `clientId` plus the optional extra audiences below (use them for the Android/iOS client ids, or the Apple bundle id).

| Env | Meaning |
| --- | --- |
| `TOKI_NATIVEAUTH=off` | Do not register the endpoint (404). |
| `TOKI_NATIVEAUTH_GOOGLE_AUDIENCES` | Extra accepted Google `aud` values, comma separated. |
| `TOKI_NATIVEAUTH_APPLE_AUDIENCES` | Extra accepted Apple `aud` values (bundle id for native iOS). |

Build tag `no_nativeauth` removes the module (stub; the boot guard refuses the two audience env vars in such a build). All profiles include it.

## Verification

- Signature RS256 or ES256 (P-256) only, key chosen by `kid` from `https://www.googleapis.com/oauth2/v3/certs` / `https://appleid.apple.com/auth/keys`. Cached; a refresh happens for an unknown `kid` or after 6 h, at most once per minute, 5 s timeout. This is the only network access.
- `iss`: Google `accounts.google.com` or `https://accounts.google.com`; Apple `https://appleid.apple.com`. `aud` must match (see above).
- `exp` required, `iat`/`nbf` checked, 60 s clock skew.
- Nonce: when `nonce` is sent, the token claim must equal it or its hex SHA-256 (the usual Apple pattern with `sign_in_with_apple`); Apple tokens with `nonce_supported: false` are exempt. A token that carries a nonce while none was submitted is refused.
- Google `email_verified` is honoured: an unverified email is never used to match or create a record. Apple emails count as verified.
- Replay: a token (`jti`, else its SHA-256) is accepted once until `exp` (bounded LRU, 20 000 entries, per process). A sign-in that fails after verification (for example invalid `createData`) does not consume the token.

## Account mapping

Identical to `authWithOAuth2`: look up `_externalAuths` by (collection, provider, `sub`); else match an existing record by the verified email; else create one through the collection create rule with `createData` (superusers cannot sign up this way). Unverified pre-existing records get the same anti pre-hijacking treatment. The `OnRecordAuthWithOAuth2Request` hook fires, so existing hooks keep working. If MFA is enabled on the collection the response is the usual `401 {mfaId}`.

Failures answer `400 Failed to authenticate.` (a locked record too). They go to the lockout counter when the `sub` is already linked to a record, to the denylog like any 4xx, to the audit log as `auth.native_failed`, and 20 failed attempts per minute per IP answer `429`. Success is audited as `auth.native`.

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
