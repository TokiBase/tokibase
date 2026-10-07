# push

Push notifications to FCM (HTTP v1) and APNs (token based auth, HTTP/2) with a device registry, topics and delivery through the kernel job queue (`modules/jobs`, job kind `push.send`). Disable with `TOKI_PUSH=off` (collections are then not created; existing ones stay).

Requires the jobs module for delivery: without `modules/jobs`, `Send` fails with `kernel.ErrNoJobQueue`.

## Setup

### FCM

1. Firebase console, Project settings, Service accounts, "Generate new private key". Keep the JSON file out of git.
2. Give the server the JSON: `TOKI_PUSH_FCM_SA_FILE=/etc/toki/fcm-sa.json`, or the base64 of the file in `TOKI_PUSH_FCM_SA_B64` (use this for containers; `base64 -w0 sa.json`).
3. Run `toki push test`; it prints `fcm: configured`.

The module signs a JWT (RS256, scope `firebase.messaging`), exchanges it at the service account `token_uri` and caches the OAuth2 access token until one minute before it expires. A 401 from FCM drops the cache and retries.

### APNs

1. Apple Developer, Certificates, Identifiers & Profiles, Keys: create a key with "Apple Push Notifications service (APNs)" and download the `.p8` (only once).
2. Note the Key ID and your Team ID. The topic is the app bundle id.
3. Set `TOKI_PUSH_APNS_KEY_FILE=/etc/toki/AuthKey_ABC123.p8`, `TOKI_PUSH_APNS_KEY_ID`, `TOKI_PUSH_APNS_TEAM_ID`, `TOKI_PUSH_APNS_TOPIC=com.example.app` (the default topic; a device that registered an `app_id` is sent with that bundle id as `apns-topic` instead, so several apps of one team work with one key).
4. `TOKI_PUSH_APNS_SANDBOX=1` targets `api.sandbox.push.apple.com` (development builds); default is `api.push.apple.com`.
5. `toki push test` prints `apns: configured`.

The ES256 provider token is cached for 50 minutes (Apple accepts 20 to 60). Requests use HTTP/2 (negotiated by Go via ALPN).

| Env | Meaning |
| --- | --- |
| `TOKI_PUSH` | `off` disables the module |
| `TOKI_PUSH_FCM_SA_FILE` / `TOKI_PUSH_FCM_SA_B64` | Google service account JSON (file wins) |
| `TOKI_PUSH_APNS_KEY_FILE`, `_KEY_ID`, `_TEAM_ID`, `_TOPIC`, `_SANDBOX` | APNs token auth |
| `TOKI_PUSH_MAX_FANOUT` | most devices one send may target (default 100000) |
| `TOKI_PUSH_APP_IDS` | optional comma allowlist of `app_id` values accepted at registration |

## Collections

All system collections in `data.db`, API rules `null` (superuser only). Users reach them only through `/api/push/*`.

- `_push_devices`: `collection` (auth collection id), `record`, `token` (unique), `platform` (`fcm`|`apns`), `app_id`, `locale`, `last_seen`, `enabled`, `created`, `updated`.
- `_push_topics`: `name` (unique, `[A-Za-z0-9_.-]`, max 64), `description`, `visibility` (`public`|`private`; empty or missing means private, existing topics stay private after an upgrade).
- `_push_subscriptions`: `device`, `topic` (relations with cascade delete, unique pair).

Deleting an auth record deletes its devices (and so their subscriptions).

## Endpoints

| Method and path | Auth | Body | Result |
| --- | --- | --- | --- |
| `POST /api/push/devices` | any auth record | `token`, `platform`, `app_id`, `locale` | upsert own device, `200` |
| `DELETE /api/push/devices` | owner | `{token}` JSON body, or header `X-Push-Token` | `204`; `404` for a token that is not yours |
| `DELETE /api/push/devices/{token}` | owner | | same; the token is in the URL and therefore in request logs (`_logs`), prefer the body or header form |
| `POST /api/push/subscribe` | owner | `token`, `topic` | `200`; `404` unknown topic or not your device |
| `POST /api/push/unsubscribe` | owner | `token`, `topic` | `200` |
| `GET /api/push/topics` | any auth record | | `{items:[{name,description,visibility}]}`, public topics only |
| `POST /api/push/send` | superuser | see below | `{queued: n}` |

A token registered by a second user (shared phone) is re-assigned to that user and loses its subscriptions. Other users' tokens are never revealed: they answer `404`. Topics are created by a superuser (`toki push topic add <name> [--public]`, or a record in `_push_topics`); users cannot create topics. Users can list and subscribe only to `public` topics; a private topic answers `404` on subscribe, exactly like a missing one. Server-side sends (`/api/push/send`, `push.Subscribe`) work with any topic. Limits: at most 20 devices per auth record (registering a 21st evicts the least recently seen) and 100 subscriptions per device (`429`). `app_id` must look like a bundle/package id (`[A-Za-z0-9._-]`, alphanumeric at both ends, max 200).

Apps must call `DELETE /api/push/devices` before logging a user out: there is no logout hook, so until another user registers the same token the old owner keeps receiving its notifications.

`POST /api/push/send`:

```json
{"to": {"users": [{"collection": "users", "id": "abc"}], "topics": ["news"], "tokens": ["..."]},
 "title": "Hello", "body": "World", "data": {"screen": "inbox"},
 "ttl_seconds": 3600, "collapse_key": "inbox", "priority": "high"}
```

`data` values are stringified without loss: numbers keep their digits (`1000000`, not `1e+06`; integers above 2^53 are kept as written), booleans are `true`/`false`, objects and arrays are JSON. `ttl_seconds` is at most 2419200 (28 days).

Recipients of all three selectors are merged and de-duplicated; only enabled registered devices count (an unknown token is skipped). `title` or `body` is required. One job per device is queued.

## Payload mapping

| Field | FCM v1 | APNs |
| --- | --- | --- |
| `title`, `body` | `message.notification` | `aps.alert` |
| `data` (values stringified) | `message.data` | top-level keys next to `aps` (`aps` is reserved) |
| `ttl_seconds` | `android.ttl` | `apns-expiration` (now + ttl) |
| `collapse_key` | `android.collapse_key`, `apns-collapse-id` | `apns-collapse-id` (max 64 bytes) |
| `priority` high / normal | `android.priority` HIGH / NORMAL, `apns-priority` 10 / 5 | `apns-priority` 10 / 5 |

## Retries and invalid tokens

The handler returns an error for retryable failures (HTTP 429, 5xx, FCM/APNs auth refresh, network errors, missing provider config, provider configuration errors), so the job queue backs off (5 s doubling, up to 8 attempts, then dead-letter, see `toki jobs`). The provider's `Retry-After` is not honored beyond that backoff.

Which answers disable a device (`enabled=false`, job succeeds, audit `push.device.disabled`; the device registers again on the next app start):

| Provider | Disables the device | Configuration error (retried, device kept) | Other |
| --- | --- | --- | --- |
| FCM | `UNREGISTERED`; `INVALID_ARGUMENT` whose field violation is `message.token` | `NOT_FOUND` / 404 without `UNREGISTERED` (wrong `project_id`, FCM API off), 403 / `PERMISSION_DENIED`, `SENDER_ID_MISMATCH`, oauth token endpoint 4xx | other 4xx: logged, dropped, device kept |
| APNs | 410, `Unregistered`, `BadDeviceToken` | `DeviceTokenNotForTopic`, `BadTopic`, `MissingTopic`, `TopicDisallowed`, `InvalidProviderToken`, `BadCertificate*`, `Forbidden` | `ExpiredProviderToken` and 429/5xx retry; other 4xx dropped |

A configuration error is logged at `Error` level and audited as `push.provider_error` (platform and message, at most once a minute per message), and the jobs stay visible and replayable after the operator fixes the config. Missing or misconfigured provider credentials are audited the same way. APNs `BadDeviceToken` also appears when `TOKI_PUSH_APNS_SANDBOX` does not match the build, so there is a circuit breaker: after 200 disables within a minute the module stops disabling, audits `push.provider_error` and retries those jobs; check sandbox/production and credentials.

- Re-registration race: a disable caused by a job only applies when the device's `last_seen` is not newer than the job's enqueue time, so an app that re-registered while the job was in flight keeps its fresh registration.
- TTL: `ttl_seconds` becomes an absolute deadline at enqueue (`expires_at` in the job). Retries send the remaining seconds, and a job past its deadline is dropped (OTP codes never arrive late).
- Provider errors never contain the device token: URLs of failed APNs requests are stripped to the underlying network error and token strings are masked before an error is stored in the job `last_error` or logged. FCM keeps the token in the request body only.
- The APNs provider token is refreshed at most every 20 minutes (Apple answers `TooManyProviderTokenUpdates` otherwise). The FCM OAuth assertion is backdated 30 s to tolerate clock skew.

Delivery is at-least-once: a retry after a lost response can show a notification twice; use `collapse_key` for idempotent client display.

Message content (title, body, data) of queued notifications is kept in plain text in the job table of `auxiliary.db` (one copy per device) until the queue prunes it and `toki jobs` can show it. Do not send secrets (OTP codes, balances) in push content if that is not acceptable.

## Go API

```go
n, err := push.SendToUser(app, "users", userID, push.Notification{Title: "Hi", Body: "..."})
n, err := push.Send(app, push.Message{To: push.Target{Topics: []string{"news"}}, Notification: push.Notification{Title: "x"}})
```

`(*push.Module).SetProvider(platform, provider)` replaces a provider (tests use `push.NewFake`). JS hook bindings arrive with the jsvm phase.

## Audit

`push.SetAuditSink` (wired to the audit log in `tokibase.go`): `push.send` with counts and the source (`endpoint`, `cli`, `api`; never tokens or content), and `push.device.disabled` with platform and reason, and `push.provider_error` for provider configuration/credential failures.

## CLI

```
toki push devices [--user col:id] [--json]     tokens are shortened
toki push send (--to-user col:id | --topic t | --token x) [--title ..] [--body ..] [--data '{"k":"v"}']
toki push test                                 provider config status + dry run on the fake provider (no network)
toki push prune [--days 90]                    delete devices not seen for N days
toki push topic add <name> [--description ..] [--public] | list      list prints name, visibility, description
```

`send` only queues jobs; a running server (or worker role) delivers them. `last_seen` is refreshed on every `POST /api/push/devices`, so clients should call it at app start.

## Limits

- No scheduling of silent/background pushes and no per-user quiet hours yet.
- No batch API: one request per device (FCM HTTP v1 has no multicast); a send is capped by `TOKI_PUSH_MAX_FANOUT`.
- Topics are TokiBase topics (one job per subscribed device), not FCM server topics.
- Provider hosts are fixed (Google, Apple), so no SSRF guard is needed; the service account `token_uri` comes from trusted operator config.
- Device tokens are stored in clear in `_push_devices` (needed to send); the collection is superuser only.
