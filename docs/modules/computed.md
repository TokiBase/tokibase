# Module `computed`

Server-maintained aggregate fields. Totals such as `total_distance_km`, `likes_count` or `members_count` are kept correct by the kernel instead of client code or hand written hooks. Package `modules/computed`. The collection JSON schema is not changed: definitions live in a system collection and target number fields that already exist on the parent collection.

- Always on: `tokibase.go` calls `computed.Register(app)` and adds the `computed` command. With no rows in `_computed_fields` it does nothing.

## Model

System collection `_computed_fields` (main db, all API rules `null` = superusers only).

| Field | Meaning |
| --- | --- |
| `collection` | parent collection, stored as its **id** (a name is accepted on input and converted on save; `list` and the API show the current name) |
| `field` | EXISTING number field on the parent that the module writes |
| `kind` | `count`, `sum`, `avg`, `min`, `max` or `last` |
| `source_collection` | child collection, stored as its id like `collection` |
| `source_relation` | single relation field on the child that points to the parent |
| `source_field` | child number field (required for sum/avg/min/max/last, must be empty for count) |
| `filter` | optional filter on the child, same language as list filters (`status = "done"`) |
| `created`, `updated` | autodate |

Unique index on `(collection, field)`.

Validation (on `toki computed add` and on every save of a `_computed_fields` record, whatever the path): parent and child exist, the parent field is a number field, `source_relation` is a relation to the parent with `maxSelect` 1, `source_field` is a number field, the filter compiles against the child and runs once without error (so a deleted column is caught), it does not reference an encrypted field (the `crypto` module registers those in `kernel.SensitiveFieldsOf`; ciphertext would be compared), `last` needs a `created` field on the child, the child is not a view.

## Kinds

| Kind | Value | No matching child |
| --- | --- | --- |
| `count` | number of children | 0 |
| `sum` / `avg` / `min` / `max` | aggregate of `source_field` (null counts as skipped by SQL) | 0 |
| `last` | `source_field` of the child with the greatest `(created, id)` | 0 |

If the parent field has `onlyInt`, the value is rounded.

Example:

```sh
toki computed add fgr_users total_distance_km --kind sum \
    --source fgr_workout_sessions --relation user --source-field distance_km --filter 'status = "done"'
toki computed add clans members_count --kind count --source clan_members --relation clan
toki computed backfill fgr_users
```

## Maintenance

Bound to the record events `OnRecordAfterCreateSuccess`, `OnRecordAfterUpdateSuccess` and `OnRecordAfterDeleteSuccess` of every collection (they fire after the transaction committed; inside `RunInTransaction` or `/api/batch` they fire after the outer commit). For each definition whose `source_collection` matches, the parents touched by the write are recomputed: the current relation value and, on update, the previous one, so a child moved to another parent fixes both. The previous value is captured per update call (a FIFO per record object, filled in `OnRecordUpdate`, consumed by that update's after-hook, dropped when the update fails or rolls back), so the same record updated several times in one transaction (A to B, then B to C) recomputes A, B and C, and the module's own marked saves leave nothing behind.

Recompute is one aggregate SQL per parent (`SELECT relation, COUNT(*)/SUM/... FROM child WHERE relation IN (?) AND <filter> GROUP BY relation`; the filter is compiled by the kernel filter resolver). `count` uses `COUNT(*)` (answered from the relation index) unless the filter added a join, then `COUNT(DISTINCT id)`. `last` runs `ORDER BY created DESC, _rowid_ DESC LIMIT 1` per parent (ids are random, rowid breaks same-millisecond ties by insertion order) and never loads the group into memory. A failed recompute is retried once after 50 ms (another process may hold the SQLite write lock). The parent is then re-read and saved in ONE transaction, only when the value changed, with `SaveNoValidate` (unrelated fields are not validated) and no auth. Reading and writing in one transaction means a concurrent client edit of the parent is never overwritten with stale data.

- Loop protection: the module's own parent saves carry a context marker; child hooks ignore events with that marker. A self-relation (comments counting replies) therefore does not recurse. Nested rollups (a computed field feeding another definition) are NOT propagated in PR 1.
- Concurrency: recomputes are serialized per `(collection, field, parent id)` with a keyed mutex. Requests that queue behind a running recompute are coalesced: if a recompute that started after the request was made already finished, the request returns without querying.
- Bursts: a batch (`/api/batch`, `RunInTransaction`) of 100 child inserts fires 100 hooks after the commit, but the first recompute already sees all 100 rows and the others find nothing to change, so the parent is written once per field. Sequential, separate requests write the parent once per change (the value really changes each time).
- Errors in the hook are logged, never returned: the child write is already committed. Run `toki computed verify` to find a parent that missed an update.

Parent saves made by the module fire the normal parent hooks, realtime events, webhooks and `updated` autodate.

## Protection

`OnRecordCreateRequest` / `OnRecordUpdateRequest` (priority before all other handlers) reject a body key that targets a computed field (modifiers `field+`, `+field`, `field-` count) with `400`:

```json
{"status":400,"message":"Failed to update record.","data":{"cnt":{"code":"validation_computed_field","message":"This field is maintained by the server and cannot be changed."}}}
```

- Update: re-sending the stored value is not a change and is accepted (SDKs and the Admin UI PATCH whole records). Whatever the body says, the record is saved with the value stored **at write time**: an `OnRecordUpdateExecute` hook (inside the write transaction) resets the computed fields of a client-saved record to the stored values, so a PATCH loaded before a recompute can never write the old number back.
- Create: only a non-zero value is rejected.
- Superusers are rejected too; `TOKI_COMPUTED_ALLOW_MANUAL=1` lets superusers write by hand (the next recompute overwrites it).
- `/api/batch` goes through the same handlers. Go and JS hooks, `$app` and SQL are not covered (the module itself writes through `app.Save`).

## CLI

```
toki computed list [collection] [--json]
toki computed add <collection> <field> --kind K --source C --relation R [--source-field F] [--filter EXPR]
toki computed rm <collection> <field>
toki computed backfill <collection> [field] [--inline]
toki computed verify <collection> [field]
toki computed drift
toki computed index <collection> <field> [--with created]
```

- `add` validates, and updates the definition when `(collection, field)` exists. It does not compute existing values: run `backfill`. It warns when the child has no index starting with the relation column.
- `index <collection> <field>` creates that index through the collection indexes API (`--with created` makes it `(relation, created)`, the best shape for `last`); it is idempotent.
- `rm` deletes the definition; the stored values stay as they are and become writable again.
- `backfill` recomputes every parent in id order, 500 per batch (one grouped query per batch), and writes only the ones that drifted. Without `--inline` it enqueues a `computed.backfill` job when a job queue exists (a worker must run; see `docs/modules/jobs.md`); without a queue, or with `--inline`, it runs in the process and prints progress.
- `verify` reports, per definition, parents scanned and parents where stored != recomputed, with a sample of ids, and writes nothing. Exit code 1 on drift. It reads only `id` and the computed column of the parents, and re-reads every candidate before counting it, so writes that land during the scan are not reported. A definition that no longer matches the schema (deleted column, relation changed to multiple) fails with `definition ... is invalid`.
- `drift` is `verify` over every definition. The same check runs daily at 03:23 (cron `TOKI_COMPUTED_DRIFT_CRON`, `off` disables): through the job queue (`computed.drift`, once per day) when available, inline otherwise. Drift is logged as a warning.

## Audit

Through `computed.SetAuditSink`, wired in `tokibase.go` to the audit log: `computed.def.create|update|delete` (details: kind, source, relation, source field, filter), `computed.backfill` (parents, drift, fixed) and `computed.drift` (drift, parents, sample).

## Consistency

- Eventually consistent right after commit: the parent is recomputed by the after-commit hook, in the same request in the normal case, but a crash between the commit and the hook, or a hook error, leaves a stale value until the next write of that parent's children.
- Exact after `backfill`; `verify` and the daily `drift` check detect the rest.

## Limits

- Same database only: parent and child must be regular collections of the main db (no cross-database, no views).
- Single relation (`maxSelect` 1) only. Multi-relation sources are not supported.
- No nested rollups in PR 1 (a computed field is not a valid `source_field` trigger for another definition: the module's writes are ignored by its own hooks).
- Filters that join multi-valued relations can duplicate rows; `count` then uses `COUNT(DISTINCT id)`, sum/avg do not.
- Collection renames are safe: definitions store collection ids and the cache is dropped on every collection change. Field renames or type changes of the parent field, the relation or `source_field` are not followed: the definition becomes invalid (see `verify`); remove and re-add it.
- Definitions are cached for 5 s per process; `toki computed add` from another process leaves a short window in which the new field is not guarded yet.
- Boot logs a warning when `_computed_fields` has an API rule set: it must stay `null`, otherwise any user could point a definition at an arbitrary number field.
- Performance: one indexed read per touched parent plus one parent write when the value changes. Index the child's relation field (`toki computed index <child> <relation>`), otherwise every recompute scans the child table (`add` and the boot log warn). `last` reads one row per parent; an index on `(relation, created)` makes it constant time. Every child write still re-aggregates the whole group of its parent (O(n) per write), so counters on parents with millions of children are expensive: there is no incremental +1/-1 mode yet. Hot parents (one parent receiving thousands of writes per second) serialize on the keyed mutex and on SQLite's single writer.
