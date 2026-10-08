# Module `sync`

Phase 3 hub/spoke replication (offline-first). The full design is `docs/SYNC_DESIGN.md`; this page describes what exists today. Package `modules/sync`, subpackage `modules/sync/hlc`.

**Status: PR1 of 11 (change capture only).** There is no network code, no `/api/sync/*` route and no client yet. A node with `TOKI_SYNC_ROLE=hub|spoke` records every write of the synced collections into a local change log. Nothing is sent anywhere.

## What exists now

- `hlc`: the 64-bit hybrid logical clock (48 bit unix ms, 16 bit logical). `Clock` has `Now`, `Observe`, `SetOffset`/`Offset`/`WallNow`, an injected `now`, a spin to the next millisecond on logical overflow, and a persisted floor (`hlc_floor` in `_sync_state`, written every 1000 ticks inside the capture transaction and on terminate).
- Capture hooks on `OnRecord{Create,Update,Delete}Execute` at priority `-1<<19` (outer than crypto, which binds at 0). Each hook runs the write in `RunInTransaction`, so the `_changes` row, the `_sync_meta` row and the tombstone commit or roll back together with the record. Inside the hook `e.App` is replaced by the transaction app; the kernel copies it to the model event before the database write (`syncModelEventWithRecordEvent`), the same mechanism that `kernel.onRecordDeleteExecute` uses. A test proves that a failing inner handler leaves no record and no change row.
- Patch diff, canonical hash, exclusions, empty-patch skip, tombstone guard, `status=local`, atomic group id (`tx`), actor from the request.
- `toki sync status [--json]`.
- Kernel seams (no behaviour change for other modules): `kernel.SyncOrigin` / `WithSyncOrigin` / `SyncOriginFrom` / `IsSyncReplica`, `kernel.RegisterDerivedField` / `UnregisterDerivedField` / `IsDerived` / `DerivedFieldsOf`, and the reserved `@request.context = "sync"` (`kernel.RequestInfoContextSync`). `modules/computed` registers its target fields as derived and unregisters them when a definition is removed or its collection changes.

## Env

| Variable | Meaning |
| --- | --- |
| `TOKI_SYNC_ROLE` | `off` (default), `hub` or `spoke`. With `off` the module registers nothing except its marker: no tables, no hooks, no cost. Unknown values count as `off`. |

The other `TOKI_SYNC_*` variables of the design arrive with the PRs that use them. In PR1 hub and spoke behave identically.

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

`node_id` is a random placeholder (`n` + 14 base32 chars) until PR2 derives it from the device key.

Writes by sync apply paths (`kernel.WithSyncOrigin`, mode pull/snapshot/bundle) update `_sync_meta` and tombstones only and write no `_changes` row. The push mode (hub replay) is a no-op for capture until PR3.

## CLI

```
toki sync status [--json]    # role, node id, pending (status=local), last hlc, hlc floor
```

## Build tag

`-tags no_sync` replaces the module with a stub (`Enabled()` false, `Register` no-op) and a marker that owns the tables above, the collection `_sync_policies`, `TOKI_SYNC_ROLE`, `TOKI_SYNC_HUB_URL` and the file `sync_node.key`. A `no_sync` binary refuses to start on a data dir that has these tables or when the role env is set, unless `TOKI_ALLOW_STUBBED_MODULES=1`. Sync is compiled into every profile; it is in no profile's `no_` list.

## Limits (PR1)

- Raw SQL writes (`app.DB().NewQuery("UPDATE ...")`) are not captured.
- Files are not synced; file fields are not in patches or hashes.
- No network, no apply, no conflict handling, no compaction: `_changes` grows until PR6.
- Hub and spoke are not distinguished yet.
- A derived-only update (computed rollup) bumps the `updated` autodate locally without a change row, so `updated` and the stored hash can differ between nodes after the apply paths exist. The apply side (PR3) must decide how to treat it.
