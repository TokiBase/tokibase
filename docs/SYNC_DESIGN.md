# SYNC_DESIGN.md — Phase 3 `modules/sync` (hub/spoke offline sync)

Status: design, not implemented (2026-10-08). Target: TokiBase phase 3. Exit gate: parking prototype (2 edge gates + 1 nano phone, 48 h offline, converge with zero data loss).

`walreplica` is one-way physical HA (WAL pages from primary to standby). `sync` is logical, multi-writer, rule-checked replication between a **hub** and many **spokes**. The two are orthogonal: a `cluster` hub can use walreplica for its own HA and serve as a sync hub at the same time (see §3.9).

---

## 0. Decisions at a glance

| Topic | Decision |
| --- | --- |
| Topology | Star. The hub is the single sequencer and source of truth. Spokes never talk to each other. |
| Change capture | Kernel model hooks (`OnRecord{Create,Update,Delete}Execute`) in the same transaction as the write. No SQLite triggers. |
| Clock | 64-bit HLC (48 bit ms + 16 bit logical) with the hub's time offset applied on spokes. Ties broken by node id. |
| Pull cursor | Hub `seq` (monotonic per hub), **not** HLC. HLC decides conflicts, `seq` decides "what have I seen". |
| Node identity | Ed25519 device key generated on the spoke. `node_id` is derived from the key. The hub signs a device certificate when a one-time enrollment code is used. |
| Transport | REST under `/api/sync/*` on the hub (JSON, gzip), and a Go client in the spoke. A realtime topic pokes online spokes. |
| Auth | Node session token (from a signed handshake) for transport. Each change carries an **actor grant id** that the hub issued for (user, node). The hub replays each change through the normal record handlers with `e.Auth` = the original actor. |
| Rules on hub | Replay through `apis` record create/update/delete handlers (a new export, the same path `/api/batch` uses), so collection rules, fieldperm, batchguard, timelint, computed guards and webhooks behave exactly as for a REST client. |
| Conflicts | `lww` (record-level, default), `hub-wins`, `field-merge` (per-field clocks), `hook` (WASM on the hub). `counter`/`set` typed fields map to PocketBase's native `field+`/`field-` modifiers. |
| Partial replication | v1 supports partition-key filters only (`field = @node.<param>`). Arbitrary rule filters are an open question. |
| Tombstones | Stored in `_sync_tombstones`. Delete tombstones are pruned at `sync.retention`. `legal` tombstones (purge) are never pruned and are enforced on both sides. |
| Encryption | v1 syncs **ciphertext verbatim**. The hub sends collection DEKs wrapped to each device's X25519 key. Reasons in §7.6. |
| Files | Not synced in v1. File fields are excluded from patches and hashes. |
| Build | Tag `no_sync`, runtime `TOKI_SYNC_ROLE=off|hub|spoke` (default `off`). Compiled into every profile. |

---

## 1. Architecture

### 1.1 Components

```
                      HUB (solo/team/cluster, TOKI_SYNC_ROLE=hub)
  ┌──────────────────────────────────────────────────────────────────────────┐
  │ /api/sync/{enroll,handshake,push,pull,ack,snapshot,reserve,actor,digest} │
  │   identity ─ actor grants ─ apply pipeline ─ resolver ─ replay (apis)    │
  │   capture hooks → _changes / _sync_meta / _sync_tombstones               │
  │   compaction cron (kernel.Jobs) ─ schema bundles ─ reservations          │
  │   realtime poke topic "@sync"                                            │
  └───────────────▲───────────────────────────────▲──────────────────────────┘
                  │ HTTPS (node token)            │
     ┌────────────┴───────────┐       ┌───────────┴────────────┐
     │ SPOKE edge (gate-1)    │       │ SPOKE nano (phone)     │
     │ local REST + rules     │       │ embed.Instance + mobile│
     │ capture → _changes     │       │ capture → _changes     │
     │ client loop (push/pull)│       │ client loop, battery/  │
     │ pull apply + rebase    │       │ network hooks          │
     └────────────────────────┘       └────────────────────────┘
```

Both roles run the same capture code. Only the hub mounts the routes. Only spokes run the client loop.

### 1.2 Change capture: kernel hooks vs SQLite triggers

| Criterion | Kernel hooks (chosen) | SQLite triggers |
| --- | --- | --- |
| Actor identity (who wrote) | Available (request stash, see §1.6) | Not available |
| HLC | Go clock, persisted | Needs a custom SQL function registered in `modules/store/sqlite` |
| Ciphertext / derived fields / exclusions | Can consult `kernel.SensitiveFields`, the new `DerivedFields` and policies | Blind; would capture computed rollups and ping-pong them |
| Atomic with the write | Yes. The hook wraps `e.Next()` in `RunInTransaction` and inserts into `_changes` through the tx app | Yes |
| Raw SQL writes (`app.DB().NewQuery("UPDATE…")`) | **Not captured** (documented; `toki sync verify` finds the drift) | Captured |
| Schema changes | Nothing to regenerate | Every collection change must drop and recreate per-table triggers |
| PostgreSQL store (later) | Dialect-neutral | Second implementation |
| Kernel dependency rule | Fine (module binds hooks) | Logic would live in the SQLite driver module |

Choice: **hooks**. Raw SQL writers are already outside rules, audit and webhooks. Sync joins that existing convention.

Capture handler, bound at priority `-1<<19` (outer, after the audit/wasm stashes):

```go
func (m *Module) onExecute(op Op) func(e *core.RecordEvent) error {
    return func(e *core.RecordEvent) error {
        if !m.captures(e.Record.Collection()) { return e.Next() }
        return e.App.RunInTransaction(func(tx kernel.App) error {   // joins an outer tx
            e.App = tx
            pre := m.snapshotBefore(e)                 // implemented as FindRecordById in the tx (Original() can be stale); see docs/modules/sync.md
            if err := m.guardTombstone(tx, e, op); err != nil { return err }
            if err := e.Next(); err != nil { return err }   // crypto has encrypted by now
            return m.record(tx, e, op, pre)            // _changes + _sync_meta (+ tombstone)
        })
    }
}
```

Priority note: crypto binds at priority 0. The capture hook is outer and reads values after `e.Next()`, so it always sees stored ciphertext. Test in PR1 that replacing `e.App` inside an Execute hook reaches the default DB write (the record/model event sync copies `App`).

What is captured:

- Base and auth collections whose policy direction is not `none`. System collections (`_*`) are never captured as data. Their config travels in schema bundles (§3.8).
- Per field: everything except file fields, `password`, `tokenKey`, derived (computed) fields, and the policy's `exclude` list.
- An update whose patch ends up empty (for example a computed-only recompute) writes nothing.
- Pull/snapshot applies on a spoke (`SyncOrigin.Mode` = `Pull`/`Snapshot`) update `_sync_meta` only. No `_changes` row is written, because spokes are never a source for others.
- Hub replays (`Mode` = `Push`) write a hub `_changes` row that keeps the **origin** node, HLC and actor, with the effective (post-resolution) patch.

### 1.3 HLC

Package `modules/sync/hlc` (stdlib only):

```go
type HLC uint64                     // [48 bit unix ms][16 bit logical]
func (h HLC) Physical() time.Time
func (h HLC) Logical() uint16
func (h HLC) String() string        // 16 lowercase hex chars, used on the wire (JS-safe)
func Parse(s string) (HLC, error)
func Less(a HLC, an string, b HLC, bn string) bool   // total order: (hlc, node id)

type Clock struct { /* mu, last HLC, offset atomic.Int64 (ns in code), now func() time.Time */ }
func NewClock(now func() time.Time, last HLC) *Clock
func (c *Clock) Now() HLC                    // local event; logical overflow spins to next ms
func (c *Clock) Observe(remote HLC) HLC      // merge on receive (pull apply, push accept)
func (c *Clock) SetOffset(d time.Duration)   // hub time correction (spoke)
func (c *Clock) Offset() time.Duration
func (c *Clock) WallNow() time.Time          // now()+offset, used for client_time and autodates
```

- Persistence: at boot, `last = max(SELECT max(hlc) FROM _changes, _sync_state.hlc_floor)`. `hlc_floor` is written every 1000 ticks and on stop, so a pruned `_changes` never lets the clock go backwards.
- The hub's `Observe` rejects remote HLCs whose physical part is more than `TOKI_SYNC_MAX_DRIFT` (5 m) ahead of hub wall time (`future_hlc`).
- Tests inject `now`. e2e uses `TOKI_SYNC_TEST_CLOCK_OFFSET` (only honored when `TOKI_SYNC_TEST=1`) to compress 48 h.

### 1.4 Node identity and `device-cert`

1. An admin creates an enrollment on the hub: `toki sync enroll --name gate-1 --profile edge [--param branch=B12] [--actor gate_devices/abc123]`. This prints a one-time code (8 groups of base32, 24 h TTL, stored hashed in `_sync_nodes.enroll_hash`).
2. The spoke generates an Ed25519 signing key and an X25519 key-exchange key. They are stored in `<dataDir>/sync_node.key` (0600, a module marker file). On mobile the app may provide the key through `Options.Env["TOKI_SYNC_NODE_KEY"]` from the Keystore/Keychain.
3. `node_id = "n" + base32(sha256(ed25519_pub))[:14]`. The id is bound to the key, so it cannot be claimed without it.
4. `POST /api/sync/enroll` with `{code, ed25519_pub, x25519_pub, name, profile, app_version}`. The hub marks the node `active` and returns a **device certificate**: a compact JWS (EdDSA, signed by the hub sync key) with claims `{iss: hub_id, sub: node_id, pub, kx, params: {branch: "B12"}, iat, exp: iat+365d, ser}`. The spoke stores it in `_sync_cursors`.
5. Hub sync key: Ed25519, generated at first `hub` boot. It is stored in `_sync_state` (data.db), so a walreplica standby has the same key after failover. Optionally `TOKI_SYNC_HUB_KEY_FILE` keeps it outside `pb_data`, which is recommended when backups are less trusted than the host.
6. Revocation: `toki sync revoke <node>` sets `status=revoked` and records `revoked_at`. Every request checks the status. Unused reservations of the node are retired.

### 1.5 Transport

- Hub routes are mounted on `OnServe` only when `TOKI_SYNC_ROLE=hub`. Their middleware is node auth, rate limit labels `sync:push`/`sync:pull` (via `apis.CheckRateLimitTags`), a body limit of 8 MiB on push, and gzip.
- Node session: `POST /api/sync/handshake` is signed. The request carries `sig = Ed25519(node_key, sha256(METHOD|path|host|hub_id|ts|nonce|hex(sha256(body))))` where `host` is the normalized host of the hub URL and `hub_id` the hub the node enrolled on (a captured request is useless against another hub or host). ts must be within ±5 min (after offset, see §3.7), higher than the persisted per-node floor `_sync_nodes.sig_ts_floor`, and the nonce must be unused within 10 min (per-node LRU). The hub signs its time in the answer (`X-Toki-Server-Time`/`X-Toki-Server-Sig`) and the client trusts only a signed time. The response is `session_token`: an HS256 JWT keyed by a secret derived from the hub key (HKDF, never stored), `typ:"toki_sync"`, `sub: node_id`, 15 min. Push/pull/ack/snapshot/reserve send `Authorization: Bearer <session_token>`. PocketBase's `loadAuthToken` treats it as a guest (no record matches), and the sync middleware reads it.
- Client: `modules/sync/client` uses `net/http` with keep-alive, `TOKI_SYNC_HUB_URL` (https required unless `TOKI_SYNC_INSECURE=1`, which only allows loopback and private hosts; redirects are never followed), optional pinned hub cert SPKI hash `TOKI_SYNC_HUB_PIN`, and a 30 s timeout per request.

### 1.6 Auth model: node token + original actor

The node token proves **which device** is talking. A change is applied **as the user** who made it, and the hub never sees that user's password.

**Actor grant (hub-issued, cached on the spoke):**

1. While online, the user logs in to the hub through the normal PocketBase flow (Flutter SDK against the hub URL) and gets a hub auth token.
2. The app passes it to the spoke: `inst.Sync().AddActor(hubToken)`. The spoke calls `POST /api/sync/actor` with its node token plus `X-Toki-Actor-Token: <hub token>`.
3. The hub validates the user token (signature, `tokenKey`, sessions `sid` active). It inserts `_sync_actor_grants{aid, node, collection, record, sid, iat, exp = iat + TOKI_SYNC_ACTOR_TTL (30 d, ≤ retention)}` and returns `{aid, exp, assertion}`, where `assertion` is a JWS of the same claims signed by the hub key. The hub also returns the user's own record (without password/tokenKey) so that a spoke whose policy does not pull the auth collection can still create the local copy.
4. The spoke stores the grant in `_sync_actors` and upserts the local auth record. `inst.Sync().LocalToken(aid)` then mints a **local** auth token (`NewAuthToken` with the local `tokenKey`, which is never synced), so local rules see the same `@request.auth.id` offline.
5. Every local write captures the actor from the request stash (`OnRecord*Request` → record pointer, the same pattern as `modules/wasm/host.go` `stash`) and stores `actor = aid` of the newest valid grant for (collection, record id) on this node. Writes without request auth (Go/JS hooks, cron) use `actor = "node"`; a request by an authenticated record WITHOUT a valid grant is captured as `rec:<collectionId>:<id>` and rejected `actor_unknown` by the hub (never promoted to `node`). `node` changes are applied as the node's **service actor** (`_sync_nodes.actor_collection/actor_record`). Without a service actor they are rejected (`actor_unknown`).
6. Edge gates normally run entirely as the service actor (for example a `gate_devices` auth record bound at enrollment).

**Hub validation per change** (no password, no replayable token in the payload):

| Check | Failure code |
| --- | --- |
| grant `aid` exists | `actor_unknown` |
| `grant.node == pushing node` (an aid harvested from another device is useless) | `actor_node_mismatch` |
| `grant.iat - 5m ≤ change.hlc.physical ≤ grant.exp` | `actor_expired` |
| grant not revoked, sessions `sid` still active **at apply time**, node not revoked | `actor_revoked` |
| actor record still exists on hub | `actor_unknown` |
| actor is not a superuser (unless `TOKI_SYNC_ALLOW_SUPERUSER_ACTORS=1`) | `actor_forbidden` |

Revocation is a hard reject even for changes whose (spoke-controlled) HLC predates it. Such changes go to `_sync_conflicts` with status `open` for review (`toki sync conflicts --resolve`), never silently applied.

### 1.7 Realtime fan-out

- **Hub-local clients** (web, other REST clients): the hub replay goes through the normal record handlers. Upstream `bindRealtimeEvents` (`OnModelAfter*Success`) broadcasts with the usual per-subscriber rule checks. Nothing new is needed.
- **Spokes:** after each committed apply batch the hub sends a message on the custom realtime topic `@sync` through `app.SubscriptionsBroker()`. The payload is only `{"seq": N}` (no data, so no rule check is needed). An online edge spoke keeps one SSE subscription (`POST /api/realtime` with `subscriptions:["@sync"]`) and pulls on a poke. Spokes that cannot hold SSE (mobile in the background) use `GET /api/sync/pull?wait=25` long-poll or the interval timer.
- **Spoke-local clients:** a pull apply on a spoke uses `SaveWithContext` with origin `Pull`. Local realtime fires as usual, so the Flutter UI subscribed via `inst.Subscribe` updates live.

---

## 2. Data model

All sync tables live in **data.db**, which is needed for atomicity with record writes. High-volume tables are plain tables created with `CREATE TABLE IF NOT EXISTS` at bootstrap (audit/sessions pattern). Admin-facing config is in **system collections** (superuser-only rules, `System=true`, batchguard pattern) so the Admin UI shows them. All are listed in the module marker.

### 2.1 `_changes` (plain table; hub and spoke)

```sql
CREATE TABLE IF NOT EXISTS _changes (
  seq            INTEGER PRIMARY KEY AUTOINCREMENT, -- local order; on the hub = pull cursor (hub_seq)
  node           TEXT    NOT NULL,                  -- ORIGIN node id (hub id for hub-local writes)
  origin_seq     INTEGER NOT NULL,                  -- seq on the origin node; (node, origin_seq) = change id
  hlc            INTEGER NOT NULL,
  base_hlc       INTEGER NOT NULL DEFAULT 0,        -- record HLC the writer saw (0 = create / unknown)
  collection     TEXT    NOT NULL,                  -- collection id
  record         TEXT    NOT NULL,
  op             TEXT    NOT NULL CHECK (op IN ('c','u','d','p')),   -- create update delete purge
  patch          TEXT    NOT NULL DEFAULT '{}',     -- JSON, see 2.1.1
  hash           BLOB,                              -- sha256 of canonical record after (NULL for d/p)
  schema_version INTEGER NOT NULL,
  actor          TEXT    NOT NULL DEFAULT '',       -- grant aid | 'node' | hub: 'rec:<colId>:<id>'
  tx             TEXT    NOT NULL DEFAULT '',       -- atomic group id ('' = alone)
  part_old       TEXT    NOT NULL DEFAULT '',       -- partition key value before (hub only)
  part_new       TEXT    NOT NULL DEFAULT '',       -- partition key value after  (hub only)
  target         TEXT    NOT NULL DEFAULT '',       -- '' broadcast | node id (revert addressed to one node)
  status         TEXT    NOT NULL,                  -- spoke: local|pushed|acked ; hub: applied|rejected|parked|revert
  code           TEXT    NOT NULL DEFAULT '',       -- result code for rejected/parked
  created        TEXT    NOT NULL                   -- wall time (UTC, PB layout)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx__changes_origin ON _changes (node, origin_seq);
CREATE INDEX IF NOT EXISTS idx__changes_rec    ON _changes (collection, record, hlc);
CREATE INDEX IF NOT EXISTS idx__changes_out    ON _changes (status, seq) WHERE status IN ('local','pushed');
CREATE INDEX IF NOT EXISTS idx__changes_pull   ON _changes (seq, collection, part_new) WHERE status IN ('applied','revert');
```

On a spoke, `origin_seq = seq` for local rows. The change id on the wire is `"<node>:<origin_seq>"`.

#### 2.1.1 Patch format

```json
{
  "title": "Ticket 120034",
  "updated": "2026-10-08 10:12:00.120Z",
  "fee":   {"$inc": 2000},
  "tags":  {"$add": ["vip"], "$rm": ["new"]}
}
```

- `title`: plain value (DB export form; ciphertext stays `tkc1:…`). `fee`: counter field, delta on the spoke, absolute value from hub to spokes. `tags`: set field diff.
- `c`: all synced fields.
- `u`: changed fields only. Counter fields become `$inc` deltas and set fields become `$add/$rm` diffs, computed from `Original()`.
- `d`: `{}`. `p`: `{"reason": "..."}`.

#### 2.1.2 Canonical hash

`sha256` over canonical JSON `{"c": collectionId, "id": id, "f": {sorted synced fields → DB export value}}`. Numbers are formatted with `strconv.FormatFloat(v,'g',-1,64)`, bools/strings verbatim, json fields re-encoded with sorted keys, no HTML escaping. Excluded: file, password, tokenKey, derived, and policy `exclude` fields. Autodate fields are **included**: the apply path preserves the origin's `created`/`updated` by setting them with `SetRaw` before the autodate interceptor, which keeps manually set values (`kernel/field_autodate.go`). Ciphertext is hashed verbatim (identical on all nodes thanks to §7.6).

### 2.2 `_sync_meta` (plain table; per record clock)

```sql
CREATE TABLE IF NOT EXISTS _sync_meta (
  collection TEXT    NOT NULL,
  record     TEXT    NOT NULL,
  hlc        INTEGER NOT NULL,          -- HLC of the last applied change
  node       TEXT    NOT NULL,          -- its origin node (tie-break)
  fields     TEXT    NOT NULL DEFAULT '{}',  -- {"field": "<hlc hex>"} for field-merge collections
  hash       BLOB,
  part       TEXT    NOT NULL DEFAULT '',     -- current partition value (hub)
  PRIMARY KEY (collection, record)
) WITHOUT ROWID;
```

Records without a meta row (data from before sync was enabled) count as `hlc=0`.

### 2.3 `_sync_tombstones` (plain table)

```sql
CREATE TABLE IF NOT EXISTS _sync_tombstones (
  collection TEXT    NOT NULL,
  record     TEXT    NOT NULL,
  kind       TEXT    NOT NULL CHECK (kind IN ('delete','legal')),
  hlc        INTEGER NOT NULL,
  node       TEXT    NOT NULL,
  actor      TEXT    NOT NULL DEFAULT '',
  reason     TEXT    NOT NULL DEFAULT '',
  created    TEXT    NOT NULL,
  PRIMARY KEY (collection, record)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS idx__sync_tomb_prune ON _sync_tombstones (created) WHERE kind = 'delete';
CREATE TRIGGER IF NOT EXISTS trg__sync_tomb_legal_del BEFORE DELETE ON _sync_tombstones
  WHEN old.kind = 'legal' BEGIN SELECT RAISE(ABORT, 'legal tombstone is permanent'); END;
CREATE TRIGGER IF NOT EXISTS trg__sync_tomb_legal_upd BEFORE UPDATE ON _sync_tombstones
  WHEN old.kind = 'legal' BEGIN SELECT RAISE(ABORT, 'legal tombstone is permanent'); END;
```

These triggers are guards, not capture. They also stop a buggy prune.

### 2.4 `_sync_nodes` (system collection; hub)

| Field | Type | Notes |
| --- | --- | --- |
| `id` | text pk | `node_id` (or enrollment placeholder until enrolled) |
| `name` | text | unique |
| `profile` | select | `nano|edge|solo|team|cluster` |
| `status` | select | `pending|active|stale|rebootstrap|revoked` |
| `enroll_hash`, `enroll_expires` | text, date | sha256 of the one-time code (hidden) |
| `pubkey`, `kx_pubkey` | text | base64 |
| `cert_serial`, `cert_expires` | text, date | |
| `params` | json | partition params, for example `{"branch":"B12"}` (`@node.branch`) |
| `actor_collection`, `actor_record` | text | service actor |
| `pulled_seq` | number | highest hub `seq` the node acknowledged (compaction input) |
| `pushed_origin_seq` | number | highest **contiguous** origin_seq processed |
| `schema_version` | number | last reported |
| `clock_offset_ms` | number | last measured |
| `last_seen`, `revoked_at` | date | |
| `app_version` | text | |
| `created`, `updated` | autodate | |

Index: unique `name`, index `status`.

### 2.5 `_sync_cursors` (plain table; spoke side, one row per hub)

```sql
CREATE TABLE IF NOT EXISTS _sync_cursors (
  hub_id         TEXT PRIMARY KEY,
  hub_url        TEXT NOT NULL,
  hub_epoch      TEXT NOT NULL DEFAULT '',   -- changes after hub restore/promote (§3.9)
  node_id        TEXT NOT NULL,
  cert           TEXT NOT NULL,              -- device-cert JWS
  pull_after     INTEGER NOT NULL DEFAULT 0, -- hub seq applied locally
  acked_origin   INTEGER NOT NULL DEFAULT 0, -- own origin_seq acked by hub
  schema_version INTEGER NOT NULL DEFAULT 0,
  clock_offset_ms INTEGER NOT NULL DEFAULT 0,
  snapshot_id    TEXT NOT NULL DEFAULT '',   -- in-progress bootstrap (resumable)
  snapshot_after TEXT NOT NULL DEFAULT '',   -- "<collection>/<last id>"
  last_ok        TEXT, last_error TEXT NOT NULL DEFAULT '',
  state          TEXT NOT NULL DEFAULT 'idle' -- idle|bootstrapping|rebootstrap_required|paused
);
```

`_sync_state(key TEXT PRIMARY KEY, value TEXT)` holds `hub_id`, `epoch`, hub key (hub), `hlc_floor`, `schema_version` (hub counter) and `low_water` (lowest retained hub seq).

### 2.6 `_sync_policies` (system collection; authored on hub, shipped in bundles)

| Field | Type | Notes |
| --- | --- | --- |
| `collection` | text | name or id, unique |
| `direction` | select | `both|push|pull|none` (default when no row: `none`) |
| `strategy` | select | `lww|hub-wins|field-merge|hook` (default `lww`) |
| `partition` | text | `"<field> = @node.<param>"` or empty (all records) |
| `field_types` | json | `{"fee":"counter","tags":"set","ticket_no":"reserve:tickets"}` |
| `exclude` | json | field names never synced |
| `hook` | text | WASM module name (strategy `hook`) |
| `pull_view_rule` | bool | default true: also require the collection `viewRule` for the node's service actor on pull |
| `crypto` | select | `ciphertext|strip` (strip = encrypted fields are not sent to spokes) |
| `order` | number | bootstrap/apply order (parents before children) |
| `enabled` | bool | |

Validation on save: auth collections may only be `pull`/`none` in v1 (§10). `counter` needs a number field, `set` a multi select/relation field, `reserve:` a text or number field.

### 2.7 `_sync_conflicts` (system collection; hub, mirrored locally on spokes for display)

| Field | Notes |
| --- | --- |
| `collection`, `record` | |
| `change` | change id `node:origin_seq` |
| `node`, `actor` | |
| `kind` | `concurrent_field|rule_denied|validation_failed|unique_violation|tombstoned|actor_revoked|hook_failed|hub_wins|schema_dropped_field|reservation_out_of_range|orphaned` |
| `strategy` | |
| `incoming` | json (patch; sensitive fields `[encrypted]`) |
| `current` | json (hub state at decision; redacted) |
| `resolution` | `auto_lww|auto_merge|reverted|parked|accepted|rejected` |
| `status` | `open|resolved` |
| `resolved_by`, `resolved_at`, `note` | |
| `created` | |

Indexes: `(status, created)`, `(collection, record)`.

### 2.8 `_sync_reservations` + `_sync_sequences` (system collections; hub)

`_sync_sequences`: `name` (unique), `next` (number), `block` (default 1000), `max_open_per_node` (default 2), `max_block` (default 10000), `format` (for example `"G{node.code}-{n:06}"`, only documentation and client formatting), `created/updated`.

`_sync_reservations`: `sequence`, `node`, `start`, `end` (inclusive), `status` (`active|exhausted|retired`), `high_water` (highest value seen used in pushes), `issued`, `expires`, `created`. Index `(sequence, node, status)` and unique `(sequence, start)`.

Spoke mirror: `_sync_reserved(sequence, start, end, next, status)` plain table.

### 2.9 Also

- `_sync_actor_grants` (plain; hub): `aid PK, node, collection, record, sid, iat, exp, revoked_at`. Index `(node, collection, record)`.
- `_sync_actors` (plain; spoke): `aid PK, collection, record, exp, assertion`.
- `_sync_schema` (plain; hub): `version PK, hash, bundle (json), created`.

### 2.10 Size estimate (10k changes/day)

| Item | Per row | Notes |
| --- | --- | --- |
| `_changes` row | ~470 B data + ~170 B indexes ≈ **0.65 KB** | patch avg 250 B |
| 10k/day | ~6.5 MB/day | |
| Hub, all spokes ack within 1 day (min keep 24 h) | ~10–15 MB steady | compaction §3.6 |
| Hub, worst case (one spoke offline 90 d) | ~585 MB | `stale` after retention releases it |
| Spoke | ~0 steady | acked local rows pruned after `TOKI_SYNC_SPOKE_KEEP` (24 h, see §3.9) |
| `_sync_meta` | ~120 B/record | 1M records ≈ 120 MB, the dominant long-term cost; drop `fields` for non-field-merge collections |
| `_sync_tombstones` | ~110 B/delete | pruned at 90 d |

Compaction runs `PRAGMA incremental_vacuum` when `auto_vacuum=INCREMENTAL` is set. Otherwise `toki sync compact --vacuum`.

---

## 3. Protocol

All bodies are JSON, gzip accepted. Errors use the PocketBase shape `{"status":409,"message":"...","data":{"code":"sync_clock_drift", ...}}`. HLCs are 16-hex strings. Times are RFC3339 UTC with milliseconds.

### 3.1 Session order (spoke loop)

```
handshake → [apply schema bundles] → [refresh keys/policies] → push* (pages) → pull* (pages) → ack → sleep/poke
                     └ rebootstrap_required → snapshot (resumable) → pull from snapshot seq
```

### 3.2 `POST /api/sync/enroll` (no auth; code is the secret)

```json
// req
{"code":"7KQ2-...","ed25519_pub":"base64","x25519_pub":"base64","name":"gate-1","profile":"edge","app_version":"0.41.0"}
// 200
{"node_id":"nq3x7...","hub_id":"h9...","hub_url":"https://hub.example.com","cert":"<JWS>","hub_pub":"base64"}
```

Errors: 400 `sync_enroll_invalid` (bad/expired/used code, same message for all), 429.

### 3.3 `POST /api/sync/handshake`

Headers: `X-Toki-Node`, `X-Toki-Sig-Ts`, `X-Toki-Sig-Nonce`, `X-Toki-Sig`.

```json
// req
{
  "node_id":"nq3x7...", "cert":"<JWS>",
  "client_time":"2026-10-08T10:00:00.120Z",
  "schema_version": 12,
  "pull_after": 88100, "acked_origin": 1042, "next_origin": 1043,
  "hub_epoch": "e5f...", "profile":"edge", "app_version":"0.41.0",
  "caps": ["gzip","counter","set","reserve","ciphertext"]
}
// 200
{
  "session_token":"<jwt>", "expires":"...", "hub_id":"h9...", "hub_epoch":"e5f...",
  "server_time":"2026-10-08T10:00:01.002Z",
  "clock": {"ok": true, "offset_ms": 882, "max_drift_ms": 300000},
  "schema": {"version": 14, "bundles": [{"version":13,"hash":"...","bundle":{}}, {"version":14,"hash":"...","bundle":{}}]},
  "policies": [{"collection":"pbc_tickets","direction":"both","strategy":"field-merge",
                "partition":"branch = @node.branch","field_types":{"fee":"counter"},"exclude":[],"crypto":"ciphertext"}],
  "params": {"branch":"B12"},
  "keys": [{"collection":"pbc_patients","version":2,"wrapped":"base64(x25519-hkdf-aesgcm)"}],
  "push_from": 1043,
  "low_water": 51000,
  "rebootstrap": false,
  "reservations": [{"sequence":"tickets","start":120001,"end":121000,"remaining_hint":312}],
  "poll_ms": 30000
}
```

Rules:

- `client_time` is the spoke wall time WITH its current offset. `push_from` is the hub's contiguous+1: the spoke resends from there. `low_water` is the oldest retained hub seq.
- `clock.ok=false` when `|client_time - server_time| > max_drift`. The handshake still succeeds and returns `offset_ms`, but push returns 409 `sync_clock_drift` until a handshake with corrected time succeeds (§3.7).
- `rebootstrap=true` when `pull_after < low_water`, the node is `stale`/`rebootstrap`, `schema_version` is older than the oldest kept bundle, or the hub epoch differs and the spoke's cursor is ahead of the hub's max seq (§3.9).
- `bundles` lists every version greater than the spoke's, up to `TOKI_SYNC_MAX_BUNDLES` (50). Beyond that the answer is `rebootstrap`.

### 3.4 `POST /api/sync/push`

```json
// req
{
  "batch": "nq3x7-1043-1090",
  "schema_version": 14,
  "client_time": "2026-10-08T10:00:02.000Z",
  "changes": [
    {"id":"nq3x7:1043","hlc":"0192a3b4c5d60001","base":"0192a3b4c5d50000",
     "collection":"pbc_tickets","record":"t8h2k...","op":"u",
     "patch":{"status":"closed","fee":{"$inc":2000},"updated":"2026-10-06 22:11:00.000Z"},
     "hash":"9f2c...","actor":"agr_3k2...","tx":"","sv":13}
  ]
}
// 200
{
  "server_time":"...",
  "acked_through": 1090,
  "results":[
    {"id":"nq3x7:1043","status":"merged","hub_seq":88231,"hash":"a71e..."},
    {"id":"nq3x7:1044","status":"rejected","code":"rule_denied","hub_seq":88232},
    {"id":"nq3x7:1045","status":"duplicate","hub_seq":88010}
  ]
}
```

- `batch` is the idempotency key = node + first..last origin_seq. `acked_through` is the highest contiguous origin_seq with a FINAL status. A rejected result's `hub_seq` is that of the revert row.
- Limits: max 500 changes or 8 MiB per request (413 `sync_batch_too_large`). Changes must be in ascending `origin_seq`, starting at `≤ push_from`. Gaps return 409 `sync_push_gap` with `push_from`.
- Statuses: `applied`, `merged` (resolution changed the patch), `superseded` (lost lww, nothing written), `duplicate` (already processed; returns the stored result), `rejected` (with code; a `revert` row for the node is written), `parked` (hook failed or actor revoked; awaiting admin; counts as final for ack, and the spoke keeps the record marked "pending review").
- `tx` groups: changes sharing a non-empty `tx` must be contiguous. They are applied in one transaction through the batch replay path (batchguard runs), all or nothing. If any fails, the whole group is rejected with the first failure's code.
- Idempotency: `(node, origin_seq)` is unique in `_changes`. Rejected/parked results are stored too (status `rejected`/`parked`), so a re-push after a lost response is answered from the table.
- Pre-checks per request (whole request refused): node active (403 `sync_node_revoked`), clock ok (409 `sync_clock_drift`), `schema_version == hub version` (409 `sync_schema_behind`, apply bundles first; pending changes stamped with older `sv` are mapped, see §3.8).

### 3.5 `GET /api/sync/pull?after=<seq>&limit=500[&wait=25]`

```json
// 200
{
  "server_time":"...", "schema_version":14, "low_water":51000,
  "changes":[
    {"seq":88231,"id":"nq3x7:1043","node":"nq3x7","hlc":"0192a3b4c5d60001",
     "collection":"pbc_tickets","record":"t8h2k...","op":"u",
     "patch":{"status":"closed","fee":14000,"updated":"..."},
     "hash":"a71e...", "fields":{"status":"0192a3b4c5d60001"}},
    {"seq":88232,"id":"nq3x7:1044","node":"nq3x7","op":"u","revert":true,
     "record":"t9...","patch":{"...":"full current hub record"},"hash":"..."},
    {"seq":88240,"op":"x","collection":"pbc_tickets","record":"t1...","evict":true}
  ],
  "next": 88240, "more": false
}
```

- Counters/sets carry ABSOLUTE values from the hub. `fields` holds field clocks (field-merge only). `op: "x"` with `evict` means the record left the node's partition.
- Server query: `seq > after AND (status='applied' OR (status='revert' AND target=:node)) AND collection IN (:pull_enabled) AND (partition IS NULL OR part_new=:p OR part_old=:p)`. A row with `part_old=:p AND part_new!=:p` becomes an `evict` (local delete, no tombstone). With `pull_view_rule` the candidate record ids are also checked with one `SELECT id … WHERE id IN (…) AND (<viewRule>)` per collection per page, using the service actor's RequestInfo. Records that fail are omitted. Evict and delete rows are sent only for records previously delivered to the node: with a restrictive view rule the hub keeps a per node sent set (`_sync_sent`), with a partition the `part_old` key already says it (implemented in the QC pass; nodes that pulled before it keep the old behaviour until re-bootstrapped).
- `pull` with `after = X` implicitly acks X (`pulled_seq = max(pulled_seq, X)`).
- `after < low_water` returns 410 `sync_rebootstrap_required`.
- `wait` (0–25 s) long-polls until a new `seq` exists.

### 3.6 `POST /api/sync/ack`

```json
{"pulled_through": 88240, "digest": {"pbc_tickets": "sha256-of-sorted(id,hash)"}}
// 200 {"ok":true, "digest_mismatch": ["pbc_tickets"]}
```

Compaction (hourly cron through `kernel.Jobs`, `CronKey("sync.compact:<hour>")`):

1. Nodes with `last_seen < now - retention` become `stale`. They are excluded from the minimum and must re-bootstrap.
2. `safe = min(pulled_seq of active nodes)`. Delete `_changes` where `seq ≤ safe AND created < now - TOKI_SYNC_MIN_KEEP (24h)`, plus everything older than retention. Set `low_water` to the HIGHEST deleted seq (not the oldest remaining one): a cursor at exactly `low_water` can still resume, and the safe clause never deletes rows an active node still needs (deliberate; PR6 behaviour).
3. Delete `_sync_tombstones` with `kind='delete' AND created < now - retention`. `legal` rows are never deleted.
4. Delete `_sync_conflicts` resolved more than 90 days ago.
5. On spokes: delete `_changes` with `status='acked' AND created < now - TOKI_SYNC_SPOKE_KEEP`.

### 3.7 Clock drift

- The spoke measures `offset = server_time - (t_send + t_recv)/2` on each handshake and stores it in `_sync_cursors.clock_offset_ms` and `Clock.SetOffset`. All new HLCs, `client_time` and autodate values written by sync use `WallNow()`.
- When the hub says `clock.ok=false`: the spoke sets the offset, **re-stamps** pending `local` changes whose HLC physical is more than 5 min ahead of corrected now (in origin order, `hlc = Clock.Now()` per row, which keeps their relative order), and re-handshakes once. Changes stamped in the past are kept: offline edits are legitimately old.
- The hub additionally rejects single changes with `hlc > hub_now + 5m` (`future_hlc`). This catches forged HLCs from a spoke that ignores corrections.

### 3.8 Schema versioning and migration bundles

- The hub keeps an integer `schema_version` in `_sync_state`. `OnCollectionAfter{Create,Update,Delete}Success` (and imports) on the hub bump it and store a bundle in `_sync_schema`: `{version, collections: <export of all synced collections incl. ids>, config: {_sync_policies, _field_rules, _batch_rules, _computed_fields, _crypto_fields rows}, hash}`. Bundles are full snapshots, not diffs, so applying any version is idempotent.
- The spoke applies bundles in order, each in one transaction with origin `Bundle`. It uses `app.ImportCollections(bundle.collections, false)` and replaces the config rows, then runs `computed backfill` for changed definitions.
- With `TOKI_SYNC_ROLE=spoke`, local collection create/update/delete through the API or the CLI is refused unless the origin is `Bundle`. This stops spokes from forking the schema.
- Every `_changes` row carries `schema_version` (`sv`). On push, the hub maps field names from version `sv` to current field ids using `_sync_schema` (PocketBase field ids survive renames). Fields that no longer exist are dropped and a `schema_dropped_field` conflict row is written (status `resolved`, resolution `auto_merge`).
- A spoke whose `sv` is older than the oldest kept bundle must re-bootstrap. Its pending local changes are kept as `orphaned` conflicts for the app to show.

### 3.9 Snapshot bootstrap and hub epochs

`POST /api/sync/snapshot` starts a snapshot: `{snapshot_id, start_seq (hub max seq at start), schema (latest bundle), keys, collections:[{id, order}]}`.

`GET /api/sync/snapshot?id=<sid>&collection=<id>&after=<record id>&limit=1000` returns:

```json
{"records":[{"id":"...","data":{"...":"db export, synced fields"},"hlc":"...","node":"...","fields":{},"hash":"..."}],
 "tombstones":[{"record":"...","kind":"legal","hlc":"..."}], "next":"<last id>", "more":true}
```

- Scope: policy pull-enabled collections, filtered by partition and `pull_view_rule`. Tombstones: all `legal` ones plus `delete` ones newer than retention.
- Fuzzy snapshot plus log: pages are read without a long transaction. After the last page the spoke pulls from `start_seq`. Re-applying a change already in the snapshot is a no-op (HLC/hash compare), so the result converges.
- Spoke apply: before the first page, unpushed local changes are exported to a side table and the synced collections are emptied inside one transaction per collection. Rows are inserted with `SaveNoValidateWithContext(origin=Snapshot)`, which accepts ciphertext verbatim. The cursor `snapshot_after` is updated per page, so the snapshot resumes after a crash or loss of network. After finishing, the exported local changes are re-captured as new local changes with fresh HLCs (rebase). Those older than retention become `orphaned` conflicts.
- Re-bootstrap triggers: 410 from pull, `rebootstrap=true` from handshake, digest mismatch twice in a row (optional, `TOKI_SYNC_AUTO_HEAL=1`), or `toki sync rebootstrap`.
- Hub epoch: a random id in `_sync_state`, regenerated on backup restore and on walreplica promote (hook `OnBackupRestore` plus a check at boot: `max(seq) < stored max_seq_seen`). If the handshake shows a different epoch, the spoke: (a) resets `pull_after = min(pull_after, hub max seq)`, (b) re-pushes its acked changes still kept (`SPOKE_KEEP` 24 h; duplicates are cheap). This covers the seconds of hub writes lost by an async replica failover.

### 3.10 `POST /api/sync/reserve`

```json
// req
{"sequence":"tickets","count":1000}
// 200
{"sequence":"tickets","ranges":[{"id":"rsv_9k...","start":120001,"end":121000,"expires":"2027-01-06T00:00:00Z"}],"format":"G{node.code}-{n:06}"}
// 429 sync_reservation_limit  (max_open_per_node active ranges, or count > max_block)
```

- `GET /api/sync/reserve` lists the node's active ranges. `POST /api/sync/reserve/release {"id": "..."}` returns unused ranges (values are never reissued; gaps are accepted and documented).
- Spoke use: `field_types: {"ticket_no": "reserve:tickets"}` makes an `OnRecordCreate` hook (before validation) fill an empty field with the next local value. The Go API is `sync.Next(app, "tickets") (int64, error)` and the embed API is `inst.Sync().Next("tickets")`. Values are taken in the same transaction as the record. A background prefetch runs when the remaining count drops below 20%. When ranges are exhausted, the create fails with 503 `sync_reservation_exhausted` (fail closed: no duplicate numbers).
- Hub enforcement on push: a value in a `reserve:` field must fall in an active or exhausted range issued to the pushing node (`reservation_out_of_range`). `high_water` is updated. The collection should also carry a normal unique index on that field.

### 3.11 `POST /api/sync/actor`

Covered in §1.6. Response: `{aid, exp, assertion, record}`. `DELETE /api/sync/actor/{aid}` revokes a grant (logout on device).

### 3.12 Error codes

| HTTP | code | Client action |
| --- | --- | --- |
| 400 | `sync_bad_request` | bug; log, back off |
| 401 | `sync_unauthorized` | re-handshake |
| 403 | `sync_node_revoked` | stop loop, surface to app |
| 409 | `sync_clock_drift` | apply offset, re-stamp, re-handshake |
| 409 | `sync_schema_behind` | handshake (bundles), retry |
| 409 | `sync_push_gap` | resend from `push_from` |
| 410 | `sync_rebootstrap_required` | snapshot |
| 413 | `sync_batch_too_large` | halve page |
| 429 | `sync_rate_limited` / `sync_reservation_limit` | honor `Retry-After` |
| 503 | `sync_hub_unavailable` (standby/readonly/busy) | backoff |

Per-change codes: `rule_denied`, `validation_failed`, `unique_violation`, `tombstoned`, `legal_tombstone`, `actor_unknown|actor_node_mismatch|actor_expired|actor_revoked|actor_forbidden`, `policy_direction`, `policy_partition`, `future_hlc`, `reservation_out_of_range`, `hub_wins`, `hook_rejected`, `hook_failed`, `superseded`.

### 3.13 Resumability summary

Every step is idempotent and cursor-based:

- **Push.** The change id is `(node, origin_seq)`. `push_from` comes from the handshake. After a lost response the spoke resends, and the hub answers `duplicate` with the stored result.
- **Pull.** The cursor is `pull_after`. Applying a pulled change twice is a no-op: an equal hash, or an HLC that is not newer.
- **Snapshot.** The cursor is `snapshot_after`.
- **Bundles.** Each bundle is a full schema snapshot (§3.8), so applying it again is safe.

A spoke offline for weeks resumes as long as two things hold: its age is below `sync.retention` and its schema lag is at most 50 bundles. Otherwise the hub answers 410 and the spoke re-bootstraps.

---

## 4. Conflict resolution

The hub decides everything. Spokes apply hub results and rebase their pending changes, so all nodes converge on the hub state.

### 4.1 Hub apply pipeline (per change or tx group)

```
dedupe → envelope (future_hlc, policy direction, partition before/after, schema map, reservations)
 → actor grant → tombstone → resolve(strategy) → effective patch
 → replay via apis (rules as actor) in a tx: capture writes hub _changes(status=applied, origin node/hlc/actor)
     + _sync_meta + tombstone
 → on failure: _changes(status=rejected, code) + _sync_conflicts + revert row (target=node, full hub state)
 → poke @sync
```

"Concurrent" means `change.base_hlc != meta.hlc`: the writer did not see the latest hub version.

### 4.2 `lww` (default, record level)

- Not concurrent: apply the patch. `meta.hlc = max(meta.hlc, change.hlc)`.
- Concurrent: if `Less(meta.hlc, meta.node, change.hlc, change.node)`, the incoming change wins and its patch applies (fields not in the patch keep hub values). Otherwise the result is `superseded`: nothing is written and a `_sync_conflicts` row is written with resolution `auto_lww`, status `resolved`.
- Delete vs update: tombstones win. Any `u` for a tombstoned record is `tombstoned`. A `d` newer than the meta deletes. A `d` older than a concurrent `u` still deletes (deletes are final; this keeps behavior predictable and matches "honor tombstones").

Example: gate-1 sets `status=closed` at HLC 10:05. The phone (offline) sets `status=disputed` at HLC 10:03, base 09:00. The phone pushes later and is concurrent with a lower HLC, so it is superseded. The phone pulls `status=closed`.

### 4.3 `hub-wins` (reference data)

- Not concurrent: apply the patch (subject to rules).
- Concurrent: reject with `hub_wins` and send a revert. Usually combined with `direction=pull`, so pushes are refused with `policy_direction` before resolution.

### 4.4 `field-merge`

Per-field clocks are kept in `_sync_meta.fields`. For each field `f` in the patch:

- `fields[f] ≤ base_hlc` (f unchanged since the writer's base): apply.
- Otherwise the field is concurrent. Apply if `change.hlc > fields[f]` (tie: node id), else drop the field. Either way write one `concurrent_field` conflict row (resolution `auto_merge`, status `resolved`, or `open` if the policy sets `review: true`).

The effective patch is the applied fields. If it is empty, the status is `superseded`.

Example: base `{plate:"B1234", fee:0, note:""}` at HLC 09:00.

- gate-1 (10:05): `{fee: {"$inc": 5000}}`
- phone (10:07): `{note: "scratch on door"}`
- gate-2 (10:06): `{plate: "B1243"}`
- phone (10:08): `{plate: "B1234X"}`, base 09:00

Result: fee 5000, note set, and plate is concurrent between gate-2 (10:06) and phone (10:08). The phone wins, and a conflict row records `B1243` vs `B1234X`.

### 4.5 Typed fields: `counter` and `set` (any strategy except `hub-wins`)

- `counter` (number field): the spoke captures `{"$inc": new - old}`. The hub replays as PocketBase body `{"fee+": delta}` (native modifier), so update rules see `@request.body.fee` correctly. It never conflicts and never takes part in field clocks. The hub emits the **absolute** stored value on pull. A spoke applying a pulled counter sets `value = hub_value + Σ(pending local $inc for that field)`, so local unpushed increments are not lost and are not double counted.
  - Example: hub `scans=10`. gate-1 does `+3`, phone does `+2` offline, gate-2 does `+1`. Whatever the order, hub = 16. The phone, before pushing, sees pull `scans=14` (from gate-1 and gate-2) and shows `14 + 2 = 16`.
- `set` (multi select/relation): the spoke captures `{"$add":[…],"$rm":[…]}` diffed from `Original()`. The hub replays as `{"+tags":[…], "tags-":[…]}` in hub arrival order. Concurrent add and remove of the same element: the one applied last on the hub wins (hub order = deterministic). On pull the spoke gets the absolute list and re-applies its pending ops.
  - Example: hub `{a}`. gate-1 adds `b`, phone removes `a` and adds `c`. Hub result `{b, c}` for any arrival order.
  - Known limit: "remove `x`" on one spoke and "add `x`" on another leaves `x` present only if the add arrives last. This is documented. No OR-set metadata in v1 (deliberately no general CRDTs).

### 4.6 `hook` (WASM on the hub)

Kernel event (new, §8.1): `kernel.OnSyncConflictFor(app)`, emitted by modules/sync only for concurrent changes in `hook` collections. modules/wasm subscribes and exposes guest events `sync.conflict.<collection>` / `sync.conflict.*`.

Guest stdin (ABI `toki/1`, `kind: "sync"`):

```json
{
  "abi":"toki/1","module":"payments_conflict","event":"sync.conflict.payments","kind":"sync","phase":"before",
  "collection":"payments",
  "sync":{
    "record_id":"pay_7h2...",
    "current":{"id":"pay_7h2...","invoice":"inv_1","status":"paid","provider_ref":"QR-111","amount":50000},
    "current_hlc":"0192a3b4c5d60001","current_node":"nGate1",
    "incoming":{"op":"u","node":"nPhone","hlc":"0192a3b4c5e70000","base_hlc":"0192a3b4c5000000",
                "patch":{"status":"paid","provider_ref":"CASH-9"},
                "actor":{"kind":"auth","id":"u_12","collection":"officers"}},
    "field_clocks":{"status":"0192a3b4c5d60001"}
  },
  "time":"2026-10-08T10:00:00Z"
}
```

Guest stdout:

```json
{"ok":true,"resolution":"merge","patch":{"status":"paid","provider_ref":"QR-111","note":"double payment CASH-9, refund"},
 "message":"double payment"}
```

- `resolution` is one of `accept` (apply the incoming patch), `reject` (revert to the spoke), `merge` (apply the given patch as the incoming actor, still rule-checked), or `park` (conflict `open`, nothing applied).
- The guest can create side records (refund task) with `records_save` if the module `needs=["records"]`.
- Fail closed: no handler, a trap, a timeout or invalid output means `park` with code `hook_failed`. Spokes have no WASM (edge/nano are `no_wasm`), so locally they treat `hook` collections as `lww` provisionally until the hub decides.

### 4.7 Spoke-side pull apply and rebase

For each pulled change:

- `revert`: overwrite the local record with the hub state and drop pending local changes for that record with `hlc ≤` the rejected change.
- `evict`: delete locally without a tombstone or capture.
- `d`/`p`: delete, write a tombstone, and drop pending changes for the record.
- `c`/`u`: apply each field unless a pending local change on that field has a higher HLC (it will win at the hub too). Counter and set fields are rebased as in §4.5. Then compare the hash. If there are no pending changes for the record and `local_hash != hash`, log `hash_mismatch`; it is counted in status, and two in a row trigger the auto-heal option.

---

## 5. Rule re-evaluation on the hub

### 5.1 Replay mechanism

New export `apis/sync_exports.go`:

```go
// ReplayRecordRequests executes record create/update/delete InternalRequests as `auth`
// inside ONE transaction (the batch processor, RequestInfo context "sync"), triggering
// OnBatchRequest (batchguard) when len(reqs) > 1. ctx values (kernel.SyncOrigin) reach the model hooks.
func ReplayRecordRequests(ctx context.Context, app core.App, auth *core.Record,
    headers map[string]string, reqs []*core.InternalRequest) ([]*BatchRequestResult, error)
```

- Built on `batchProcessor`/`processInternalRequest` with a synthetic base `RequestEvent` (`Auth = actor`, `RemoteAddr` = node's last IP, header `X-Toki-Sync-Node: <node>`), `infoContext = "sync"`.
- Effective patch to request mapping: `c` → `POST /api/collections/{c}/records` with `id` in the body. `u` → `PATCH …/{id}`, with counters as `field+` and sets as `+field`/`field-`. `d` → `DELETE`. `p` (purge) is not replayed through REST: it is superuser-only on the hub (§7.3).
- Change in `apis/record_crud.go`, behavior-neutral: `form.SetContext(context.WithoutCancel(e.Request.Context()))` and `e.App.DeleteWithContext(context.WithoutCancel(e.Request.Context()), e.Record)`. `WithoutCancel` keeps upstream's "no cancellation" semantics while letting context **values** (the sync origin) reach model hooks. Covered by the SDK e2e suite.
- Autodate preservation: the sync module's `OnRecord{Create,Update}Execute` handler (outer) calls `SetRaw` with the origin's `created`/`updated` when `SyncOriginFrom(ctx)` is set.
- Each change gets its own transaction unless grouped by `tx`, so a failing change does not roll back unrelated ones. Throughput: about 1–3 ms per change. A 48 h backlog of 5k changes takes 5–15 s.

### 5.2 RequestInfo with the original actor

| Rule input | Value during replay |
| --- | --- |
| `@request.auth.*` | the actor record loaded on the hub (current hub state, not the spoke's copy) |
| `@request.auth.kind` | `user` (or `agent`). `superuser` only with `TOKI_SYNC_ALLOW_SUPERUSER_ACTORS=1`. Service actor = whatever its collection is |
| `@request.context` | `"sync"` (new constant `kernel.RequestInfoContextSync`). Rules may branch, for example `@request.context != "sync" || @request.auth.role = "gate"` |
| `@request.headers.x_toki_sync_node` | node id (rules can pin collections to specific devices) |
| `@request.body.*` | effective patch (with modifiers resolved by PocketBase, as for any client) |
| `@request.method` | POST/PATCH/DELETE |

Semantics: a change is accepted only if the actor could make it **on the hub now**. If the rule changed or the record moved while the spoke was offline, the result is `rule_denied` and a revert. This is the safe default. Open question 2 covers parking instead.

### 5.3 Interaction with other modules

| Module | Behavior |
| --- | --- |
| fieldperm | Write rules run in replay (request hooks). Read rules do not affect pull (pull is node-scoped, §7.7). |
| batchguard | `tx` groups replay through `OnBatchRequest`, so `assert`/`assert_post` run on the hub. On spokes, batchguard runs locally too (nano includes it). |
| computed | Derived fields are registered in `kernel.DerivedFields` and never captured or synced. Each node recomputes from synced children. Client write guards still apply in replay. |
| crypto | Ciphertext accepted verbatim under `SyncOrigin` (§7.6). Validation runs on decrypted values. |
| timelint | Applies to replayed bodies like any request. Captured dates are already normalized at the spoke. |
| audit (hub) | New sink `sync.SetAuditSink`, wired in `tokibase.go`. Entries: `sync.apply` (only for superuser/service actors or when `TOKI_SYNC_AUDIT_ALL=1`, volume), `sync.reject`, `sync.conflict`, `sync.node.enroll/revoke`, `sync.actor.grant/revoke`, `sync.purge`, `sync.reserve`. `actor_*` = original actor. `request` JSON = `{"path":"/api/sync/push","ip":…,"node":…,"change":…,"hlc":…}` (no schema change). |
| webhooks / push / wasm `record.after.*` | Fire **once, on the hub**: replay goes through the normal After hooks. On spokes, pull/snapshot/bundle applies carry `SyncOrigin{Mode: Pull/Snapshot/Bundle}`, and webhooks/wasm after-handlers skip them. Edge/nano have neither module, but a `solo` spoke must not double-fire. |
| sessions | Grant validation calls `kernel.SessionActive(app, sid)` (new seam, set by sessions). If sessions is off: grants are checked by `tokenKey` hash captured at grant time. |
| roles | `@role()` rules evaluate on hub data (roles live on the hub). `_roles`/`_memberships` are not synced to edge/nano (`no_roles` there). |

---

## 6. Spoke side (nano / edge)

### 6.1 Local write path

The app writes through `embed.Call` or local REST and local rules. The capture hook writes `_changes(status=local)` in the same transaction, so the UI is never blocked by the network. The local `id` is generated on the spoke (PocketBase 15-char random id), and reservation fields are filled at create time.

### 6.2 Client loop (`modules/sync/client`)

```go
type Client struct{ /* app, clock, store, transport, cond Conditions, events chan Event */ }
func New(app core.App, cfg Config) (*Client, error)
func (c *Client) Start(ctx context.Context)          // started OnServe when ROLE=spoke and enrolled
func (c *Client) Stop(ctx context.Context) error     // bound to OnTerminate, finishes the current page
func (c *Client) SyncNow() <-chan Result
func (c *Client) Pause(); func (c *Client) Resume()
func (c *Client) SetConditions(Conditions)            // {Online, Metered, LowPower, Background bool}
func (c *Client) Status() Status                      // pending, last_ok, last_error, offset, state, conflicts
func (c *Client) Events() <-chan Event                // applied, rejected, conflict, rebootstrap, error
func (c *Client) Enroll(ctx context.Context, hubURL, code string) error
func (c *Client) AddActor(ctx context.Context, hubToken string) (aid string, err error)
func (c *Client) LocalToken(aid string) (string, error)
func Next(app core.App, sequence string) (int64, error)  // reservation value
```

Behavior:

- Triggers: interval (`TOKI_SYNC_INTERVAL`, 30 s; 5 min when `LowPower` or `Metered`), a local write (debounced 2 s), an `@sync` poke, `SyncNow`.
- Backoff on error: exponential 1 s → 5 min with ±20% jitter. Resets on success. `Retry-After` is honored.
- Conditions: `Online=false` means no attempts (no wasted radio). `Metered` gives push-only plus a pull page limit of 100. `Background` (iOS) means one bounded cycle within 20 s then stop.
- Each page commits locally before the next request (`pull_after` moves forward in the same transaction as the applies).

### 6.3 Embed and mobile facade

`embed/sync.go` (always compiled; methods return `ErrSyncUnavailable` under `no_sync`):

```go
func (i *Instance) Sync() *Sync
func (s *Sync) Enroll(ctx context.Context, hubURL, code string) error
func (s *Sync) AddActor(ctx context.Context, hubToken string) (aid string, err error)
func (s *Sync) LocalToken(aid string) (string, error)
func (s *Sync) Now(ctx context.Context) error
func (s *Sync) Status() ([]byte, error)                 // JSON
func (s *Sync) SetConditions(online, metered, lowPower, background bool)
func (s *Sync) OnEvent(fn func(ev []byte)) (cancel func())
func (s *Sync) Next(sequence string) (int64, error)
func (s *Sync) Rebootstrap(ctx context.Context) error
```

`Options` gains `Sync *SyncOptions{HubURL, Interval, NodeKey []byte}`. `Profile: "nano"`/`"edge"` defaults `TOKI_SYNC_ROLE=spoke` when `HubURL` is set.

`mobile/mobile.go` adds gomobile-safe wrappers: `Handle.SyncEnroll(hubURL, code) error`, `SyncAddActor(hubToken) (string, error)`, `SyncLocalToken(aid) (string, error)`, `SyncNow() error`, `SyncStatus() (string, error)`, `SyncSetConditions(online, metered, lowPower, background bool)`, `SyncSubscribe(EventCallback) (int, error)`, `SyncNext(seq string) (int64, error)`.

Flutter wiring: `connectivity_plus` → `SyncSetConditions`. Android WorkManager periodic task → `SyncNow` in a foreground service. iOS `BGAppRefreshTask` → `SyncSetConditions(…, background=true)` + `SyncNow`.

---

## 7. Security threat model

| # | Threat | Mitigation |
| --- | --- | --- |
| 7.1 | **Compromised spoke** writes beyond its rights | Every change is replayed with the original actor's hub auth record and all rules. Grants are bound to (user, node) and limited to users who logged in on that device. No superuser grants; a superuser SERVICE actor needs `--allow-superuser-actor` and bypasses all rules for what the node pushes as itself (the blast radius of a stolen node key). Policy direction and partition are checked on the hub for the record's state before and after the change, so a spoke cannot move records into or out of its partition. Revoke node → all its grants and reservations are dead. Residual: the device can act as any user who logged in on it within the grant TTL (inherent to offline auth; shorter `TOKI_SYNC_ACTOR_TTL` for sensitive apps). |
| 7.2 | **Replayed pushes** | `(node, origin_seq)` uniqueness makes a replay a `duplicate` with no effect. The handshake signature has a ts window, a per-node nonce cache and a persisted per-node ts floor, and is bound to host and hub id. Session token 15 min. TLS required. |
| 7.3 | **Tombstone bypass** (resurrect a deleted or purged record) | The hub checks `_sync_tombstones` before resolution: `tombstoned`/`legal_tombstone`. Legal tombstones are immutable (DB triggers) and never pruned. The capture hook refuses local creates with a tombstoned id on spokes. `purge` (superuser only on the hub: `POST /api/sync/purge`, `toki sync purge <col> <id> --legal --reason`) deletes the record, writes the legal tombstone, **blanks the patch of every `_changes` row of that record** (erasure), and propagates op `p`. The `p` row goes to EVERY node that pulls the collection, whatever its partition or view rule, so a node that never saw the record learns its id, the collection and the purge time (accepted: the node may hold the record from earlier). A spoke that comes back after `delete` tombstone retention is past retention and must re-bootstrap anyway. |
| 7.4 | **Forged HLC** (future, to always win lww) | `future_hlc` rejects `> hub_now + 5 m`. Drift correction plus re-stamp. A max-skew metric per node. Backdating only lets a change lose. Backdating to sneak under a revocation does not work, because revocation is checked at apply time (§1.6). Backdating to use an EXPIRED grant does not work either: besides `iat - 5 m <= hlc <= exp` the hub requires `hub_now <= exp + TOKI_SYNC_GRACE` (24 h), so a forged HLC only helps inside the grace that honest offline devices need anyway. |
| 7.5 | **Reservation exhaustion** | `max_open_per_node`, `max_block`, the `sync:reserve` rate-limit label, per-sequence global cap and alert at 80%. Ranges are audited, retired on revoke, and values are validated against issued ranges on push. |
| 7.6 | **Crypto fields** | See below. |
| 7.7 | **Exfiltration via pull filters** | Pull scope is computed **on the hub** from policy plus node `params` set by the admin at enrollment. The spoke cannot send filters. `pull_view_rule` intersects with the collection viewRule for the node's service actor. Collections with null rules (superuser only) are never pulled unless policy `trusted=true`. Auth collections never include password/tokenKey. `crypto: strip` withholds encrypted fields. |
| 7.8 | Stolen device data at rest | Out of sync scope. Use crypto with a Keychain/Keystore master key. Revoke the node. |
| 7.9 | Hub key compromise (backup leak) | `TOKI_SYNC_HUB_KEY_FILE` outside `pb_data`. `toki sync rotate-hub-key` re-issues certs at the next handshake (old key accepted for a grace period). |
| 7.10 | Malicious bundle / MITM | TLS with optional SPKI pin. Bundles are only accepted from the authenticated hub session. The bundle hash is checked. |

### 7.6 Encrypted fields: decision for v1

**Ciphertext syncs verbatim. The hub ships collection DEKs to each device, wrapped to its X25519 key.**

- Current crypto (`modules/crypto`): an AES-GCM DEK per collection and version, wrapped by a node-local master key. AAD = collection id, field, record id. Collection ids are identical on all nodes (bundles carry ids), so a ciphertext produced on any node decrypts on any node holding that DEK.
- Handshake `keys[]`: for each collection with encrypted fields that the node may pull (and `crypto != strip`), the hub sends `wrapped = AES-GCM(HKDF(X25519(hub_eph, node_kx)), dek)` per version. The spoke's crypto module re-wraps it under the **local** master key into `_crypto_keys`. The device never learns the hub master key.
- Rotation: a new version appears in the next handshake. Old versions stay for decryption (retired versions on the hub become blank on spokes too).
- Write path under `SyncOrigin`: crypto's `onValidate` decrypts incoming ciphertext (so validators see plaintext) and remembers the original ciphertext per (record ptr, field). `onWrite` stores that ciphertext unchanged when the plaintext equals the decryption. Blind-index rows are recomputed locally (same DEK, same HMAC).

Why not decrypt-on-push / re-encrypt-on-apply:

1. Hashes would differ per node (random nonces), which breaks convergence checks.
2. Plaintext would cross more code paths (the `_changes` patch would need decryption at capture or push).
3. Every hop would cost decrypt plus encrypt.
4. The v1 trade-off (a device holds the DEK of collections it pulls) adds little exposure, because the device already holds those records' plaintext after local decryption.

`crypto: strip` covers devices that must not see a field. "Crypto full" (DEK per device, per-tenant shredding) stays a later phase.

Seam (kernel, §8.1): `kernel.SyncKeyProvider`, implemented by crypto. A spoke without a master key refuses to enroll for collections that need keys (clear error), consistent with crypto refusing plaintext writes.

---

## 8. Compatibility, kernel touch points, build

### 8.1 Kernel touch points (all small, stdlib only)

```go
// kernel/sync_origin.go
type SyncApplyMode uint8
const ( SyncModePush SyncApplyMode = iota + 1; SyncModePull; SyncModeSnapshot; SyncModeBundle )
type SyncOrigin struct {
    Mode     SyncApplyMode
    Node     string   // origin node id
    HLC      uint64
    ChangeID string   // "node:origin_seq"
    Actor    string   // grant aid
    Fields   map[string]any // origin values for autodate fields
}
func WithSyncOrigin(ctx context.Context, o *SyncOrigin) context.Context
func SyncOriginFrom(ctx context.Context) *SyncOrigin          // nil when not a sync apply
func IsSyncReplica(ctx context.Context) bool                  // Mode in Pull/Snapshot/Bundle (side effects must skip)

// kernel/derived_fields.go  (mirror of sensitive_fields.go; computed registers its targets)
func RegisterDerivedField(collectionId, field string)
func UnregisterDerivedField(collectionId, field string)
func IsDerived(collectionId, field string) bool
func DerivedFieldsOf(collectionId string) []string

// kernel/sync_conflict.go  (mirror of batch.go)
const SyncConflictEventName = "sync.conflict"
type SyncConflictEvent struct {
    hook.Event
    App         App
    Collection  *Collection
    RecordID    string
    Current     map[string]any   // public export (redacted per RedactExport)
    CurrentHLC  uint64; CurrentNode string
    Incoming    SyncIncoming     // Op, Node, HLC, BaseHLC, Patch, ActorKind/ActorID/ActorCollection
    FieldClocks map[string]uint64
    // set by handlers:
    Resolution  string           // accept|reject|merge|park
    Patch       map[string]any
    Message     string
}
func OnSyncConflictFor(app App) *hook.Hook[*SyncConflictEvent]
func ReleaseSyncHooks(app App)

// kernel/sync_keys.go
type WrappedKey struct { Collection string; Version int; Wrapped []byte; Retired bool }
type SyncKeyProvider interface {
    ExportKeys(collectionIds []string, recipientX25519 []byte) ([]WrappedKey, error) // hub
    ImportKeys(keys []WrappedKey, localX25519Priv []byte) error                      // spoke
    NeedsKeys(collectionId string) bool
}
func SetSyncKeyProvider(app App, p SyncKeyProvider)
func SyncKeyProviderOf(app App) SyncKeyProvider

// kernel/request_info.go
const RequestInfoContextSync = "sync"

// kernel (sessions seam, like OnAuthTokenIssue)
var SessionActive func(app App, sid string) (bool, error)
```

Non-kernel touch points:

- `apis/sync_exports.go`: `ReplayRecordRequests`.
- `apis/record_crud.go`: `WithoutCancel` context on form/delete (2 lines).
- `modules/crypto`: SyncOrigin ciphertext acceptance and `SyncKeyProvider`.
- `modules/computed`: register derived fields.
- `modules/webhooks`, `modules/wasm` (after hooks): skip `IsSyncReplica`. wasm also adds the `sync.conflict.*` event.
- `modules/sessions`: set `kernel.SessionActive`.
- `tokibase.go`: `sync.Register`, audit sink, `sync.NewCommand`.
- `embed/`, `mobile/`: the sync facade.

### 8.2 REST contract

No change to existing endpoints, shapes or SDK flows. New routes are only under `/api/sync/*` and only when `TOKI_SYNC_ROLE=hub`. New system collections (`_sync_*`) and plain tables appear only when sync is enabled. COMPAT entry: "spokes refuse local schema edits; raw SQL writes are not synced; files are not synced; record `created`/`updated` may carry origin timestamps".

### 8.3 Env

`TOKI_SYNC_ROLE`, `TOKI_SYNC_HUB_URL`, `TOKI_SYNC_HUB_PIN`, `TOKI_SYNC_RETENTION` (90d), `TOKI_SYNC_MIN_KEEP` (24h), `TOKI_SYNC_SPOKE_KEEP` (24h), `TOKI_SYNC_INTERVAL` (30s), `TOKI_SYNC_MAX_DRIFT` (5m), `TOKI_SYNC_ACTOR_TTL` (30d), `TOKI_SYNC_GRACE` (24h), `TOKI_SYNC_PARK_TTL` (30d), `TOKI_SYNC_EVICT_INVISIBLE`, `TOKI_SYNC_PULL_VIEW_RULE`, `TOKI_SYNC_PAGE` (500), `TOKI_SYNC_MAX_BUNDLES` (50), `TOKI_SYNC_HUB_KEY_FILE`, `TOKI_SYNC_NODE_KEY`, `TOKI_SYNC_ALLOW_SUPERUSER_ACTORS`, `TOKI_SYNC_AUDIT_ALL`, `TOKI_SYNC_AUTO_HEAL`, `TOKI_SYNC_INSECURE`, `TOKI_SYNC_TEST`, `TOKI_SYNC_TEST_CLOCK_OFFSET`.

### 8.4 CLI `toki sync`

```
toki sync status [--json]                          # role, hub/node id, epoch, pending, cursors, offset, last error
toki sync peers [--json]                           # hub: nodes, status, lag (seq), last_seen, schema, offset
toki sync enroll --name N --profile P [--param k=v] [--actor col/id]   # hub: prints one-time code
toki sync join <hub-url> <code>                    # spoke: enroll from CLI (edge)
toki sync revoke <node> | toki sync rebootstrap [<node>]
toki sync conflicts [--open] [--collection c] [--json]
toki sync conflicts --resolve <id> --take hub|incoming|patch.json [--note ...]
toki sync policies list|set|rm|lint
toki sync reserve list|create-seq|release
toki sync purge <collection> <id> --legal --reason "..."
toki sync verify [--against-hub]                   # per-collection digests, lists mismatching ids
toki sync compact [--vacuum] | toki sync pause|resume | toki sync now
```

### 8.5 Build tag and profiles

- Files carry `//go:build !no_sync`. `stub.go` and `stub_marker.go` register the marker with `Stubbed=true`.
- Marker collections/tables: `_changes, _sync_meta, _sync_tombstones, _sync_nodes, _sync_cursors, _sync_policies, _sync_conflicts, _sync_reservations, _sync_sequences, _sync_reserved, _sync_actor_grants, _sync_actors, _sync_schema, _sync_state`. Envs `TOKI_SYNC_ROLE`, `TOKI_SYNC_HUB_URL`. Files `sync_node.key`.
- Included in all profiles: nano/edge as spoke, solo/team/cluster as hub. Not in any profile's `no_` list.
- Size: stdlib crypto only (`crypto/ed25519`, `crypto/ecdh`, `crypto/hkdf`), no new dependencies. Estimate +400–700 KiB stripped. nano stays under its 24 MiB CI budget (currently 21.7). Measure in PR10 and update `profiles.txt` notes.
- `profiles_test.go` covers `no_sync` automatically via `TestEachStub` once the stub exists.

### 8.6 Package layout

```
modules/sync/
  sync.go            Register(app), Enabled(), Role(), config from env
  marker.go stub.go stub_marker.go
  schema.go          DDL + system collections
  capture.go         Execute hooks, request stash, patch diff, tombstone guard
  canonical.go       canonical hash
  meta.go            _sync_meta, field clocks
  policy.go          _sync_policies cache (5 s TTL + invalidation, fieldperm pattern)
  identity.go        hub key, certs, enroll, node session tokens, signature verify
  actor.go           grants, assertions, local tokens
  hub_api.go         routes + middleware
  hub_apply.go       apply pipeline
  resolve.go         lww / hub-wins / field-merge / hook
  types.go           counter/set patch ops, mapping to PB modifiers
  pull.go            pull query, partition, view-rule filter, evict
  compact.go         cron via kernel.Jobs
  snapshot.go        hub snapshot pages
  bundle.go          schema versions, bundle build/apply, spoke schema lock
  reserve.go         sequences, ranges, Next()
  tombstone.go       purge, legal
  audit.go           SetAuditSink
  cmd.go             toki sync …
  hlc/hlc.go         HLC + Clock (stdlib only)
  proto/proto.go     wire types (shared by hub and client)
  client/client.go transport.go apply.go rebase.go bootstrap.go conditions.go
embed/sync.go, embed/sync_stub.go
mobile/sync.go
apis/sync_exports.go
tests/e2e/sync.sh, tests/e2e/syncproxy/main.go (toggleable TCP proxy), tests/e2e/parking/main.go (scenario driver)
docs/modules/sync.md
```

---

## 9. Phasing (PRs of about 1 day each)

The suggested PR2 and PR5 were split because each would be more than a day for one agent: transport/identity is separate from push/pull, and snapshot is separate from policies/compaction. Crypto gets its own PR because it touches another module and has its own tests. The result is 11 PRs.

| PR | Scope | Tests |
| --- | --- | --- |
| **PR1** capture + HLC + `_changes` (no network) | `hlc` package. Kernel `sync_origin.go`, `derived_fields.go`, `RequestInfoContextSync`. Module skeleton with marker/stub/`no_sync`. DDL for `_changes`/`_sync_meta`/`_sync_tombstones`/`_sync_state`. Capture hooks incl. request stash actor, patch diff, canonical hash, tombstone guard, tx atomicity. computed registers derived fields. `toki sync status`. | Unit: HLC monotonic/overflow/observe/persist floor. Capture for c/u/d, rollback leaves no row, batch tx, crypto ciphertext captured, computed-only update captures nothing, file fields excluded, canonical hash stable. `TestEachStub`. |
| **PR2** identity + handshake | Hub key, enrollment, device cert, signed handshake, node session token, `_sync_nodes`/`_sync_cursors`, routes skeleton with errors, `toki sync enroll/join/revoke/peers`, clock offset measurement (no enforcement yet). | Unit: cert sign/verify, signature window, nonce replay, revoked node. Integration: in-process hub + spoke via `httptest`. |
| **PR3** push/pull, lww, client loop | `/push`, `/pull`, `/ack`, apply via **direct SaveWithContext as superuser** (temporary, rules in PR4), lww record-level, revert rows, spoke pull apply plus simple rebase, client loop with backoff, realtime `@sync` poke, `wait` long-poll. | Unit: idempotent re-push, gaps, superseded. **e2e `tests/e2e/sync.sh`**: build once, hub + 2 spokes on random ports, writes on both, converge, `toki sync verify` digests equal, kill a spoke mid-push and resume. |
| **PR4** rule re-evaluation + actors + audit | `apis.ReplayRecordRequests`, `record_crud` `WithoutCancel`, actor grants (`/api/sync/actor`, local tokens, service actor), grant checks, `kernel.SessionActive`, autodate preservation, audit sink, webhooks/wasm `IsSyncReplica` skip. | Unit: rule-denied leads to revert. Actor from another node, expired, revoked, superuser refused. fieldperm write rule enforced. batchguard on tx group. Webhook fires once (hub only). SDK e2e suite still passes (`WithoutCancel`). |
| **PR5** conflict strategies + typed fields + hook | field clocks, `field-merge`, `hub-wins`, counter/set capture and replay via PocketBase modifiers plus spoke rebase, `_sync_conflicts`, `toki sync conflicts [--resolve]`, kernel `OnSyncConflictFor`, wasm `sync.conflict.*` event with fail-closed park. | Table-driven resolver tests with the worked examples in §4. Property test: random interleavings of 3 nodes converge to one hash. WASM guest test (Go SDK) for the double-payment hook. |
| **PR6** policies, partitions, tombstones, compaction | `_sync_policies` cache plus validation. Direction checks. Partition `part_old/new`, evict. `pull_view_rule`. Purge and legal tombstones plus patch blanking. Compaction cron, `stale` nodes, `low_water`, 410. | Unit: partition move gives evict, out-of-partition push rejected, legal tombstone immutable, purge erases patches. Compaction keeps the unacked minimum, stale node gets 410. |
| **PR7** snapshot bootstrap + re-bootstrap | `/snapshot` start plus pages, spoke bootstrap (resumable cursor), local change export and rebase, orphaned conflicts, hub epoch plus re-push of kept acked changes, `toki sync rebootstrap`. | e2e: new spoke bootstraps 50k records with a kill mid-snapshot and resume. Spoke older than retention re-bootstraps (fake clock). Hub restore triggers an epoch change and no loss. |
| **PR8** reservations, schema bundles, drift | `_sync_sequences/_reservations`, `/reserve`, `Next()`, `reserve:` field autofill, push range validation. Schema versioning, bundles in the handshake, spoke schema lock, sv field mapping, `schema_dropped_field`. Drift enforcement, re-stamp, `future_hlc`. | Unit: exhaustion returns 503 locally, out-of-range push rejected. Bundle apply idempotent, a field renamed on the hub while the spoke is offline maps correctly. Spoke clock +2 days gets corrected and its changes re-stamped. |
| **PR9** crypto ciphertext sync | `kernel.SyncKeyProvider`, crypto export/import of wrapped DEKs, handshake `keys`, ciphertext acceptance under SyncOrigin, `crypto: strip`. | Unit: hub and spoke with different master keys, same ciphertext and hash on both, blind-index lookup works on the spoke, strip hides the field, spoke without master key refuses enroll. |
| **PR10** nano/edge integration + parking prototype | `embed.Sync` and `mobile` facade, conditions/backoff, `examples/embed` sync demo, profile size measurement, **parking e2e** (below). | e2e `tests/e2e/sync.sh parking` runs in CI (job `e2e-sync`, ~3 min). Size budgets checked. |
| **PR11** hardening / QC | Fuzz the patch/proto decoders. Race tests on capture and loop. Rate limits. Metrics in `/api/health` (`sync_lag`, `pending`, `conflicts_open`). `docs/modules/sync.md`, COMPAT, PROFILES, ARCHITECTURE updates. Threat-model checklist tests (each row of §7 has a test). Chaos run: proxy drops/duplicates/reorders responses for 10 min. | `go test -race ./modules/sync/...`, fuzz 60 s in CI, chaos e2e nightly. |

### 9.1 Parking exit-gate test (PR10)

Setup on one machine:

- hub: `solo` binary, `TOKI_SYNC_ROLE=hub`
- gate-1 and gate-2: `edge` binaries, `TOKI_SYNC_ROLE=spoke`, `params.branch=B1`, service actors `gate_devices/*`
- phone: an `examples/embed` nano test driver with an officer actor grant

Each spoke reaches the hub through `tests/e2e/syncproxy` (a Go TCP proxy with an `/off` and `/on` control port), which simulates network loss.

Schema:

- `tickets{no (reserve:tickets, unique), plate, entry_at, exit_at, status, fee (counter), flags (set)}`, policy `field-merge`, partition by branch
- `payments{ticket, amount, ref}`, policy `hook` (a double-payment guest on the hub)
- `rates`, policy `hub-wins`, pull only

Timeline (fake clock via `TOKI_SYNC_TEST_CLOCK_OFFSET`, advanced in steps of 1 h, 48 steps):

1. All online. Reserve blocks. Bootstrap. Proxies go `/off`.
2. Each simulated hour: each gate creates 20 tickets and closes 15 (with fee increments). The phone adds flags to 5 tickets and records 2 cash payments. The hub changes `rates` once at hour 24. Injected conflicts: the same ticket is closed at gate-2 and flagged/paid on the phone. One double payment. A ticket purged on the hub at hour 30 that gate-1 later edits.
3. Proxies go `/on`. Run sync until all are idle (bounded at 120 s).

Assertions:

- (a) Every created ticket exists exactly once on the hub, and the total equals the sum created on all nodes: **zero loss**.
- (b) No duplicate `no` values.
- (c) `toki sync verify` digests are equal on hub and all spokes for every scoped collection.
- (d) Counters equal the sum of increments.
- (e) The expected conflicts are present and resolved (field-merge auto, hook merge for the double payment).
- (f) The purged ticket is absent everywhere and gate-1's edit is rejected as `legal_tombstone`.
- (g) The phone shows the new rates.
- (h) No webhook duplicates (hub webhook to a local sink counts exactly N events).

---

## 10. Explicit v1 limits (documented)

- Files are not synced: file fields stay local, with a hub warning in `toki sync policies lint`.
- Auth collections are pull-only. No offline sign-up of new users on spokes.
- Raw SQL writes are not captured.
- Pull filters are partition-key only.
- Set concurrency uses hub arrival order (no OR-set).
- `hook` strategy runs on the hub only.
- No spoke-to-spoke sync and no multi-hub.

---

## 11. Open questions for the product owner (max 5)

1. **Partial replication filters.** Is v1 enough with partition-key filters only (`field = @node.param`, one key per collection)? Or must arbitrary rule expressions (for example `branch = @node.branch && status != "archived"`) be supported? Arbitrary expressions need per-node scope tracking tables and roughly +2 PRs.
2. **Rejected offline work.** When a change made legitimately offline is denied at sync time (rule changed, record locked, actor revoked meanwhile), the default is to revert the spoke to hub state and log a conflict. Should some collections instead **park** such changes for manual review (`toki sync conflicts --resolve`)? And should changes stamped before an actor or device revocation be accepted after review, or always dropped?
3. **Offline identity.** Is a hub-issued actor grant with a 30-day TTL, plus a local copy of the user record (no password hash on the device), acceptable for nano? The user must have logged in online once on that phone within the TTL. Alternatively, should a device PIN or local password hash allow first-time offline login (a weaker security trade-off)?
4. **Encrypted fields on devices.** May devices hold the collection DEKs of encrypted collections they pull (v1 design), with `crypto: strip` as the per-collection opt-out? Or must encrypted fields never leave the hub until "crypto full" (DEK per device) lands?
5. **Retention and storage budget.** Is a 90-day `sync.retention` right for the parking and FGR deployments, given worst-case hub growth of about 0.6 GB per 10k changes/day while any spoke is offline for the full window? And are hub-issued reservation gaps (unused numbers lost on revoke or expiry) acceptable for ticket numbering, for example for fiscal or legal ticket sequences?

---

### Critical files for implementation

- `kernel/events.go`, `kernel/db.go` (Execute hook chain, write path, where capture wraps `e.Next()`)
- `apis/batch.go`, `apis/record_crud.go` (replay path for rule re-evaluation, `WithoutCancel` context change, new `apis/sync_exports.go`)
- `modules/crypto/hooks.go` (ciphertext acceptance under `SyncOrigin`, DEK export/import seam)
- `kernel/sensitive_fields.go`, `kernel/batch.go`, `kernel/modulemarkers.go` (patterns for `derived_fields.go`, `sync_conflict.go`, `sync_origin.go` and the `no_sync` marker)
- `embed/embed.go`, `mobile/mobile.go`, `tokibase.go` (profile switches, sync facade, registration and audit sink wiring)
