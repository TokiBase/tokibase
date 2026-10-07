# Module `fieldperm`

Per-field read and write rules, in the same language as collection rules (`@request.auth.*`, `@request.body.*`, `@collection.*`, relations, macros). Package `modules/fieldperm`. The collection JSON schema is not changed, so the Admin UI and the SDKs keep working.

- Always on: `tokibase.go` calls `fieldperm.Register(app)` and adds the `fieldperm` command. With no rows in `_field_rules` it does nothing.
- Field rules can only narrow access. They run in addition to the collection rules, never instead of them: a record that the collection rule hides stays hidden whatever the field rules say.

## Storage

System collection `_field_rules` (main db, all API rules `null` = superusers only).

| Field | Type | Meaning |
| --- | --- | --- |
| `collection` | text | collection name (collection id also matches) |
| `field` | text | field name |
| `read_rule` | json | `null`, `""` or a rule string (stored as a JSON value so that `null` and `""` stay distinct; a text field cannot) |
| `write_rule` | json | same |
| `note` | text | free text |
| `created`, `updated` | autodate | |

Unique index on `(collection, field)`. The module reads all rows into memory. The cache is invalidated on every create/update/delete event of `_field_rules` and at bootstrap, and also expires after 5 s so that changes made by another process (the CLI against a running server) are picked up: tightening a rule from the CLI takes effect on a running server within 5 s (instantly for rules changed through the server itself).

Fail closed on load errors: if the rules cannot be loaded even once (DB busy, `_field_rules` missing), non-superusers get every field of every record hidden except `id`, expand is dropped and writes are refused with `503` until a load succeeds (we cannot know which fields are protected). A failed load is remembered for 5 s (no retry storm, no DB I/O under the lock). After a good load, a failed refresh keeps serving the previous rules.

## Semantics

| Value | `read_rule` | `write_rule` |
| --- | --- | --- |
| `null` | inherit: no field rule | inherit: no field rule |
| `""` | always readable (explicit "public"; no effect beyond documentation) | locked: only superusers may change the field |
| rule | field is removed from the response when the rule is false for the current auth and record | checked when the field is present in the request body; rejected when false |

Superusers bypass both reads and writes. With env `TOKI_FIELDPERM_SUPERUSER=enforce` they are subject to the rules too (a locked field then cannot be changed through the API even by a superuser; rules are evaluated for the superuser auth, so `@request.auth.id = leader` fails for them).

A rule that fails to evaluate (typo, unknown field) fails closed: the field is hidden on read, the write is rejected, and a warning is logged. Run `toki fieldperm lint` to find those.

## Read enforcement

Bound to `OnRecordEnrich`, which upstream calls for every record that leaves through the record API: list, view, the create/update response, realtime events and every expanded relation (`expand=` enriches each related record with the same request info, context `expand`). After the rest of the enrich chain (including upstream's "superusers see hidden fields" step) the hook evaluates each field's rule against the stored record and calls `record.Hide(field)`. Hidden fields are simply absent from the JSON. Cost: one small query per protected field per record, only for collections that have rules, except that rules which cannot depend on the record (only `@request.auth.*`, `@request.context/method/headers/query`, datetime macros and literals; anything else, including field names, `@request.body.*` and `@collection.*`, counts as record-dependent) are evaluated once per (collection, field) and request.

Relations: when a relation field is hidden, its entry is also removed from `expand`, at every depth of the expand tree (single, nested `a.b`, list, auth responses and realtime `options.query.expand`). Back-relation expands (`posts_via_clan`) drop the related records whose relation field is hidden. When request info is missing (custom code), protected fields are hidden (fail closed).

## Write enforcement

Bound to `OnRecordCreateRequest` and `OnRecordUpdateRequest`, before `e.Next()`. For every key of the submitted body (modifiers `field+`, `+field`, `field-` count as `field`; uploaded files too) that has a `write_rule`:

- update: a key whose submitted (resolved) value equals the stored value is not a change and is skipped (SDKs that PATCH the whole record keep working); file fields are always evaluated. Otherwise the rule is evaluated against the ORIGINAL stored record (so "only the current leader may change `leader`" works), with the request info (`@request.body.*` holds the resolved values).
- create: the rule is evaluated against the submitted data using the same one-row-CTE technique as upstream's create rule.

Denial is `400` in the upstream validation shape, all denied fields listed:

```json
{"status":400,"message":"Failed to update record.","data":{"leader":{"code":"validation_field_not_allowed","message":"You are not allowed to change this field."}}}
```

(`Failed to create record.` on create.)

`/api/batch` sub-requests run through the same record handlers and hooks, so they are covered (verified by a test: a batch PATCH of a locked field is rejected and the record is unchanged).

## Audit

Every denied write calls the sink set with `fieldperm.SetAuditSink`, wired in `tokibase.go` to the audit log with action `field.denied` (`details`: field, op, user, user_collection, ip). Sampled: at most one entry per minute per (collection, field, user); the log line is sampled the same way.

## CLI

```
toki fieldperm list [collection] [--json]
toki fieldperm set <collection> <field> [--read inherit|public|<rule>] [--write inherit|locked|<rule>] [--note text]
toki fieldperm rm <collection> <field>
toki fieldperm lint [--json]
```

`set` only changes the flags that are given (a new row starts as inherit/inherit) and rejects unknown collections and fields. `lint` reports unknown collections, unknown fields and rules that do not parse against the collection; exit code 1 when anything is reported. `toki rule lint` (ruleguard) is unrelated and unchanged.

## Example: clan leader

Before, a hook guarded `leader` by hand. Now:

```sh
toki fieldperm set clans leader --write '@request.auth.id = leader'
toki fieldperm set clans invite_code --read 'members.id ?= @request.auth.id'
```

Only the stored leader can change `leader` (also through `leader+` modifiers and batch); everyone else gets the 400 above. Other members never see `invite_code`.

## Limits

- Scope is the record API. Not covered: files served from `/api/files/...` (a file field's URL stays fetchable by whoever knows it; protected file tokens are a separate mechanism), custom routes and JS/Go hooks that serialize records without `EnrichRecord`, direct `$app`/SQL access, backups.
- A hidden field can still be used in `filter=` and `sort=` of list requests by users that can list the collection, which allows inferring its value one comparison at a time. Combine with a list rule when the value is secret.
- Rules apply to the submitted keys only; a field changed by a hook or by another field's side effect is not checked. Write paths outside `OnRecordCreateRequest`/`OnRecordUpdateRequest` are not covered: email-change/verification/password-reset confirm, OAuth2 link/update of mapped fields, relation cascades, Go/JS hooks. On OAuth2 first login the server-mapped fields (`name`, `avatar`, ...) go through the create hook and ARE evaluated: a locked or ruled `name` makes OAuth2 sign-up fail with 400, so do not put a write rule on fields mapped by OAuth2.
- Known limit (F3): file downloads from `/api/files/...` ignore field read rules (see first bullet).
- No rule inheritance across collections; collection renames need the `_field_rules.collection` row updated (collection ids also match).
