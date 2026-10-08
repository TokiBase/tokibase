# Module `sync`

Phase 3 hub/spoke replication (offline-first). The full design is `docs/SYNC_DESIGN.md`; this page describes what exists today. Package `modules/sync`, subpackage `modules/sync/hlc`.

**Status: PR2 of 11 (change capture + node identity and handshake).** A node with `TOKI_SYNC_ROLE=hub|spoke` records every write of the synced collections into a local change log (PR1). Since PR2 a hub can enroll devices and authenticate them (`/api/sync/{enroll,handshake,ping}`), and a spoke has a key-derived identity and a transport client skeleton. There is no push, pull or client loop yet: no change leaves a node.

## What exists now

- `hlc`: the 64-bit hybrid logical clock (48 bit unix ms, 16 bit logical). `Clock` has `Now`, `Observe`, `SetOffset`/`Offset`/`WallNow`, an injected `now`, a spin to the next millisecond on logical overflow, and a persisted floor (`hlc_floor` in `_sync_state`, written every 1000 ticks inside the capture transaction and on terminate).
- Capture hooks on `OnRecord{Create,Update,Delete}Execute` at priority `-1<<19` (outer than crypto, which binds at 0). Each hook runs the write in `RunInTransaction`, so the `_changes` row, the `_sync_meta` row and the tombstone commit or roll back together with the record. Inside the hook `e.App` is replaced by the transaction app; the kernel copies it to the model event before the database write (`syncModelEventWithRecordEvent`), the same mechanism that `kernel.onRecordDeleteExecute` uses. A test proves that a failing inner handler leaves no record and no change row.
- Patch diff, canonical hash, exclusions, empty-patch skip, tombstone guard, `status=local`, atomic group id (`tx`), actor from the request.
- `toki sync status [--json]`.
- Kernel seams (no behaviour change for other modules): `kernel.SyncOrigin` / `WithSyncOrigin` / `SyncOriginFrom` / `IsSyncReplica`, `kernel.RegisterDerivedField` / `UnregisterDerivedField` / `IsDerived` / `DerivedFieldsOf`, and the reserved `@request.context = "sync"` (`kernel.RequestInfoContextSync`). `modules/computed` registers its target fields as derived and unregisters them when a definition is removed or its collection changes.

## Identity, enrollment and handshake (PR2)

Packages: `modules/sync` (hub side, CLI), `modules/sync/proto` (wire types, ids, certificates, request signatures), `modules/sync/client` (spoke transport).

**Hub key.** At the first boot with `TOKI_SYNC_ROLE=hub` an Ed25519 key is generated and stored in `_sync_state` (`hub_key`), so a walreplica standby has the same key. `TOKI_SYNC_HUB_KEY_FILE` keeps it in a file outside the data dir instead (created 0600, base64 of the 64 byte private key). `hub_id` = `"h" + base32(sha256(pub))[:14]` (lowercase, 15 chars). `epoch` is a random id in `_sync_state`; `session_secret` (32 random bytes) keys the session tokens. The hub writes its own captured changes under its hub id.

**Spoke key.** Ed25519 plus X25519 in `<dataDir>/sync_node.key` (0600, base64 of seed || x25519 private key) or in `TOKI_SYNC_NODE_KEY` (same blob, never written to disk). A malformed `TOKI_SYNC_NODE_KEY` stops initialization (fail closed). `node_id` = `"n" + base32(sha256(ed25519_pub))[:14]`. Losing the key means a new node id: enroll again.

**Enrollment.**

1. `toki sync enroll --name gate-1 --profile edge --param branch=B12` creates a `_sync_nodes` row (`status=pending`, `enroll_hash` = sha256 of the code, `enroll_expires` = now + 24 h) and prints the code (8 groups of 4 base32 characters) once. Case and dashes are ignored when it is entered.
2. `toki sync join <hub-url> <code>` (or `client.Join`) sends `POST /api/sync/enroll` with the code and both public keys.
3. The hub compares the code hash with every pending row in constant time, claims the row atomically (`UPDATE ... WHERE status='pending' AND enroll_hash=?`, so a code works once even under concurrency), derives the node id from the key, sets `status=active`, renames the row to the node id and answers `{node_id, hub_id, hub_url, cert, hub_pub}`. A bad, expired, used or revoked code gives the identical answer `400 sync_enroll_invalid`.
4. The spoke checks that `node_id` matches its key, `hub_id` matches `hub_pub`, and that the certificate verifies against `hub_pub` and names its key, then stores everything in `_sync_cursors` (`hub_id`, `hub_url`, `hub_pub`, `node_id`, `cert`). The hub key is trusted on first use; `TOKI_SYNC_HUB_PIN` pins the TLS key of the hub.

**Device certificate.** Compact JWS, EdDSA (only EdDSA is accepted), signed by the hub key. Claims: `iss` hub id, `sub` node id, `pub` and `kx` (base64), `params` (partition params of the node), `iat`, `exp` = `iat` + 365 d, `ser` (serial, kept in `_sync_nodes.cert_serial`). Renewal is not part of PR2.

**Signed handshake** `POST /api/sync/handshake`. Headers `X-Toki-Node`, `X-Toki-Sig-Ts` (unix ms), `X-Toki-Sig-Nonce`, `X-Toki-Sig` = base64 `Ed25519(node_key, sha256(METHOD|path|ts|nonce|hex(sha256(body))))`. The hub verifies, in this order: node exists, certificate (hub signature, expiry, `sub` = node, `pub` and `ser` match the stored node), request signature, `ts` within +-5 min of hub time, then (only for a genuine signature) status `revoked` gives `403 sync_node_revoked`, `pending` gives 401, then the nonce must be unused within 10 min (in-memory bounded LRU of 50000, cleared by a hub restart; the 5 min window bounds replay after a restart). Everything else is `401 sync_unauthorized`. A signature that is valid but outside the window returns `data.server_time`, which the client uses once to correct its offset and retry.

The answer carries `session_token` (HS256 JWT keyed by the hub `session_secret`, `typ:"toki_sync"`, `sub` node id, 15 min), `expires`, `hub_id`, `hub_epoch`, `server_time`, `clock {ok, offset_ms, max_drift_ms}`, `push_from` (`_sync_nodes.pushed_origin_seq + 1`), `params`, `policies` (the enabled `_sync_policies` rows with `strategy: "lww"`, empty `partition`, `crypto: "ciphertext"` until PR6), `poll_ms` and the fields that later PRs fill and PR2 returns empty: `schema {version: 0, bundles: []}` (PR8), `low_water: 0` (PR6), `keys: []` (PR9), `reservations: []` (PR8), `rebootstrap: false` (PR7). The hub updates `last_seen`, `clock_offset_ms`, `app_version`, `profile` and `schema_version` of the node.

**Clock.** The hub computes `offset_ms = server_time - client_time` and `ok = |offset| <= TOKI_SYNC_MAX_DRIFT` (default 5 m). PR2 reports `ok` but does not enforce it (no push exists; enforcement is PR8). The spoke measures `offset = server_time - (t_send + t_recv)/2` with its raw clock, stores it in `_sync_cursors.clock_offset_ms` and `hlc.Clock.SetOffset`.

**Node auth middleware** (for the routes of later PRs). `Authorization: Bearer <session_token>`: bad, expired or foreign token, unknown or pending node give `401 sync_unauthorized`; a revoked node gives `403 sync_node_revoked` (checked on every request, so revoking cuts live tokens). The node id is available with `sync.NodeFrom(e)`. `GET /api/sync/ping` uses it. PocketBase treats the sync token as a guest on all other routes.

**Routes** exist only with `TOKI_SYNC_ROLE=hub`. Rate-limit tags (Settings > Rate limits): `sync:enroll`, `sync:handshake`, `sync:ping`. Body limits: 16 KiB enroll, 64 KiB handshake.

**Client** (`modules/sync/client`): `New`, `Enroll`, `Join`, `Handshake`, `Ping`, `LoadCursor`, `StoreEnrollment`. https is required unless `TOKI_SYNC_INSECURE=1`; `TOKI_SYNC_HUB_PIN` (sha256 of the hub certificate SPKI, hex or base64) is checked in addition to the normal chain verification; 30 s timeout per request. `Module.NewClient()` wires the module identity and HLC clock. No loop yet.

**Audit** (`sync.SetAuditSink`, wired in `tokibase.go` when the audit module is on): `sync.node.enroll` (details `stage: created|completed`), `sync.node.revoke`, `sync.handshake.failed` (details `node`, `reason`, `ip`).

**Data.** `_sync_nodes` system collection (hub only, superusers only; fields as in the design §2.4). `_sync_cursors` plain table (all roles; one row per hub; design §2.5 plus a `hub_pub` column). `_sync_state` keys: `node_id`, `hub_id`, `hub_key`, `session_secret`, `epoch`.

## Env


| Variable | Meaning |
| --- | --- |
| `TOKI_SYNC_ROLE` | `off` (default), `hub` or `spoke`. With `off` the module registers nothing except its marker: no tables, no hooks, no cost. Unknown values count as `off`. |

| `TOKI_SYNC_HUB_KEY_FILE` | hub: file with the Ed25519 key (base64), created 0600 if missing, instead of `_sync_state` |
| `TOKI_SYNC_NODE_KEY` | spoke: base64 key blob (seed + x25519 private key) instead of `<dataDir>/sync_node.key` |
| `TOKI_SYNC_HUB_URL` | spoke: hub url when no cursor row exists |
| `TOKI_SYNC_HUB_PIN` | spoke: SPKI sha256 pin of the hub TLS certificate |
| `TOKI_SYNC_INSECURE` | `1` allows an http hub url (tests, loopback) |
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

An update whose changes are only derived or autodate fields (for example the `updated` bump that a computed rollup causes) writes no row. A file-only update writes no row either (files are not synced in v1).

`hash` is `sha256` of the canonical JSON `{"c": collectionId, "id": id, "f": {sorted synced fields}}`: object keys sorted, numbers as `strconv.FormatFloat(v, 'g', -1, 64)`, no HTML escaping. `RecordHash` computes it.

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
toki sync join <hub-url> <code>                                           # spoke: enroll, stores cert in _sync_cursors
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
- Certificates are not renewed (365 d) and the hub key cannot be rotated yet.
- A derived-only update (computed rollup) bumps the `updated` autodate locally without a change row, so `updated` and the stored hash can differ between nodes after the apply paths exist. The apply side (PR3) must decide how to treat it.
