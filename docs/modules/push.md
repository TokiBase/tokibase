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
3. Set `TOKI_PUSH_APNS_KEY_FILE=/etc/toki/AuthKey_ABC123.p8`, `TOKI_PUSH_APNS_KEY_ID`, `TOKI_PUSH_APNS_TEAM_ID`, `TOKI_PUSH_APNS_TOPIC=com.example.app`.
4. `TOKI_PUSH_APNS_SANDBOX=1` targets `api.sandbox.push.apple.com` (development builds); default is `api.push.apple.com`.
5. `toki push test` prints `apns: configured`.

The ES256 provider token is cached for 50 minutes (Apple accepts 20 to 60). Requests use HTTP/2 (negotiated by Go via ALPN).

| Env | Meaning |
| --- | --- |
| `TOKI_PUSH` | `off` disables the module |
| `TOKI_PUSH_FCM_SA_FILE` / `TOKI_PUSH_FCM_SA_B64` | Google service account JSON (file wins) |
| `TOKI_PUSH_APNS_KEY_FILE`, `_KEY_ID`, `_TEAM_ID`, `_TOPIC`, `_SANDBOX` | APNs token auth |
| `TOKI_PUSH_MAX_FANOUT` | most devices one send may target (default 100000) |

## Collections

All system collections in `data.db`, API rules `null` (superuser only). Users reach them only through `/api/push/*`.

- `_push_devices`: `collection` (auth collection id), `record`, `token` (unique), `platform` (`fcm`|`apns`), `app_id`, `locale`, `last_seen`, `enabled`, `created`, `updated`.
- `_push_topics`: `name` (unique, `[A-Za-z0-9_.-]`, max 64), `description`.
- `_push_subscriptions`: `device`, `topic` (relations with cascade delete, unique pair).

Deleting an auth record deletes its devices (and so their subscriptions).

## Endpoints

| Method and path | Auth | Body | Result |
| --- | --- | --- | --- |
| `POST /api/push/devices` | any auth record | `token`, `platform`, `app_id`, `locale` | upsert own device, `200` |
| `DELETE /api/push/devices/{token}` | owner | | `204`; `404` for a token that is not yours |
| `POST /api/push/subscribe` | owner | `token`, `topic` | `200`; `404` unknown topic or not your device |
| `POST /api/push/unsubscribe` | owner | `token`, `topic` | `200` |
| `GET /api/push/topics` | any auth record | | `{items:[{name,description}]}` |
| `POST /api/push/send` | superuser | see below | `{queued: n}` |

A token registered by a second user (shared phone) is re-assigned to that user and loses its subscriptions. Other users' tokens are never revealed: they answer `404`. Topics are created by a superuser (`toki push topic add`, or a record in `_push_topics`); users cannot create topics.

`POST /api/push/send`:

```json
{"to": {"users": [{"collection": "users", "id": "abc"}], "topics": ["news"], "tokens": ["..."]},
 "title": "Hello", "body": "World", "data": {"screen": "inbox"},
 "ttl_seconds": 3600, "collapse_key": "inbox", "priority": "high"}
```

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

The handler returns an error for retryable failures (HTTP 429, 5xx, FCM/APNs auth refresh, network errors, missing provider config), so the job queue backs off (5 s doubling, up to 8 attempts, then dead-letter, see `toki jobs`). The provider's `Retry-After` is not honored beyond that backoff.

- FCM `UNREGISTERED` / `NOT_FOUND`, APNs 410 / `BadDeviceToken` / `Unregistered` / `DeviceTokenNotForTopic`: the device is disabled (`enabled=false`), the job succeeds, audit action `push.device.disabled`. The device registers again on the next app start through `POST /api/push/devices`.
- Other 4xx (bad payload, wrong topic): logged, not retried, device kept.

Delivery is at-least-once: a retry after a lost response can show a notification twice; use `collapse_key` for idempotent client display.

## Go API

```go
n, err := push.SendToUser(app, "users", userID, push.Notification{Title: "Hi", Body: "..."})
n, err := push.Send(app, push.Message{To: push.Target{Topics: []string{"news"}}, Notification: push.Notification{Title: "x"}})
```

`(*push.Module).SetProvider(platform, provider)` replaces a provider (tests use `push.NewFake`). JS hook bindings arrive with the jsvm phase.

## Audit

`push.SetAuditSink` (wired to the audit log in `tokibase.go`): `push.send` with counts and the source (`endpoint`, `cli`, `api`; never tokens or content), and `push.device.disabled` with platform and reason.

## CLI

```
toki push devices [--user col:id] [--json]     tokens are shortened
toki push send (--to-user col:id | --topic t | --token x) [--title ..] [--body ..] [--data '{"k":"v"}']
toki push test                                 provider config status + dry run on the fake provider (no network)
toki push prune [--days 90]                    delete devices not seen for N days
toki push topic add <name> [--description ..] | list
```

`send` only queues jobs; a running server (or worker role) delivers them. `last_seen` is refreshed on every `POST /api/push/devices`, so clients should call it at app start.

## Limits

- No scheduling of silent/background pushes and no per-user quiet hours yet.
- No batch API: one request per device (FCM HTTP v1 has no multicast); a send is capped by `TOKI_PUSH_MAX_FANOUT`.
- Topics are TokiBase topics (one job per subscribed device), not FCM server topics.
- Provider hosts are fixed (Google, Apple), so no SSRF guard is needed; the service account `token_uri` comes from trusted operator config.
- Device tokens are stored in clear in `_push_devices` (needed to send); the collection is superuser only.
