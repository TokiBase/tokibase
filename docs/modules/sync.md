# Module `sync`

Phase 3 hub/spoke replication (offline-first). The full design is `docs/SYNC_DESIGN.md`; this page describes what exists today. Package `modules/sync`, subpackage `modules/sync/hlc`.

**Status: PR7 of 11 (snapshot bootstrap, hub epoch; policies, partitions, purge, compaction since PR6; conflict strategies since PR5; rule re-evaluation and actor grants since PR4).** A node with `TOKI_SYNC_ROLE=hub|spoke` records every write of the synced collections into a local change log (PR1). Since PR2 a hub can enroll devices and authenticate them (`/api/sync/{enroll,handshake,ping}`). Since PR3 a spoke pushes its changes to the hub, the hub applies them (record level `lww`), and the spoke pulls everything it is missing and applies it; hub and spokes converge. Since PR4 a pushed change is replayed as the user who made it, through the record API, so collection rules, fieldperm and batchguard apply on the hub. Since PR5 the hub resolves concurrent changes by the collection's strategy (`lww`, `hub-wins`, `field-merge`, `hook`) and records them in `_sync_conflicts`. Since PR6 the policies are complete (partitions, view rule on pull), records can be purged for good, and the hub compacts its change log. Since PR7 a node that is new, stale or behind the compaction fetches a snapshot of the hub by itself and a restored or promoted hub is noticed by its spokes.

## What exists now

- `hlc`: the 64-bit hybrid logical clock (48 bit unix ms, 16 bit logical). `Clock` has `Now`, `Observe`, `ObserveBounded(remote, maxAhead)` (refuses a remote too far ahead with `ErrFutureHLC`, the hub applies the 5 minute `future_hlc` check of §3.7 with it), `SetOffset`/`Offset`/`WallNow`, an injected `now`, a spin to the next millisecond on logical overflow, and a persisted floor; the clock never decreases (a remote at the top of the 48 bit range is clamped to the int64 range of `_changes.hlc`); at boot the clock starts at `max(MAX(_changes.hlc), MAX(_sync_meta.hlc), hlc_floor)` since HLCs observed from remote nodes live only in `_sync_meta` (`hlc_floor` in `_sync_state`, written every 1000 ticks inside the capture transaction and on terminate).
- Capture hooks on `OnRecord{Create,Update,Delete}Execute` at priority `-1<<19` (outer than crypto, which binds at 0). Each hook runs the write in `RunInTransaction`, so the `_changes` row, the `_sync_meta` row and the tombstone commit or roll back together with the record. Inside the hook `e.App` is replaced by the transaction app; the kernel copies it to the model event before the database write (`syncModelEventWithRecordEvent`), the same mechanism that `kernel.onRecordDeleteExecute` uses. A test proves that a failing inner handler leaves no record and no change row.
- Atomicity and locking: the first statement of the capture transaction is a no-op write on `_sync_state`, so the SQLite write lock is taken before any SELECT. Reading first and upgrading later fails at once with `SQLITE_BUSY_SNAPSHOT` when a raw writer (`app.DB()`) committed in between, and neither `busy_timeout` nor the store's lock retry helps inside a stale snapshot. When a transaction already exists (batch, hook, user `RunInTransaction`) the record write and the change rows run in a `SAVEPOINT`: if capture fails the record write is rolled back even when the caller swallows the error and commits.
- Fail closed: a policy load error without a previous good set refuses the write (with a good set it keeps serving it and retries after 1 s); if `Init` failed on an already bootstrapped app, writes to capturable collections are refused until restart; an unknown `TOKI_SYNC_ROLE` aborts startup.
- The pre-write state is read with an extra `FindRecordById` inside the transaction (not `Original()`, which can be stale and is unreliable with `ignoreUnchangedFields`); this costs one SELECT per captured update.
- Patch diff, canonical hash, exclusions, empty-patch skip, tombstone guard, `status=local`, atomic group id (`tx`), actor from the request.
- `toki sync status [--json]`.
- Kernel seams (no behaviour change for other modules): `kernel.SyncOrigin` / `WithSyncOrigin` / `SyncOriginFrom` / `IsSyncReplica`, `kernel.RegisterDerivedField` / `UnregisterDerivedField` / `IsDerived` / `DerivedFieldsOf`, and the reserved `@request.context = "sync"` (`kernel.RequestInfoContextSync`). `modules/computed` registers its target fields as derived and unregisters them when a definition is removed.

## Identity, enrollment and handshake (PR2)

Packages: `modules/sync` (hub side, CLI), `modules/sync/proto` (wire types, ids, certificates, request signatures), `modules/sync/client` (spoke transport).

**Hub key.** At the first boot with `TOKI_SYNC_ROLE=hub` an Ed25519 key is generated and stored in `_sync_state` (`hub_key`), so a walreplica standby has the same key. `TOKI_SYNC_HUB_KEY_FILE` keeps it in a file outside the data dir instead (created 0600 in a 0700 directory, base64 of the 64 byte private key; a file readable by group or others logs a warning). `hub_id` = `"h" + base32(sha256(pub))[:14]` (lowercase, 15 chars) is stored in `_sync_state`. `epoch` is a random id in `_sync_state`. The session token key is not stored: it is `HKDF-SHA256(secret = hub key seed, info = "toki_sync/session/v1")`, so a copy of `data.db` without the key can not mint session tokens (an old `session_secret` row is deleted at boot). The hub writes its own captured changes under its hub id.

The hub **never replaces an existing identity**. Boot fails with a clear error (the hub does not start) when `hub_id` exists and the key is missing (`TOKI_SYNC_HUB_KEY_FILE` points to a file that does not exist, or the variable was removed and `hub_key` is not in `_sync_state`) or when the key does not derive the stored `hub_id`. A key file is only created for a hub that has no identity yet. Setting `TOKI_SYNC_HUB_KEY_FILE` on a hub whose key is in `_sync_state` moves it: the file is written with the same key and `hub_key` is deleted from `data.db`. A deliberate change of the hub key (`toki sync rotate-hub-key`, design §7.9) is not implemented yet. `hub_key` in `_sync_state` is a secret: the table is a plain table that no export, MCP or webhook path reads (so there is nothing to redact), but a `data.db` backup contains it unless the key file is used. Keep the key file out of the database backups.

**Spoke key.** Ed25519 plus X25519 in `<dataDir>/sync_node.key` (0600, base64 of seed || x25519 private key) or in `TOKI_SYNC_NODE_KEY` (same blob, never written to disk). A malformed `TOKI_SYNC_NODE_KEY` stops initialization (fail closed). `node_id` = `"n" + base32(sha256(ed25519_pub))[:14]`. Losing the key means a new node id: enroll again.

**Enrollment.**

1. `toki sync enroll --name gate-1 --profile edge --param branch=B12` creates a `_sync_nodes` row (`status=pending`, `enroll_hash` = sha256 of the code, `enroll_expires` = now + 24 h) and prints the code (8 groups of 4 base32 characters) once. Case and dashes are ignored when it is entered.
2. `toki sync join <hub-url> <code>` (or `client.Join`) sends `POST /api/sync/enroll` with the code and both public keys.
3. The hub compares the code hash with every pending row in constant time, claims the row atomically (`UPDATE ... WHERE status='pending' AND enroll_hash=?`, so a code works once even under concurrency), derives the node id from the key, sets `status=active`, renames the row to the node id and answers `{node_id, hub_id, hub_url, cert, hub_pub}`. A bad, expired, used or revoked code gives the identical answer `400 sync_enroll_invalid`. The hash is kept until the first handshake of the node, so a retry with the same key and code (the answer was lost) is answered again with a fresh certificate of the same serial; any other key stays refused. Name and profile are the ones the admin gave to `toki sync enroll`; the spoke can not choose them (neither at enroll nor at handshake). The node id check runs inside the transaction, so a key that is already enrolled can not take a second pending row (`sync_enroll_invalid`, the claim rolls back).
4. The spoke checks that `node_id` matches its key, `hub_id` matches `hub_pub`, and that the certificate verifies against `hub_pub` and names its key, then stores everything in `_sync_cursors` (`hub_id`, `hub_url`, `hub_pub`, `node_id`, `cert`). The hub key is trusted on first use and from then on is the only key that can sign a hub time or a renewed certificate for this node; `TOKI_SYNC_HUB_PIN` pins the TLS key of the hub.

**Device certificate.** Compact JWS, EdDSA (only EdDSA is accepted), signed by the hub key. Claims: `iss` hub id, `sub` node id, `pub` and `kx` (base64), `params` (partition params of the node), `iat`, `exp` = `iat` + 365 d, `ser` (serial, kept in `_sync_nodes.cert_serial`). The handshake renews it: when the certificate presented has less than 30 days left, the answer carries a new `cert` (same serial, new `exp`, current `params`) which the client verifies and stores in `_sync_cursors`. The serial stays, so a lost answer leaves the old certificate valid until it expires. A node that stays offline past `exp` must enroll again.

**Signed handshake** `POST /api/sync/handshake`. Headers `X-Toki-Node` (`n` + 14 base32 chars), `X-Toki-Sig-Ts` (unix ms), `X-Toki-Sig-Nonce` (`[A-Za-z0-9_-]{8,64}`), `X-Toki-Sig` = base64 `Ed25519(node_key, digest)` with

```
digest = sha256( UPPER(method) | path | host | hub_id | ts | nonce | hex(sha256(body)) )
```

`host` is the host (with a non-default port) of the hub URL the node is configured with, lower case, without `:80`/`:443` and a trailing dot (`proto.NormalizeHost`); the hub accepts the `Host` of the request or the host of Settings > Meta > App URL. `hub_id` is the hub id stored in `_sync_cursors` at enrollment. A captured request is therefore useless against any other hub or host. The client never follows redirects (any 3xx is an error), so the signed request and the enroll code are only sent to the configured URL. The hub verifies, in this order: header shape (a malformed header is `400` and costs no database work), node exists (an unknown id is `401`, counted in `Module.UnknownHandshakes()` and logged at debug level, never audited), certificate (hub signature, expiry, `sub` = node, `pub` and `ser` match the stored node), request signature, `ts` within +-5 min of hub time, then (only for a genuine signature) status `revoked` gives `403 sync_node_revoked`, `pending` gives 401, then `ts` must be higher than the node's `sig_ts_floor`, then the nonce must be unused. The nonce cache is per node (256 entries, 10 min) and in memory; `_sync_nodes.sig_ts_floor` (the highest accepted `ts`) is written by the same statement that records the handshake, so a hub restart or a failover can not replay a captured request. A node whose clock moves backwards is refused until its `ts` passes the floor. Everything else is `401 sync_unauthorized`; failures of KNOWN nodes are audited.

**Signed hub time.** Every `200` answer and the `401 ts_window` answer carry `X-Toki-Server-Time` and `X-Toki-Server-Sig` = base64 `Ed25519(hub_key, sha256("toki-sync-time|" + node + "|" + ts + "|" + nonce + "|" + server_time))`, bound to the request they answer. The client only trusts a hub time (clock correction, offset measurement) when the signature verifies with the stored `hub_pub` and `data.server_time` of the body equals the header; a `200` without a valid signature, or from another `hub_id`, is refused. An unsigned or forged 401 changes nothing.

**Write path of the handshake.** The node row is updated with a targeted `UPDATE` of `last_seen`, `clock_offset_ms`, `schema_version`, `app_version`, `sig_ts_floor`, `cert_expires` and `enroll_hash` that only matches a live node (`status` active, stale or rebootstrap), never with a whole-row `Save`. A `toki sync revoke` that runs at the same time can not be undone; the status is read again after the write and a revoked node gets `403` instead of a token. `RevokeNode` is a targeted `UPDATE ... WHERE name=? AND status!='revoked'` (the name does not change when enrollment renames the row id); when no row changes and the node is not revoked it returns an error and writes no audit entry.

**Abuse limits.** `POST /api/sync/enroll` and `/handshake` have a built-in per-IP limit (20 and 120 requests per minute per address, IPv6 per /64, `429 sync_rate_limited` with `Retry-After`) that does not depend on Settings > Rate limits. Behind a reverse proxy configure the trusted proxy headers in Settings, otherwise all clients share one address.

The answer carries `session_token` (HS256 JWT keyed by the hub `session_secret`, `typ:"toki_sync"`, `sub` node id, 15 min), `expires`, `hub_id`, `hub_epoch`, `server_time`, `clock {ok, offset_ms, max_drift_ms}`, `push_from` (`_sync_nodes.pushed_origin_seq + 1`), `params`, `policies` (the enabled `_sync_policies` rows with their `strategy`, `partition` and `crypto`), `poll_ms` and the fields that later PRs fill and PR2 returns empty: `schema {version: 0, bundles: []}` (PR8), `low_water` (PR6: highest compacted seq), `keys: []` (PR9), `reservations: []` (PR8), `rebootstrap` (stale node, cursor below `low_water`, or an epoch change the cursor does not survive; PR7), `caps` (optional hub features, `filler`). The hub updates `last_seen`, `clock_offset_ms`, `app_version` and `schema_version` of the node (not `profile`).

**Clock.** The hub computes `offset_ms = server_time - client_time` and `ok = |offset| <= TOKI_SYNC_MAX_DRIFT` (default 5 m). PR2 reports `ok` but does not enforce it (no push exists; enforcement is PR8). The spoke measures `offset = server_time - (t_send + t_recv)/2` with its raw clock, stores it in `_sync_cursors.clock_offset_ms` and `hlc.Clock.SetOffset`, and starts from the stored offset after a restart. Only an offset from a hub-signed time with `|offset| <= 7 days` is applied. When the hub refuses a `ts` outside its window, the signed hub time of that `401` is used once to correct the offset and retry; if the retry fails, the previous offset is restored.

**Node auth middleware** (for the routes of later PRs). `Authorization: Bearer <session_token>`: bad, expired or foreign token, unknown or pending node give `401 sync_unauthorized`; a revoked node gives `403 sync_node_revoked` (checked on every request, so revoking cuts live tokens). The node id is available with `sync.NodeFrom(e)`. `GET /api/sync/ping` uses it. PocketBase treats the sync token as a guest on all other routes.

**Routes** exist only with `TOKI_SYNC_ROLE=hub`. The rate-limit tags `sync:enroll`, `sync:handshake`, `sync:ping` can be used in Settings > Rate limits; they do nothing until that setting is enabled and rules with those labels exist (the built-in per-IP limit above is always on). Body limits: 16 KiB enroll, 64 KiB handshake.

**Client** (`modules/sync/client`): `New`, `Enroll`, `Join`, `Handshake`, `Ping`, `LoadCursor`, `StoreEnrollment`. https is required unless `TOKI_SYNC_INSECURE=1`, which allows http only for loopback and private network hosts (`localhost`, 127.0.0.0/8, ::1, 10/8, 172.16/12, 192.168/16, fc00::/7); `TOKI_SYNC_HUB_PIN` (sha256 of the hub certificate SPKI, hex or base64) is checked in addition to the normal chain verification; 30 s timeout per request. `Module.NewClient()` wires the module identity and HLC clock. No loop yet.

**Audit** (`sync.SetAuditSink`, wired in `tokibase.go` when the audit module is on): `sync.node.enroll` (details `stage: created|completed`), `sync.node.revoke`, `sync.handshake.failed` (details `node`, `reason`, `ip`).

**Data.** `_sync_nodes` system collection (hub only, superusers only; fields as in the design §2.4). `_sync_cursors` plain table (all roles; one row per hub; design §2.5 plus a `hub_pub` column). `_sync_state` keys: `node_id`, `hub_id`, `hub_key` (only without `TOKI_SYNC_HUB_KEY_FILE`), `epoch`. `_sync_nodes.sig_ts_floor` is the replay floor of a node.

## Push, pull, ack and the client loop (PR3)

Fields that are not synced (file, password, tokenKey, hidden, derived, policy `exclude`, and `email`/`emailVisibility`/`verified` of auth collections) are dropped from pushed patches. Model validation and unique indexes still apply (`validation_failed`, `unique_violation`).

### Hub routes (node session token required)

| Route | Behaviour |
| --- | --- |
| `POST /api/sync/push` | Up to 500 changes or 8 MiB per request (else 413 `sync_batch_too_large`); gzip request bodies are accepted. Changes must be the node's own (`<node>:<origin_seq>`), ascending and contiguous; a first change beyond `pushed_origin_seq+1` or a hole answers 409 `sync_push_gap` with `push_from`. Idempotent through the unique `(node, origin_seq)`: a processed change answers `duplicate` with the stored `hub_seq`, `hash`, `code` and `was` (the original status). Each change (or each `tx` group, all or nothing, no batchguard yet) is applied in its own transaction. Result statuses: `applied`, `merged` (the effective patch differs from the pushed one, see PR5), `superseded` (lost), `parked` (PR5, `hook` strategy: waiting for an admin, final for the ack), `duplicate`, `rejected` with a code (`policy_direction`, `future_hlc`, `tombstoned`, `legal_tombstone`, `orphaned`, `validation_failed`, `unique_violation`, `rule_denied` for hook errors). `acked_through` is the contiguous `pushed_origin_seq`, advanced in the same transaction as the apply. `_sync_nodes.last_seen` is updated. The hub observes the change HLC in its clock after rejecting any HLC more than `TOKI_SYNC_MAX_DRIFT` (5 min) ahead of hub time (`future_hlc`). |
| `GET /api/sync/pull?after&limit[&wait]` | Rows with `seq` in `(after, head]`: `applied` rows, hub-local rows (see below), and `revert` rows addressed to the node. `limit` default 500, max 1000. `wait` (0-25 s) long-polls until a newer `seq` exists (woken by a notifier on every commit of a synced hub write or push batch). `after` implicitly acks (`pulled_seq`, clamped to the head). `after < low_water` answers 410 `sync_rebootstrap_required` (`low_water` is set by the compaction of PR6). Counter and set fields are sent as absolute values read from the current record; revert rows carry the current full record and the current record clock (op `d` when the record does not exist). `next` is the head when nothing is left, so a node advances past rows it cannot receive. Partition filters, `pull_view_rule` and `evict` exist since PR6 (see below), `fields` carries the field clocks of `field-merge` collections (PR5). A page stops after about 4 MiB (`more: true`, always at least one row) so that it fits the 8 MiB the client reads. Revert rows are delivered whatever the collection direction (a push-only collection must learn that its change was refused). |
| `POST /api/sync/ack` | `{"pulled_through": N, "digest": {"<collection id>": "<sha256>"}}`. Updates `pulled_seq`. A digest (sha256 over the sorted `(id, hash)` pairs of `_sync_meta`) is compared only when the node has pulled up to the hub head (`digest_checked`), because otherwise it would measure lag; differing collections come back in `digest_mismatch`. Compaction: see the PR6 section. |

### Conflicts (lww, design §4.2)

A change is concurrent when its `base` is not the record clock `_sync_meta.hlc` on the hub. A concurrent change wins when `(hlc, node)` is greater than the record clock, and only its patch fields are applied. A concurrent loser is `superseded`: nothing is written to the record, except counter/set operations, which never conflict and are always applied (result `merged`). A delete always wins, even an older one; updates of a tombstoned record are `rejected` (`tombstoned`); deleting a record that is already gone is `superseded`. Autodate columns (`created`, `updated`) keep the origin value: the autodate interceptor regenerates them on every save, so the apply paths restore them with a direct column update afterwards (and recompute the stored hash).

**Revert rows.** For every rejected change, and for every `superseded` or `merged` update, the hub writes a `_changes` row with `status=revert`, `target=<node>`, directly after the verdict row (so a `rejected` result reports the revert's `hub_seq`). The node receives the hub state of the record in its next pull. This is a deviation from the design text, which writes no revert for `superseded`: without it a node whose lost edit touched fields the winner did not touch would keep its local value forever. Rows stored by the hub: `applied` rows keep the origin node, origin_seq, hlc, base and actor and carry the effective patch (diff of the hub record before and after, so it includes the hub's resolved values); rejected and superseded verdicts are stored as `status=rejected` with `code` (`superseded` for lww losers).

**Hub-local writes** (REST clients, hooks) are captured by PR1 as `status=local` rows under the hub id. They are pulled like `applied` rows. Changes whose patch is empty (a push that changed nothing) are not delivered.

### Realtime poke

After each committed push batch and each committed write of a synced collection on the hub, the hub sends `{"seq": N}` on the realtime topic `@sync` (through `app.SubscriptionsBroker()`). The payload has no record data. Subscribing to `@sync` through `POST /api/realtime` needs `Authorization: Bearer <node session token>`; guests and other users get 403, so only enrolled nodes ever receive it (and the payload would be harmless anyway: it only tells that something changed). An online spoke keeps one SSE connection (`GET /api/realtime`, `TOKI_SYNC_POKE=0` disables it) and runs a cycle on every poke and once after every (re)subscription. Clients that cannot hold SSE use the interval timer or `Client.Pull(wait)`.

### Spoke client loop (`modules/sync/client`)

`Start`, `Stop`, `SyncNow`, `Pause`, `Resume`, `SetConditions`, `Status`, `Events`, plus `RunOnce` (one synchronous cycle) and `PullOnce`. The module starts it OnServe when `TOKI_SYNC_ROLE=spoke` and the node is enrolled, and stops it OnTerminate.

A cycle is: session (handshake when there is no valid token) -> push pages from `_changes` in origin_seq order -> pull pages -> apply -> ack.

- **Handshake effects.** The local `_sync_policies` rows become equal to the hub's list (until schema bundles in PR8, collections still have to exist on the spoke). Rows below the hub's `push_from` become `acked`; an `acked` row at or above it (hub restored) is sent again.
- **Push.** Pages of `TOKI_SYNC_PAGE` (500) rows and about 4 MiB; a `tx` group is never split. Rows are marked `pushed` before the request and `acked` up to `acked_through` after it. 409 `sync_push_gap` resends from `push_from`; 413 halves the page; 401 triggers a new handshake. Rejected and superseded results are reported on `Events()`.
- **Pull apply (design §4.7).** Per page in one transaction together with `pull_after`, with `kernel.WithSyncOrigin(Mode: Pull)` (no `_changes` row is captured, `_sync_meta` and tombstones are updated). `revert`/`c`/`u` set the fields that differ, unless a pending local change (`local`/`pushed`) on that field has a higher HLC; counters become `hub value + sum of pending $inc`, sets re-apply the pending `$add/$rm` on the hub list; `d` deletes (tombstone). Changes the node itself originated are applied too, in hub order: an older foreign row pulled after the node's own newer acked change is followed by that change again, which keeps all nodes on the hub's sequence. Equal values are not written. Every change is applied in its own savepoint. A change that cannot be applied (unique index, missing collection, ...) leaves no partial write, is written to the local `_sync_conflicts` (kind `apply_error`, once per change), counted (`Status().ApplyErrors`) and reported as an `error` event; the page STOPS there: the changes before it stay applied, `pull_after` stops just before it, and the cycle fails with `*client.ApplyError` (shown as the last error in `toki sync status`) so that it is retried with the normal backoff instead of being skipped. A poison change therefore blocks the pull until the cause is fixed. Deviation: pending local changes are not dropped on a revert (design §4.7 says to drop those with `hlc <=` the rejected one): rows must stay contiguous for `push_from`, and by construction no such row exists. A revert (or hub delete) to "deleted" does discard the newer pending local edits of that record: they are marked `acked` with code `discarded`, logged at WARN, and kept in a local conflict row (kind `orphaned`) with their patches. The `hash_mismatch` check of §4.7 runs after a change is applied to a record without pending local changes (collections without counter/set fields only, because the hub sends their CURRENT absolute value with a historic hash): mismatches are counted in `Status().HashMismatches` / `HashStreak` and logged; the auto-heal re-fetch is not implemented.
- **Triggers.** `TOKI_SYNC_INTERVAL` (30 s; 5 min when `Metered` or `LowPower`), local writes (debounced 2 s, hooked on `OnRecordAfter*Success` of synced collections, not for pull applies), `@sync` pokes, `SyncNow`.
- **Backoff.** After the n-th consecutive failure the next attempt waits `1 s * 2^(n-1)` up to 5 min with +-20% jitter; a `Retry-After` header is a floor. Pokes and local writes do not break a backoff; `SyncNow` does. 403 `sync_node_revoked` stops the loop (`ErrRevoked`, state `revoked`).
- **Conditions.** `Online=false` makes no attempt at all (`SyncNow` answers `ErrOffline`). `Metered` limits the pull page to 100 and uses the long interval. `Background` and the bounded background cycle are PR10. `Pause`/`Resume` stop and resume attempts.

### `toki sync verify [--against-hub] [--json]`

For every synced collection: record count, `digest` (sha256 over the sorted `(id, canonical hash)` of the stored records; equal on every converged node), `meta_digest` (the same over `_sync_meta`, what `/ack` compares) and the records whose stored row differs from `_sync_meta` (`no_meta`, `hash`, `orphan_meta`), which is how raw SQL writes and saves without a change row show up. Exit status 1 when anything differs. `--against-hub` (spoke) sends the meta digests in an `/ack` and prints the collections the hub reports as different.

### Other notes

- Saving a record without changing any synced field still bumps its `updated` column but writes no change row (PR1 capture). Such a node then differs from the hub in `updated` and `toki sync verify` reports a `hash` mismatch for it. Apps should not save unchanged records; a fix belongs in capture (PR1 hardening).
- The hub applies pushes serially (one mutex); throughput is bounded by SQLite's single writer anyway.

## Rule re-evaluation, actors and audit (PR4)

**Replay as the original actor (design §5).** `apis.ReplayRecordRequests(ctx, app, auth, headers, reqs)` runs record create/update/delete requests as `auth` in ONE transaction through the batch processor, with RequestInfo context `sync`. A push group (one change, or the changes of a `tx` group) becomes `POST /api/collections/{c}/records` (id in the body), `PATCH .../{id}` (counters as `field+`, sets as `field+` / `field-`) or `DELETE`. A `tx` group goes through `OnBatchRequest`, so batchguard `assert`/`assert_post` run on the hub; a single change is not a batch and skips it. Zero values of a new record are not sent (a client create would not send them), so write rules of untouched fields do not fire. Rule failures map to `rule_denied` (403/404 and create/manage rule failures), validation errors to `validation_failed`, unique errors to `unique_violation`; each rejected change gets a revert row as before. `p` (purge) is never replayed: it stays hub-only.

What the rules see: `@request.auth.*` is the actor record loaded on the hub, `@request.auth.kind`, `@request.context = "sync"`, `@request.headers.x_toki_sync_node` (node id; the header is stripped from every incoming HTTP request and every `/api/batch` sub-request, so only the replay sets it and rules may rely on it, still best written as `@request.context = "sync" && ...`), `@request.method`, `@request.body.*` is the effective patch. Example: `@request.context != "sync" || @request.auth.role = "gate"`. Rate limits do not apply to replay sub-requests.

**Actor grants (design §1.6).**

1. The user logs in to the hub normally and the app calls `inst.Sync().AddActor(hubToken)` (Go: `Module.AddActor` / `client.Client.AddActor`). The spoke sends `POST /api/sync/actor` with its node session and `X-Toki-Actor-Token`.
2. The hub validates signature and `tokenKey` (`FindAuthRecordByToken`), the sessions `sid` through `kernel.SessionActive` (set by `modules/sessions`), refuses superusers (unless `TOKI_SYNC_ALLOW_SUPERUSER_ACTORS=1`), inserts `_sync_actor_grants{aid,node,collection,record,sid,tkh,iat,exp}` and answers `{aid, exp, assertion, record}`; `assertion` is an EdDSA JWS of the claims signed by the hub key, `record` is the user record without password/tokenKey/files. `TOKI_SYNC_ACTOR_TTL` (default `30d`, `d` suffix supported) is capped at `TOKI_SYNC_RETENTION` (default `90d`).
3. The spoke verifies the assertion, stores the grant in `_sync_actors`, creates the local auth record when missing (no password, fresh local tokenKey) and `LocalToken(aid)` mints a LOCAL auth token, so local rules see the same `@request.auth.id` offline. `RemoveActor(aid)` revokes on the hub (`DELETE /api/sync/actor/{aid}`) and forgets the grant.
4. Local writes made by a request of a granted user capture `actor = aid` (newest valid grant of that record on this node); everything else is `actor = "node"`.

**Hub validation per change (at apply time).** `actor_unknown` (grant or actor record missing, or a node without service actor), `actor_node_mismatch`, `actor_expired` (`grant.iat - 5 min <= change.hlc <= grant.exp` AND hub time `<= grant.exp + TOKI_SYNC_GRACE`, default `24h`: the HLC is chosen by the spoke, so it alone never keeps an expired grant alive; a device offline longer than the grace needs a new grant), `actor_revoked` (grant revoked, sessions `sid` inactive, the user's `tokenKey` changed, or node revoked), `actor_forbidden` (superuser). Changes with `actor = "node"` (writes without request auth: hooks, cron, direct app calls) run as the node's **service actor** (`toki sync enroll --actor <collection>/<id>`). A request made by an authenticated record WITHOUT a valid grant (never granted, grant expired or removed, local token still alive) is captured as `rec:<collectionId>:<id>`, never as `node`; the hub rejects it `actor_unknown` with a revert row. **Blast radius of the service actor:** every change a node pushes as `node` (and every hook-written change of a user request) runs with its rights; a superuser service actor bypasses all collection rules, fieldperm and validation rules for those changes, and whoever holds the node key can push them. `toki sync enroll --actor _superusers/<id>` therefore needs `--allow-superuser-actor` and the audit entry `sync.node.enroll` records `service_actor_kind` and a warning; prefer a dedicated low-privilege auth record. A tx group runs as ONE grant: members written by hooks (actor `node`) replay as the service actor in the same transaction (the group is refused `actor_unknown` when the node has none); two different grants in one group are rejected `actor_unknown`. A superuser of the spoke itself (admin UI, CLI token) is the operator of the node and is captured as `node`; every other auth record needs a grant. **`actor_revoked` parks** the change: `_changes.status = parked`, no revert row, a `_sync_conflicts` row (`kind actor_revoked`, `resolution parked`, `status open`) for review; the push result is `parked` and counts as final for the ack. Every other code is `rejected` with a revert row. Parking has three exits so that it is never a silent divergence: (1) on the next pull the hub sends the node an informational row `{notice: "parked", code}` without data; the node marks the record "pending review" (`client.PendingReview`, event `parked`), keeps serving its local value and clears the mark when the hub decides; (2) an operator resolves it (`toki sync conflicts --resolve`, PR5), which produces a normal applied row or a revert; (3) a parked change older than `TOKI_SYNC_PARK_TTL` (default `30d`) is rejected by an hourly hub job: row `rejected/park_expired`, a revert row for the node, the conflict closed (`resolved/rejected`) and an audit entry `sync.reject` with `stage: park_ttl`. Until one of these happens the offline work of that user exists only on the device. Because sessions rotation revokes the old `sid` on refresh, a deployment that rotates refresh tokens should keep grants short-lived or re-run `AddActor` after login. A replay that exceeds the 60 s replay timeout is retried (HTTP 500 to the node) and rejected as `apply_error` after 3 attempts, so one poison group cannot hold the apply lock of every push for ever.

**What a node may receive (P3-2, P3-10, P4-6, P4-7).** The collection view rule is evaluated for the service actor of the node on ordinary pull rows when the policy sets `pull_view_rule` (PR6), or for every collection with `TOKI_SYNC_PULL_VIEW_RULE=1` (rows of records it may not view are not delivered or evicted; a node without service actor is not filtered by the variable). Without either, ordinary rows are NOT filtered by the view rule (only fieldperm hides fields): do not rely on it for secrecy. The revert row of a record that the original actor of the rejected change may not view is a verdict without data, stored with that actor: the node receives `{notice: "invisible"}` and KEEPS its local copy (marked pending review), never an op `d`; with `TOKI_SYNC_EVICT_INVISIBLE=1` it receives the PR6 eviction (op `x`) instead, a local delete without tombstone. Revert rows only contain the fields they were stored with, so a field hidden for the original actor is not re-added at pull time. Pull rows drop fields that fieldperm hides from the node's service actor and fields no longer in the sync set. Hidden fields and the auth system fields `email`, `emailVisibility`, `verified` are not synced; opt in per field with policy `field_types {"email":"include"}`. Pull page rows of records that no longer exist are skipped (the delete row follows). `pull?after=N` with `N` above the hub head answers `410 sync_rebootstrap_required`.

**Robustness (P3-5, P3-6).** A permanent infrastructure error while applying a change no longer returns 500 forever: it is stored as `rejected` with code `apply_error` (plus a resolved `_sync_conflicts` row and a revert) so the queue advances; transient errors (locked, busy, timeout) still return 500 and are retried. A sequence at or below `pushed_origin_seq` whose `_changes` row is gone is answered `duplicate` and never applied again.

**Side effects.** Webhooks and WASM `record.after.*` handlers skip writes with `kernel.IsSyncReplica(ctx)` (pull, snapshot and bundle applies on a spoke); the hub replay fires them once. Origin `created`/`updated` are put into the record with `SetRaw` before the autodate interceptor (`kernel.SyncOrigin.Fields`, key `<collectionId>/<recordId>/<field>`) and verified after the replay.

**Audit.** Sink entries: `sync.apply` (only for superuser or service actors, or every actor with `TOKI_SYNC_AUDIT_ALL=1`), `sync.reject` (names the grant's user even when the grant did not resolve), `sync.actor.grant`, `sync.actor.revoke`. `actor_kind/actor_id/actor_collection` are the original actor and `request` is `{path, ip, node, change, hlc}`.

**Env.** `TOKI_SYNC_ACTOR_TTL`, `TOKI_SYNC_RETENTION`, `TOKI_SYNC_ALLOW_SUPERUSER_ACTORS`, `TOKI_SYNC_AUDIT_ALL`. **Migration for PR3 users:** enroll nodes with `--actor`; a node without a service actor is rejected `actor_unknown`.

## Conflict strategies, typed fields and `_sync_conflicts` (PR5)

The hub decides every concurrent change (the writer's `base` is not the record clock `_sync_meta.hlc`) by the `strategy` of the collection's policy. `resolve.go` holds the pure resolver (`Resolve`, no database), `hub_resolve.go` connects it to the apply pipeline, `types.go` the counter/set handling, `conflicts*.go` the collection and the CLI. A change that saw the latest hub version is never a conflict.

| Strategy | Concurrent change | Conflict row |
| --- | --- | --- |
| `lww` (default) | the greater `(hlc, node)` wins; the loser is `superseded` (nothing written, its counter/set operations still apply, result `merged`) | `concurrent_field`, `auto_lww`, `resolved`, only when the incoming change lost |
| `hub-wins` | rejected with `hub_wins`, nothing applied (not even counter/set operations), the node gets a revert | `hub_wins`, `reverted`, `resolved` |
| `field-merge` | per plain field: applied when its clock `fields[f] <= base`, else the greater HLC wins (tie: node id). Dropped fields make the result `merged` (revert to the node); nothing left is `superseded` | one `concurrent_field` row per change, `auto_merge`, `resolved`, or `open` when the policy sets `review: true` (the merge is applied either way) |
| `hook` | the wasm module of the policy decides (below) | see below |

**Field clocks.** `_sync_meta.fields` is `{"field": "<hlc hex>"}` and exists only for `field-merge` collections: pushes set the clock of every plain field they write, and hub-local writes (REST, hooks) do too, so a stale push cannot overwrite them unnoticed. Counter and set fields never have a clock. A pull sends the clocks of the pulled fields in `fields`; a spoke stores them in its `_sync_meta.fields` (informational, the hub decides). Records written before the strategy was switched on have no clocks: every field counts as unchanged since the writer's base. Two clocks that are exactly equal fall back to the node id comparison with the record's last node (the exact HLC tie of two nodes is practically unreachable; the clock stores no node per field).

Worked examples of the design (§4.2, §4.4, §4.5) are table tests in `resolve_test.go`, run in every arrival order: lww `closed` 10:05 vs `disputed` 10:03; field-merge plate/fee/note (fee 5000, note set, plate `B1234X`, one conflict row recording `B1243` vs `B1234X`); counter 10+3+2+1 = 16; set `{a}` + add b / remove a add c = `{b, c}`.

**Typed fields (`field_types`).** `counter` (number) and `set` (multi select / relation). A spoke captures `{"$inc": delta}` and `{"$add": [...], "$rm": [...]}`; the hub replays them through PocketBase's own modifiers (`field+`, `field+`/`field-`), sends ABSOLUTE values on pull, and the spoke rebases them (`value = hub value + sum of pending $inc`, pending `$add/$rm` re-applied on the hub list). They never take part in field clocks and never conflict, except under `hub-wins` and a rejecting/parking hook. **The push is validated against `field_types`** (`validation_failed`): `$inc` only on a declared counter (finite number, and nothing else in the object), `$add`/`$rm` only on a declared set (arrays), no operation on any other field (a JSON field may hold `$`-keys as a plain value), and no absolute value for a counter or set unless the record is created. Otherwise a spoke could wrap a plain field in `$inc` to dodge the lww clock, or overwrite other nodes' increments with an absolute counter.

*Known set limit (no OR-set metadata in v1):* "remove x" on one node and "add x" on another leave x present only if the add arrives last at the hub. Counters are exact for integers (float64 deltas).

**`hook` strategy.** For a concurrent change in a collection with `strategy = hook` the hub emits `kernel.OnSyncConflictFor(app)` (`kernel.SyncConflictEvent`, inside the apply transaction). `modules/wasm` answers it for modules that list `sync.conflict.<collection>` or `sync.conflict.*` in `events` (the `hook` field of the policy narrows it to that one module); see [wasm](wasm.md#sync-conflict-events). Resolutions: `accept` (apply the pushed patch), `reject` (code `hook_rejected`, revert to the node), `merge` (apply the guest's patch instead, plus the pushed counter/set operations it does not mention; result `merged`), `park` (apply nothing; the change is stored with `status=parked`, the node gets result `parked`, final for the ack; conflict `open`, code `hook_parked`). Fail closed: no handler (wasm not built in, module missing or not subscribed), an error, a trap, a timeout, an invalid output or an unknown resolution park the change with code `hook_failed` and a `hook_failed` conflict row (open). Inside a `tx` group a park would split the group, so it rejects the whole group with the same code and an open conflict row. The pulled state of a parked record on the spoke stays diverged until an admin resolves it (`toki sync verify` shows it). Spokes have no wasm and no strategy: they apply `hook` collections with the local lww rule (a pending local field with a higher HLC survives a pull) until the hub decides. The policy `strategy` is not shipped to spokes.

**`_sync_conflicts`** (system collection, superusers only, indexes `(status, created)` and `(collection, record)`), fields as in the design §2.7 (`collection` holds the collection ID for rows the hub wrote; since the QC pass pushed collection names are normalised to the ID, rows written earlier, and spoke-local rows, may hold the NAME, which is why purge matches both). Written by the hub for: lww losses, field-merge concurrency, `hub-wins` rejections, hook outcomes, and (PR5 only these kinds) nothing for rule/validation rejections, which stay in `_changes` (`rejected` + `code`). Values of fields registered with `kernel.RegisterSensitiveField` are `[encrypted]` and a stored patch/record snapshot above 256 KiB is replaced by `{"_truncated": true, "fields": [...]}`; the pushed patch itself stays in `_changes`. **Spokes** keep a local, informational copy of what the hub answered to their own pushes (`merged`, `superseded`, `rejected`, `parked`), of local edits discarded by a delete/revert (`orphaned`) and of pulled changes that failed to apply (`apply_error`); `toki sync conflicts` lists it there. It is not synced and not updated when the hub resolves a parked change. Resolved spoke rows are pruned to the newest 500.

**`toki sync conflicts`.** `--open` keeps rows waiting for an admin, `--collection` filters by name or id, `--json` prints `ConflictRow` objects. `--resolve <id> --take hub|incoming|<patch.json> [--note ...]` (hub only, open conflicts only): `hub` writes nothing and sends the node a revert, `incoming` applies the stored pushed patch (for parked changes the real patch from `_changes`; for a field-merge review the fields it dropped), `<file>` applies the JSON patch in the file (use it when the row holds `[encrypted]`). The write is replayed as the ORIGINAL ACTOR of the conflict (`apis.ReplayRecordRequestsFrom`, `@request.context = "sync"`), so collection rules, fieldperm and batchguard apply exactly as for the pushed change; it is captured as an ordinary hub change and delivered to every node by pull, and the originating node additionally gets a revert. If the grant is no longer valid the node's service actor is used; without one the resolution is refused. The row becomes `resolved` with `resolved_by=cli`.

**Compaction (PR6):** `parked` rows of `_changes` are never compacted; resolved conflicts are pruned after 90 days, open ones stay.

## Policies, partitions, purge and compaction (PR6)

### `_sync_policies` (design §2.6)

System collection, superusers only, one row per collection (by name or id; two rows that resolve to the same collection are refused).

| Field | Meaning |
| --- | --- |
| `collection` | name or id of a non-system, non-view collection |
| `direction` | `both` (default), `push`, `pull`, `none`. **Auth collections can only be `pull` or `none`.** |
| `strategy` | `lww` (default), `hub-wins`, `field-merge`, `hook`. Resolved by the hub, see "Conflict strategies" above |
| `partition` | `"<field> = @node.<param>"` or empty (all records), see below |
| `field_types` | `{"fee":"counter","tags":"set","ticket_no":"reserve:tickets","email":"include"}`. `counter` needs a number field, `set` a multi select or multi relation, `reserve:<sequence>` a text or number field (accepted now, enforced with PR8) |
| `exclude` | field names that never sync (unknown names are only a lint warning) |
| `hook` | WASM module of strategy `hook` |
| `pull_view_rule` | default **true** on every creation path on the hub (REST, CLI, `app.Save`, WASM, import): a create hook sets it unless the REST body, `SetPolicy` or a save with `withExplicitViewRule` states the value; a Go caller that really wants `false` creates the row and then updates it. Lint warns when it is off for a collection with a `viewRule`: on pull a record is only sent when the collection `viewRule` allows the node's service actor |
| `trusted` | lets a collection whose `viewRule` is `null` be pulled (see below) |
| `crypto` | `ciphertext` (default) or `strip` (stored; PR9 uses it) |
| `order` | integer >= 0, apply order for the snapshot (PR7) |
| `enabled` | the row only counts when true |
| `review` | keep automatic `field-merge` conflicts open for review |

**Validation.** Every save on the hub (REST, `toki sync policies set`, Go) runs `OnRecordValidate`: the same checks as `lint`, errors only, reported as field errors (HTTP 400 over REST). A spoke does not validate: it stores the hub's rows as they come, even before the collection exists. Findings that are only warnings: unknown `exclude` field, file fields that are not excluded (files do not sync in v1), `strategy` on a `pull`/`none` policy (nothing is pushed), a typed field that is also excluded, `counter`/`set` under `hub-wins`, a partition field that is not indexed or hidden, `trusted` without a null view rule, a null view rule without `trusted`.

**Cache.** `policyCache` keeps the parsed rows for 5 s and is invalidated when a policy row or any collection changes; a failed reload keeps serving the last good set.

**CLI.**

```
toki sync policies list [--json]
toki sync policies set <collection> [--direction d] [--strategy s] [--partition "f = @node.p"] [--field-type f=counter]... [--exclude f]...
                                    [--hook h] [--crypto c] [--order n] [--enabled=bool] [--review=bool] [--trusted=bool] [--pull-view-rule=bool]
toki sync policies rm <collection>
toki sync policies lint [--json]          # exit 1 when there is an error; warnings do not fail
```

`set` creates the policy (direction `both`, `lww`, enabled, `pull_view_rule` on) or changes only the flags that are passed (`--field-type` and `--exclude` replace the whole list).

### Direction

Push: the hub refuses a change for a collection whose policy is missing, `pull` or `none` with `policy_direction` (a revert row follows). Pull: only `both` and `pull` collections are delivered (revert rows addressed to the node are delivered whatever the direction, so a refused push is undone on the node).

### Partitions (design §2.1, §3.5, §7.1)

A policy with `partition: "branch = @node.branch"` limits a node to the records whose `branch` equals the parameter `branch` of the node (`toki sync enroll --param branch=B12`, stored in `_sync_nodes.params`; the admin can change it later). The field must exist, must not be hidden, derived or an auth system field (an error: its value would not travel), be a single-value text, number, bool, email, url, select or relation field, must not be excluded and must not be a counter or set. Comparison is by the string form (`12` and `"12"` are equal). A node that has no such parameter gets nothing and may push nothing (fail closed).

- **Hub rows.** Every `_changes` row of a partitioned collection carries `part_old` and `part_new`, the key before and after the change (`""` before a create and after a delete), for hub-local writes, pushes and tx groups (the last change of a record in a group carries the stored key). `_sync_meta.part` holds the current key.
- **Pull.** The page query drops the rows of other partitions in SQL (`part_new = :p OR part_old = :p`; revert rows and purges are exempt). In the page:
  - key unchanged inside the partition: the normal row;
  - `part_old` in, `part_new` out (record left): `{"op":"x","evict":true}`, no data;
  - `part_new` in, `part_old` out (record entered): the whole current record as op `c` (an update row alone would be ignored by a node that never had the record);
  - a delete is delivered when `part_old` is in (the spoke keeps a delete tombstone as for any delete).
- **Push.** A change is refused with `policy_partition` when the record exists on the hub outside the node's partition (read, update or delete of someone else's record, also by guessing its id) or when the record would be outside it after the change (a create without the key, a key set to another value). The check uses the hub state before the change and the patch after it, per change inside a tx group. The revert row of such a refusal carries no data: a record in another partition is reported to the node as an eviction.
- **Spoke.** Op `x` deletes the local copy and removes the delete tombstone that the replica delete wrote; the record can come back later. No `_changes` row is written.

Records written before a partition was configured have `part_new = ""` and are not delivered to partitioned nodes; changing a partition means a snapshot (PR7) for the nodes concerned.

### `pull_view_rule` and `trusted` (design §7.7)

With `pull_view_rule` on, a non-revert row is sent only when the collection `viewRule` lets the node's service actor see the record (evaluated like a normal view, `@request.context = "sync"`, fieldperm included). Records that fail are omitted (create) or evicted (update). **Sent tracking:** for a collection with a restrictive view rule the hub remembers in `_sync_sent(node, collection, record)` which records it delivered, and sends evict and delete rows only for those, so a node learns nothing (id, origin node, HLC) about records it never had. Nodes that had already pulled when the table was introduced are marked `sent_legacy:<node>` in `_sync_state` and keep the old behaviour (every evict/delete is sent) until they are re-bootstrapped (PR7). An entry stays after an evict (a lost page can be re-read); compaction drops the entries of removed nodes, purge those of the record. A restrictive (non-empty) rule makes updates travel as the whole record, because a record can become visible by an update. A collection with a **null** view rule (superusers only) is never pulled unless `trusted` is true, even when the service actor is a superuser; `pull_view_rule = false` switches both checks off. Reverts always use the view rule of the actor.

Limits: visibility is evaluated when a row is sent, so a change of the rule itself, or of data the rule joins, does not retroactively evict or deliver records; the check runs per record, not as one query per collection per page.

### Purge and legal tombstones (design §7.3)

`POST /api/sync/purge` (superuser token) with `{"collection":"items","record":"<id>","reason":"...","legal":true}`, or `toki sync purge <collection> <id> --legal --reason "..."`:

1. Deletes the record (hooks and webhooks of a delete fire once; the capture hook is bypassed) and its `_sync_meta`.
2. Writes the `legal` tombstone (upgrading a `delete` tombstone). The PR1 triggers make it permanent: it can neither be updated nor deleted, and compaction never prunes it.
3. Sets `patch='{}'` and `hash=NULL` on **every** `_changes` row of the record (own, rejected, parked, reverts) and clears the JSON copies and the note in `_sync_conflicts`. Later refused pushes of the record are stored without data too.
4. Appends an op `p` row (`{"reason": ...}`). It is delivered to every node that pulls the collection, whatever its partition (the node may hold the record from earlier).

A spoke that applies `p` deletes the record, writes the legal tombstone, deletes `_sync_meta` and blanks the patches of its own `_changes` rows of the record. A push of `c` or `u` for a tombstoned record is rejected `tombstoned` (delete tombstone) or `legal_tombstone` (purge); a delete tombstone is only an obstacle until it is pruned, a legal one never. Creating the id locally fails with `validation_sync_tombstoned` on hub and spokes. A purge is idempotent (`already: true`), works for ids that never existed (tombstone in advance) and needs a reason. A purge does not reach backups, files or any copy outside the sync log.

**Purge completeness (QC).** Purge works for every existing, non-system collection, also one whose policy is `none`, disabled or removed (it is hub-local). `_changes` rows are matched by collection id AND name. On a spoke the `p` op also blanks the spoke-local `_sync_conflicts` copies (incoming, current, note) of the record. The purge runs with `PRAGMA secure_delete=ON` (freed cells are zeroed), then `PRAGMA incremental_vacuum` and `PRAGMA wal_checkpoint(TRUNCATE)`; `scrubbed: true` in the result means the checkpoint completed (readers can make it fail: run `toki sync compact --vacuum` later). **Residual data that cannot be erased retroactively**: copies in `walreplica` streams and replicas, backups (`pb_data/backups`), file system and VM snapshots, and free pages written before the purge when `secure_delete` was off. Operational procedure for a legal erasure: purge on the hub, wait until every node pulled the `p` row (`toki sync status`), run `toki sync compact --vacuum` on hub and spokes, then delete or rotate the backups and walreplica generations that predate the purge.

### Compaction (design §3.6)

Hourly cron entry `__tokiSyncCompact` -> job kind `sync.compact` through `kernel.Jobs` with `CronKey("sync.compact:<yyyymmddhh>")` (one run per hour even with several processes; without the jobs module it runs inline). `toki sync compact [--vacuum] [--json]` runs it now. Hub:

1. Active nodes with `last_seen` (or creation time) older than `TOKI_SYNC_RETENTION` become `stale` and leave the minimum.
2. `safe` = the lowest `pulled_seq` of the active nodes (0 when there is none: only the retention deletes then). Delete `_changes` with `seq <= safe` and older than `TOKI_SYNC_MIN_KEEP`, plus everything older than the retention. `parked` rows are never deleted. **`low_water` is set to the highest deleted seq** (monotonic), so a node that acknowledged exactly `safe` can still resume. `hlc_floor` is saved before the delete.
3. `delete` tombstones older than the retention are pruned (spokes do this too); `legal` never.
4. Resolved conflicts older than 90 days are deleted.

Spoke: step 3 and `status='acked'` rows older than `TOKI_SYNC_SPOKE_KEEP`.

**Consequences.** `GET /pull?after=N` answers 410 `sync_rebootstrap_required` (with `low_water`) when `N < low_water` and always for a node with status `stale` or `rebootstrap`. The handshake returns `low_water` and `rebootstrap: true` for such nodes (and for a node whose `pull_after` is below `low_water`, which includes a new node that joins a compacted hub). Since PR7 the spoke sets `_sync_cursors.state = 'rebootstrap_required'` and its loop runs the snapshot bootstrap (next section); a `stale` node is reactivated when the snapshot is complete. Pending local changes are kept and rebased.

### Health

`GET /api/health` with a superuser token adds `data.sync`: `role`, `pending` (spoke: local + pushed rows; hub: head - lowest `pulled_seq` of the active nodes), `low_water`, `head`, `stale_nodes`, and on the hub `active_nodes` and `open_conflicts`. Other callers see no change.

### Env

| Variable | Meaning |
| --- | --- |
| `TOKI_SYNC_RETENTION` | `90d`: nodes silent for longer are `stale`; hub changes and `delete` tombstones older than this are deleted |
| `TOKI_SYNC_MIN_KEEP` | `24h`: acknowledged hub changes are kept at least this long |
| `TOKI_SYNC_SPOKE_KEEP` | `24h`: acked spoke rows are kept this long (re-push after a hub restore) |

Durations accept Go syntax and a `d` suffix; zero or an invalid value falls back to the default.

## Snapshot bootstrap and hub epoch (PR7)

### Hub routes (node session token)

| Route | Notes |
| --- | --- |
| `POST /api/sync/snapshot` | Starts a snapshot: `{snapshot_id, start_seq (hub head), expires, hub_epoch, schema [collection exports], policies, collections [{id,name,order}]}`. The id is an HMAC-signed token (node, start_seq, epoch, 24 h). The hub keeps one small `_sync_state` row `snap:<node>` (`<start_seq>:<expiry>`, the "pin") for the snapshot a node has in progress; ids of other nodes, expired ones and ones from an older epoch answer 410 `sync_snapshot_expired` on the pages. Auth collection secrets are redacted in the schema. Collections: policy direction `both`/`pull`, ordered by `order`, then name. Rate-limit tag `sync:snapshot`. |
| `GET /api/sync/snapshot?id&collection&after&limit` | One page (max 1000 records, about 4 MiB): records in id order after `after`, each with the DB export of the synced fields (hidden fields, fieldperm-hidden fields and email/verified rules as for pull), `hlc`/`node`/field clocks from `_sync_meta` and the canonical hash. Scope: exactly the pull's (`Module.recordScope` = partition + `pullRuleOn` + `visibleForNode`): the policy's `pull_view_rule` AND the `TOKI_SYNC_PULL_VIEW_RULE=1` default (for nodes with a service actor), fieldperm-hidden fields removed; field clocks are only sent for fields that travel. A partition on a plain text/email/url/number column is filtered in SQL; other scopes are checked row by row with a cap of 20 000 scanned rows per request (the answer then has `more: true` and may hold no record; `next` is the scan position). Tombstones travel on the same id axis: `legal` ones and `delete` ones newer than `TOKI_SYNC_RETENTION`; **collections with a partition or an active view rule (policy or env default) send none** (so a fresh node has no `legal` tombstone of a purged id in such a collection: a local create of that id succeeds until the hub's push revert arrives) (a deleted record can not be checked against the node's scope, so its id/clock/origin must not leak). `next`/`more` as for pull. No long transaction: the snapshot is fuzzy and the log from `start_seq` fixes it. |
| `POST /api/sync/ack` | Takes an optional `snapshot_id`: a finished snapshot makes a `stale`/`rebootstrap` node `active` and sets `pulled_seq = start_seq`, and ends the pin. An id that is invalid, expired, of another epoch or void (the operator ran `toki sync rebootstrap <node>` or a newer snapshot replaced it) answers 410 `sync_snapshot_expired`. The id is still a claim: the hub does not check that every page was read. Digests are compared over `_sync_meta` rows with `hlc > 0` on both sides (rows that existed before sync was enabled have no hub meta row); collections with a partition, an active view rule (policy or env default) or fieldperm-hidden fields (probe over the first 2000 rows) are not compared (the node holds a subset or reduced rows). |
| `POST /api/sync/push` | Accepts op `n` (filler, see rebase) which only advances `pushed_origin_seq`. The handshake advertises it in `caps: ["filler"]`; a client sends a filler as op `u` with an empty patch to a hub without that capability (an older hub answers 400 to an unknown op). |

### Spoke (`client/bootstrap.go`)

Triggers: the handshake says `rebootstrap`, pull answers 410, `_sync_cursors.state` is `rebootstrap_required` (set by `toki sync rebootstrap` or the auto-heal), or a bootstrap was interrupted (`bootstrapping`). The loop (not `RunOnce`) then runs `Client.Bootstrap`:

1. `POST /snapshot`, create the missing collections (`ImportCollections`, existing ones untouched; an existing collection that lacks a field the hub syncs (and does not exclude) fails the bootstrap with a clear `last_error` instead of dropping that field's data).
2. Unpushed local changes of the replaced collections are **parked** (`_changes.status = 'rebase'`, same row). Changes below `push_from` are already on the hub.
3. Per collection, page by page, in one transaction per page: the first page empties the collection (rows, `_sync_meta`, `delete` tombstones, no capture), tombstones are inserted, records are applied through the pull apply path with sync origin `Snapshot` (ciphertext verbatim) so pending local changes, field clocks and the hash check behave as for pull. `_sync_cursors.snapshot_after = "<collection id>/<last id>"` moves in the same transaction: a SIGKILL or a lost connection resumes at the next page. After the last page `pull_after = start_seq`.
4. Phase `!ack` (the hub reactivates the node), `!pull` (the log from `start_seq`), `!rebase`.
5. **Rebase**: each parked change is replayed on the new data as a new local change (base = the new record clock, original actor kept; counters and sets are re-applied as deltas). The replay **keeps the original HLC** of the change (change row, record clock and field clocks are set back to it, never below what the hub data had). A plain field is only replayed when the hub value is not newer than the change (record clock, or the field clock of a field-merge collection, `<=` the original HLC); otherwise that field keeps the hub value and the lost edit is an open `orphaned` conflict (this is what lww on the original times would decide at the hub, and a newer edit is never silently overridden). Counter and set fields are commutative and always apply. Each parked change is replayed in its own transaction with its own tx id, so a multi-record transaction (debit + credit) can be applied or refused per member on the hub: do not rely on tx atomicity across a bootstrap. A change older than `TOKI_SYNC_RETENTION`, or whose record is gone or deleted on the hub, becomes an `orphaned` conflict too (open, in the local `_sync_conflicts`). The parked row turns into a filler (`code = 'rebased'`, pushed as op `n`) so that the hub's contiguous `origin_seq` stays contiguous.
6. `state = idle`, position cleared.

`state` values: `idle`, `bootstrapping`, `rebootstrap_required`, `paused`. `toki sync status` shows `state` and, during a bootstrap, `snapshot` (the position or phase). **Local writes to synced collections are refused with `503` (`sync_bootstrapping` in the message) while `state = bootstrapping`** (checked inside the capture transaction; the phase `!rebase`, which replays the parked changes, is exempt; writes of the sync apply paths are not local writes). Reads of the local REST API serve whatever the collection holds at that moment, which can be partial or empty for the whole bootstrap; `TOKI_SYNC_BOOTSTRAP_BLOCK_READS=1` makes list/view requests of synced collections answer 503 as well. `Client.RunOnce`/`PullOnce` return `ErrRebootstrap` without pushing or pulling while a bootstrap is pending (`bootstrapping` or `rebootstrap_required`) and none runs in the process.

### Deviations from the design text

- The collections are emptied lazily, with the first page of each collection (in the same transaction), not all before the first page: a bootstrap that never finishes leaves the not yet reached collections intact.
- Unpushed local changes are parked in place (`_changes.status = 'rebase'`) instead of copied to a side table, and their rows become fillers instead of being deleted: the hub's `pushed_origin_seq` must stay contiguous.
- The hub reactivates the node when the data pages are applied (ack with `snapshot_id`), before the log is pulled, because a pull of a node still flagged `stale` is refused with 410.
- A restore is noticed through a marker file and a state file outside `data.db`, because the restore replaces `data.db` (an epoch stored in it would be rolled back too). The handshake follows design §3.3: it sets `rebootstrap: true` on an epoch change unless the spoke's cursor is provably safe (see Hub epoch).
- The auto-heal is limited to 2 heals per 24 h per node (see Triggers).

### Triggers and CLI

- `toki sync rebootstrap` (spoke): sets `rebootstrap_required`; the running loop starts at its next cycle. `--now` runs the bootstrap in the CLI process.
- `toki sync rebootstrap <node>` (hub): status `rebootstrap`; the next handshake answers `rebootstrap: true`. Audit `sync.node.rebootstrap`.
- `TOKI_SYNC_AUTO_HEAL=1`: two hash mismatches in a row, or two digest checks in a row (at most every `TOKI_SYNC_DIGEST_INTERVAL`, default 10 min, only without pending changes and only when the node is at the hub head) that the hub reports as different, schedule a re-bootstrap. At most 2 heals per 24 h (`_sync_state.heal_log`); after that the loop stops healing, `Status().Heal` is `heal_exhausted`, `last_error` says so and one error is logged, until the cause is fixed or `toki sync rebootstrap` is run. A digest ignores rows without hub meta (`hlc 0`) on both sides and is not compared for subsets or fieldperm-reduced rows.
- Compaction (hub) keeps every `_changes` row with `seq >= start_seq` of a snapshot in progress and does not mark that node `stale` while the pin lives (expiry: the snapshot id's 24 h).
- New env: `TOKI_SYNC_SNAPSHOT_PAGE` (default 1000, halved after a too-large response), `TOKI_SYNC_AUTO_HEAL`, `TOKI_SYNC_DIGEST_INTERVAL`, `TOKI_SYNC_BOOTSTRAP_BLOCK_READS`, `TOKI_SYNC_TEST` + `TOKI_SYNC_TEST_CLOCK_OFFSET` (shifts the wall clock of the process, for tests).

### Hub epoch (design §3.9)

`_sync_state.epoch` is renewed (with `epoch_seq` = the head at that moment, and an entry in `_sync_state.epoch_history`, a list of `{epoch, seq}`, newest last, 32 kept) when: `OnBackupRestore` ran (the hook writes `<dataDir>/.toki-sync-restored`, excluded from the directory swap, and the next boot consumes it), a new `.toki-promoted.json` (walreplica promote) is found, or the head is below `max_seq_seen`. `max_seq_seen` is kept in `_sync_state` AND in `<dataDir>/.toki-sync-state.json` (also excluded from the restore swap), so a manual restore (copying `data.db`, a VM rollback) is noticed from the file; it is raised at handshakes, pushes, pulls and compaction (the file at most every 250 ms). The handshake returns `hub_epoch`, `hub_epoch_seq` and, **when the spoke's epoch differs, `rebootstrap: true` unless the spoke's `pull_after` is at or below `epoch_seq` of EVERY epoch after the one it knew** (history lookup; an epoch the history does not contain also re-bootstraps). Only then is everything the spoke pulled still in the hub's log; hub-origin writes that a restore lost are otherwise reconciled by the re-bootstrap, and two epoch changes can no longer skip a range. In the safe case the spoke stores the epoch and `reconcile` sends again the acked changes it still keeps (`TOKI_SYNC_SPOKE_KEEP`, 24 h). If the rows it should send again are gone (compacted: the hub lost spoke-origin changes the spoke no longer has), the spoke notices the hole in its `origin_seq` and re-bootstraps instead of keeping data that exists nowhere else. A 410 on pull first forces a handshake, so a restore is noticed even while the session token is still valid. A node enrolled after the restored backup does not exist in the restored `_sync_nodes` and is locked out: re-enroll it (restore runbook).

## Env


| Variable | Meaning |
| --- | --- |
| `TOKI_SYNC_ROLE` | `off` (default), `hub` or `spoke`. With `off` the module registers nothing except its marker: no tables, no hooks, no cost. An unknown value is a startup error (`Register` panics, `RegisterFromEnv` returns it): a mistyped hub must not run without sync. `toki sync status` reports an unknown value as `off`. |

| `TOKI_SYNC_HUB_KEY_FILE` | hub: file with the Ed25519 key (base64) instead of `_sync_state`. Created 0600 only for a hub without identity; a missing file on a hub that has one stops the boot |
| `TOKI_SYNC_NODE_KEY` | spoke: base64 key blob (seed + x25519 private key) instead of `<dataDir>/sync_node.key` |
| `TOKI_SYNC_HUB_URL` | spoke: hub url when no cursor row exists |
| `TOKI_SYNC_HUB_PIN` | spoke: SPKI sha256 pin of the hub TLS certificate |
| `TOKI_SYNC_INSECURE` | `1` allows an http hub url for loopback and private network hosts only |
| `TOKI_SYNC_ENROLL_CODE` | spoke: enrollment code for `toki sync join <hub-url>` when the code argument is left out |
| `TOKI_SYNC_MAX_DRIFT` | hub: clock drift for `clock.ok` (default `5m`) |
| `TOKI_SYNC_MAX_DRIFT` | hub: clock drift for `clock.ok` and the `future_hlc` check of pushes (default `5m`) |
| `TOKI_SYNC_INTERVAL` | spoke: idle sync interval (default `30s`) |
| `TOKI_SYNC_PAGE` | spoke: changes per push/pull page (default `500`) |
| `TOKI_SYNC_POKE` | spoke: `0` disables the realtime `@sync` subscription |

The remaining `TOKI_SYNC_*` variables of the design arrive with the PRs that use them.

## What is captured

A collection is captured only when `_sync_policies` has an `enabled` row for it (by name or id) whose `direction` is not `none`. System collections (`_*`) and views are never captured. Auth collections are allowed by the capture code; the "pull only" validation of the design comes with the policy validation in PR6.

Per field, everything is captured except: `id`, file fields, password fields and `tokenKey`, derived fields (registered by computed), and the policy `exclude` list. Autodate fields are included.

Patch (`_changes.patch`, JSON):

| op | patch |
| --- | --- |
| `c` | all synced fields (DB export form) |
| `u` | changed fields only; fields typed `counter` become `{"$inc": delta}`, fields typed `set` become `{"$add": [...], "$rm": [...]}` |
| `d` | `{}` |

Encrypted fields (modules/crypto) are captured as the stored ciphertext, byte for byte. The hash covers the ciphertext too.

An update whose changes are only derived or autodate fields (for example the `updated` bump that a computed rollup causes) writes no `_changes` row, but `_sync_meta.hash` is refreshed (the `hlc` stays), so it keeps equal to `RecordHash` of the stored row and `toki sync verify` does not see false drift. A file-only update writes no row either (files are not synced in v1).

`hash` is `sha256` of the canonical JSON `{"c": collectionId, "id": id, "f": {sorted synced fields}}`: object keys sorted, numbers as `strconv.FormatFloat(v, 'g', -1, 64)`, no HTML escaping. Numbers inside JSON fields keep their exact text (decoded with `UseNumber`; integers above 2^53 are not rounded, `1.0` stays `1.0`). Fields typed `set` are sorted and de-duplicated before hashing and diffing, so a reorder is not a change and two nodes that applied the same adds in another order have the same hash. `RecordHash` computes it.

`actor` is `rec:<authCollectionId>:<authId>` when the write came from a REST request (including `/api/batch` sub-requests) with an authenticated record, otherwise `node`. Grants (`aid`) replace this in PR4.

`tx` groups the rows of one database transaction (a `/api/batch` call, a cascade delete, or several saves in `RunInTransaction`). A row that is alone has `tx = ''`.

## Tables

All in `data.db`, created with `IF NOT EXISTS` at bootstrap when the role is not `off`. DDL exactly as in `docs/SYNC_DESIGN.md` §2.

| Table | PR1 use |
| --- | --- |
| `_changes` | the change log (`status = 'local'` for everything captured here) |
| `_sync_meta` | per record clock: `hlc`, `node`, `hash`; removed on delete |
| `_sync_tombstones` | one row per deleted id (`kind = 'delete'`); `kind = 'legal'` rows can never be updated or deleted (trigger `RAISE(ABORT)`); creating a record with a tombstoned id fails with `validation_sync_tombstoned` |
| `_sync_state` | `node_id`, `hlc_floor`, `schema_version` |
| `_sync_conflicts` (system collection, superusers only; PR5) | design §2.7: conflict rows of the hub, and on a spoke a local informational copy |
| `_sync_policies` (system collection, superusers only) | the full model of PR6 (see "Policies, partitions, purge and compaction"). The cache has a 5 s TTL and is invalidated when a policy row changes. |

`node_id` is derived from the node key since PR2 (see Identity). Rows written under the PR1 placeholder id (`_changes.node`, `_sync_meta.node`, `_sync_tombstones.node`) are migrated to the derived id in one transaction at the first boot with PR2.

Writes by sync apply paths (`kernel.WithSyncOrigin`, mode pull/snapshot/bundle) update `_sync_meta` and tombstones only and write no `_changes` row. The push mode (hub replay) is a no-op for capture: the hub apply pipeline (`hub_apply.go`) writes the `_changes` row, the record clock and the tombstone itself.

## CLI

```
toki sync status [--json]    # role, node id, pending, last hlc, hlc floor; hub: hub id, epoch, node count;
                             # spoke: hub id/url, epoch, cert expiry, clock offset, last handshake, last error
toki sync enroll --name N --profile P [--param k=v]... [--actor col/id]   # hub: prints the one-time code ONCE
toki sync join <hub-url> [<code>|-]                                       # spoke: enroll, stores cert in _sync_cursors; '-' reads the code from stdin
toki sync revoke <node id|name>                                           # hub
toki sync peers [--json]                                                  # hub: id, name, profile, status, lag, last seen, schema, offset
toki sync verify [--against-hub] [--json]                                 # per-collection digests, mismatching ids (PR3)
toki sync conflicts [--open] [--collection c] [--json]                    # list _sync_conflicts (PR5; on a spoke: the hub's answers to its changes)
toki sync conflicts --resolve <id> --take hub|incoming|<patch.json> [--note ...]   # hub: settle an open conflict
toki sync policies list|set|rm|lint                                       # hub: the policy model (PR6)
toki sync purge <collection> <id> --legal --reason "..."                  # hub: erase a record for good (PR6)
toki sync compact [--vacuum] [--json]                                     # compaction now (PR6)
toki sync rebootstrap [<node>] [--now]                                    # hub: flag a node; spoke: snapshot bootstrap (PR7)
```

`enroll`, `revoke` and `peers` need `TOKI_SYNC_ROLE=hub`, `join` needs `TOKI_SYNC_ROLE=spoke`. `join` requires an https hub url unless `TOKI_SYNC_INSECURE=1`.

## Build tag

`-tags no_sync` replaces the module with a stub (`Enabled()` false, `Register` no-op) and a marker that owns the tables above, the collection `_sync_policies`, `TOKI_SYNC_ROLE`, `TOKI_SYNC_HUB_URL` and the file `sync_node.key`. A `no_sync` binary refuses to start on a data dir that has these tables or when the role env is set, unless `TOKI_ALLOW_STUBBED_MODULES=1`. Sync is compiled into every profile; it is in no profile's `no_` list.

## Limits

- Raw SQL writes (`app.DB().NewQuery("UPDATE ...")`) are not captured.
- Files are not synced; file fields are not in patches or hashes.
- Schema bundles, reservations and clock-drift enforcement (PR8), keys (PR9) are not implemented; the handshake returns those fields empty. A snapshot creates the collections a spoke lacks (existing ones are left alone, schema changes are PR8). `Client.RunOnce` still returns `ErrRebootstrap` (the loop bootstraps; call `Client.Bootstrap` yourself otherwise or set `Options.NoAutoBootstrap`).
- The hub key cannot be rotated yet (`toki sync rotate-hub-key`, design §7.9); a lost hub key means a new hub id and re-enrollment of every node. A node offline past its certificate expiry (365 d, renewed by the handshake within the last 30 d) must enroll again.
- Several hub processes behind one URL do not share the in-memory nonce cache; the persisted `sig_ts_floor` still blocks replays of an older request.
- A derived-only update (computed rollup) or a save without changes bumps the `updated` autodate locally without a change row, so `updated` and the stored hash can differ between nodes; `toki sync verify` reports it (see Push, pull and the client loop above).
- Pull cascade: a spoke that applies a pulled delete, purge or evict runs the cascade (relation unset, cascade delete) of PocketBase locally as a **replica** write (no `_changes` row; the hub performs the same cascade itself when it applies the parent change and sends the children). The hub still captures cascade children of its own writes and of push replays.
- Hook strategy: the hub holds its apply lock while guests run. All hook conflicts of one push share a 10 s budget; after it is spent or a guest times out the remaining conflicts of that push are parked `hook_failed` without asking the guest. A `merge` patch from a guest goes through the typed-field and partition checks like a pushed patch (`validation_failed` / `policy_partition`).
- Parked changes: `--take incoming` applies a parked `c` (POST), `u` (PATCH) or `d` (DELETE) and resolves a parked `tx` group as a unit (all open conflicts of the group). `--take <patch.json>` is refused for a group.
- Not changed in the QC pass, by decision: the policy cache TTL is 5 s for changes made by another process (the CLI); a node's `base_hlc` is trusted (an authenticated node can claim to have seen the latest version); the wall clock drives compaction; a policy refers to a collection by the name it was typed with; `pull` sends one whole-record row per update under a restrictive view rule; the push existence oracle (`orphaned` vs `policy_partition`) stays as documented.
- Known gaps, planned: a policy refers to a collection or field by name (rename stops capture until the policy is fixed; `field_types` names are validated on save, `exclude` names only by lint); `counter` deltas use float64 subtraction (exact for integers); cascade children and hook-written rows are attributed to `node`, not to the request user; a panic inside a transaction leaks one `txs` map entry; `toki sync status` runs `Init` in a second process (a first start can race on `node_id`).
