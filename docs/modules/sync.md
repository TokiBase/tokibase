# Module `sync`

Phase 3 hub/spoke replication (offline-first). The full design is `docs/SYNC_DESIGN.md`; this page describes what exists today. Package `modules/sync`, subpackage `modules/sync/hlc`.

**Status: PR2 of 11 (change capture + node identity and handshake).** A node with `TOKI_SYNC_ROLE=hub|spoke` records every write of the synced collections into a local change log (PR1). Since PR2 a hub can enroll devices and authenticate them (`/api/sync/{enroll,handshake,ping}`), and a spoke has a key-derived identity and a transport client skeleton. There is no push, pull or client loop yet: no change leaves a node.

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

The answer carries `session_token` (HS256 JWT keyed by the hub `session_secret`, `typ:"toki_sync"`, `sub` node id, 15 min), `expires`, `hub_id`, `hub_epoch`, `server_time`, `clock {ok, offset_ms, max_drift_ms}`, `push_from` (`_sync_nodes.pushed_origin_seq + 1`), `params`, `policies` (the enabled `_sync_policies` rows with `strategy: "lww"`, empty `partition`, `crypto: "ciphertext"` until PR6), `poll_ms` and the fields that later PRs fill and PR2 returns empty: `schema {version: 0, bundles: []}` (PR8), `low_water: 0` (PR6), `keys: []` (PR9), `reservations: []` (PR8), `rebootstrap: false` (PR7). The hub updates `last_seen`, `clock_offset_ms`, `app_version` and `schema_version` of the node (not `profile`).

**Clock.** The hub computes `offset_ms = server_time - client_time` and `ok = |offset| <= TOKI_SYNC_MAX_DRIFT` (default 5 m). PR2 reports `ok` but does not enforce it (no push exists; enforcement is PR8). The spoke measures `offset = server_time - (t_send + t_recv)/2` with its raw clock, stores it in `_sync_cursors.clock_offset_ms` and `hlc.Clock.SetOffset`, and starts from the stored offset after a restart. Only an offset from a hub-signed time with `|offset| <= 7 days` is applied. When the hub refuses a `ts` outside its window, the signed hub time of that `401` is used once to correct the offset and retry; if the retry fails, the previous offset is restored.

**Node auth middleware** (for the routes of later PRs). `Authorization: Bearer <session_token>`: bad, expired or foreign token, unknown or pending node give `401 sync_unauthorized`; a revoked node gives `403 sync_node_revoked` (checked on every request, so revoking cuts live tokens). The node id is available with `sync.NodeFrom(e)`. `GET /api/sync/ping` uses it. PocketBase treats the sync token as a guest on all other routes.

**Routes** exist only with `TOKI_SYNC_ROLE=hub`. The rate-limit tags `sync:enroll`, `sync:handshake`, `sync:ping` can be used in Settings > Rate limits; they do nothing until that setting is enabled and rules with those labels exist (the built-in per-IP limit above is always on). Body limits: 16 KiB enroll, 64 KiB handshake.

**Client** (`modules/sync/client`): `New`, `Enroll`, `Join`, `Handshake`, `Ping`, `LoadCursor`, `StoreEnrollment`. https is required unless `TOKI_SYNC_INSECURE=1`, which allows http only for loopback and private network hosts (`localhost`, 127.0.0.0/8, ::1, 10/8, 172.16/12, 192.168/16, fc00::/7); `TOKI_SYNC_HUB_PIN` (sha256 of the hub certificate SPKI, hex or base64) is checked in addition to the normal chain verification; 30 s timeout per request. `Module.NewClient()` wires the module identity and HLC clock. No loop yet.

**Audit** (`sync.SetAuditSink`, wired in `tokibase.go` when the audit module is on): `sync.node.enroll` (details `stage: created|completed`), `sync.node.revoke`, `sync.handshake.failed` (details `node`, `reason`, `ip`).

**Data.** `_sync_nodes` system collection (hub only, superusers only; fields as in the design §2.4). `_sync_cursors` plain table (all roles; one row per hub; design §2.5 plus a `hub_pub` column). `_sync_state` keys: `node_id`, `hub_id`, `hub_key` (only without `TOKI_SYNC_HUB_KEY_FILE`), `epoch`. `_sync_nodes.sig_ts_floor` is the replay floor of a node.

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
| `_sync_policies` (system collection, superusers only) | minimal: `collection`, `direction` (`both|push|pull|none`), `field_types` (json), `exclude` (json), `enabled`. The remaining policy fields arrive in PR6. The cache has a 5 s TTL and is invalidated when a policy row changes. |

`node_id` is derived from the node key since PR2 (see Identity). Rows written under the PR1 placeholder id (`_changes.node`, `_sync_meta.node`, `_sync_tombstones.node`) are migrated to the derived id in one transaction at the first boot with PR2.

Writes by sync apply paths (`kernel.WithSyncOrigin`, mode pull/snapshot/bundle) update `_sync_meta` and tombstones only and write no `_changes` row. The push mode (hub replay) is a no-op for capture until PR3.

## CLI

```
toki sync status [--json]    # role, node id, pending, last hlc, hlc floor; hub: hub id, epoch, node count;
                             # spoke: hub id/url, epoch, cert expiry, clock offset, last handshake, last error
toki sync enroll --name N --profile P [--param k=v]... [--actor col/id]   # hub: prints the one-time code ONCE
toki sync join <hub-url> [<code>|-]                                       # spoke: enroll, stores cert in _sync_cursors; '-' reads the code from stdin
toki sync revoke <node id|name>                                           # hub
toki sync peers [--json]                                                  # hub: id, name, profile, status, lag, last seen, schema, offset
```

`enroll`, `revoke` and `peers` need `TOKI_SYNC_ROLE=hub`, `join` needs `TOKI_SYNC_ROLE=spoke`. `join` requires an https hub url unless `TOKI_SYNC_INSECURE=1`.

## Build tag

`-tags no_sync` replaces the module with a stub (`Enabled()` false, `Register` no-op) and a marker that owns the tables above, the collection `_sync_policies`, `TOKI_SYNC_ROLE`, `TOKI_SYNC_HUB_URL` and the file `sync_node.key`. A `no_sync` binary refuses to start on a data dir that has these tables or when the role env is set, unless `TOKI_ALLOW_STUBBED_MODULES=1`. Sync is compiled into every profile; it is in no profile's `no_` list.

## Limits (PR1)

- Raw SQL writes (`app.DB().NewQuery("UPDATE ...")`) are not captured.
- Files are not synced; file fields are not in patches or hashes.
- No network, no apply, no conflict handling, no compaction: `_changes` grows until PR6.
- Push, pull, the client loop, actor grants, schema bundles, keys and reservations are not implemented; the handshake returns those fields empty.
- Several hub processes behind one URL do not share the in-memory nonce cache; the persisted `sig_ts_floor` still blocks replays of an older request.
- A derived-only update (computed rollup) bumps the `updated` autodate locally without a change row, so `updated` and the stored hash can differ between nodes after the apply paths exist. The apply side (PR3) must decide how to treat it.
- A derived-only update (computed rollup) bumps the `updated` autodate locally without a change row, so `updated` and the stored hash can differ between nodes after the apply paths exist. Since `_sync_meta.hash` is refreshed for such saves, the apply side (PR3) can tell stale from diverged by comparing the hash with `RecordHash`.
- Known gaps, planned: a policy refers to a collection or field by name (rename stops capture until the policy is fixed; no `field_types`/`exclude` name validation until PR6); `counter` deltas use float64 subtraction (exact for integers); cascade children and hook-written rows are attributed to `node`, not to the request user; a panic inside a transaction leaks one `txs` map entry; `toki sync status` runs `Init` in a second process (a first start can race on `node_id`).
