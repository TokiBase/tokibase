# Module `crypto`

Per-field encryption at rest with envelope keys. Package `modules/crypto`. The collection JSON schema is not changed, so the Admin UI and the SDKs keep working. Scope (PR 1 + PR 2): field modes `random` and `blind-index` for text-like fields, key management, rotation. Crypto-shredding by tenant/user comes later.

- Always registered by `tokibase.go`, but inactive without a master key. With no rows in `_crypto_fields` it does nothing.
- Only `text`, `editor`, `json`, `email` and `url` fields can be encrypted. Refused: system fields (`id`, auth `email`/`password`/`tokenKey`, ...), view collections and the system collections, auth identity fields (`passwordAuth.identityFields`, `username`) and OAuth2 mapped fields (login would compare against ciphertext), fields covered by any index (a unique index would apply to random ciphertext, any other index is useless) and fields selected by a view query. `blind-index` is not available on `json` fields.

## Threat model

| Protects | Does not protect |
| --- | --- |
| `data.db` copies, `toki backup` archives, Litestream/WAL replicas, a stolen disk or snapshot (they hold ciphertext; the master key lives elsewhere) | A running server: it holds the master key in memory and decrypts for every API response |
| Superuser browsing of the Admin UI, only when `TOKI_CRYPTO_ADMIN_PLAINTEXT=off` (see below) | Go/JS hooks, custom routes and anything with `$app` access: they can call `crypto.Decrypt`, or read the key from the environment |
| Tampering: AES-GCM authenticates each value and binds it to collection, field and record id (a ciphertext copied to another row fails to decrypt) | Metadata: row counts, which rows are empty (empty values are stored empty), value lengths (ciphertext is plaintext length + 28 bytes + prefix), access patterns |
| | `blind-index`: an attacker with the DB can see which rows share a value (equal values have equal HMACs) and can test guesses only if they also have the master key |

The Admin UI goes through the API and so sees plaintext by default. `TOKI_CRYPTO_ADMIN_PLAINTEXT=off` makes API responses to superusers (list, view, realtime, expand, lookup endpoint) show ciphertext for encrypted fields. Saving such a record back from the Admin UI is safe: an unchanged ciphertext is recognised and kept. Other users still get plaintext according to the collection rules.

## Key hierarchy

```
master key (32 bytes, outside pb_data)
  wraps -> DEK per collection and version (32 bytes random, AES-256-GCM, AAD "tkc-dek|collection id|version")
              encrypts -> field values (AES-256-GCM, 12 byte random nonce)
              derives (HKDF-SHA256) -> blind-index key of the collection
```

Master key: `TOKI_CRYPTO_MASTER_KEY` (base64, 32 bytes; std or URL alphabet) or `TOKI_CRYPTO_MASTER_KEY_FILE` (path to a file holding the base64 string). Generate one with `head -c32 /dev/urandom | base64`. **Store it outside `pb_data` and back it up separately.** Without it every encrypted value is unrecoverable; `toki crypto status` warns when the key file sits inside `pb_data`. There is no master-key rotation in PR 1.

Stored value: `tkc1:<keyver>:<base64 nonce||ciphertext||tag>`; AAD = collection id, field name and record id (NUL separated). For `json` fields the column holds the JSON string of that value.

`_crypto_keys` (main db, system collection, rules `null`): `collection` (id), `version`, `wrapped_dek`, `created`, `retired_at`; unique `(collection, version)`. Retiring a version blanks `wrapped_dek` (key retirement, see below). A row with `version = -1` is the advisory operation lock of the collection (see "Locking and resuming"); key reads ignore it.

`_crypto_fields` (main db, system collection, rules `null`): `collection` (id; names are accepted when written by hand), `field`, `mode` (`random|blind-index`), `state` (`""`, `enabling`, `disabling`), `created`; unique `(collection, field)`. Cached in memory like `fieldperm` (5 s TTL plus invalidation on change; CLI changes reach a running server within 5 s).

`_crypto_index` (plain table in the main db, not a collection): `collection, field, record, hmac, ver`, primary key `(collection, field, record)`, index on `(collection, field, hmac)`. The HMAC is HMAC-SHA256 over `field NUL value` with a key derived from the DEK, so it never equals across collections or fields. A lookup computes the HMAC under every usable key version, so rows indexed under an older version stay findable until rotation rewrites them.

## Modes

| Mode | Stored | Filter/sort | Lookup |
| --- | --- | --- | --- |
| `random` | fresh nonce per write, same value gives different ciphertexts | rejected | none |
| `blind-index` | like `random`, plus an HMAC row in `_crypto_index` | equality only (`=`, `!=`, `?=`, `?!=` against a non-empty string literal), rewritten to an index lookup (PR 2); sort rejected | exact match: `crypto.FindByBlindIndex` and `POST /api/crypto/lookup/{collection}/{field}` with body `{"value":"..."}` |

Empty values (`""`, `null`) are stored empty, not encrypted.

## Write path

Bound to the kernel model hooks `OnRecordCreateExecute` / `OnRecordUpdateExecute`, so every writer is covered: the REST API, `/api/batch`, Go and JS hooks (`$app.save`), MCP, OAuth2 sign-up. It is not a request hook. Before the DB write each configured field is encrypted in place (a field whose plaintext equals the stored one keeps its stored ciphertext, so unrelated updates do not churn nonces). After the write the blind index is updated; on delete the index rows are removed.

Because the validator must see plaintext, an `OnRecordValidate` hook decrypts untouched ciphertext first (email/url/pattern/max length rules run on the plaintext).

If the collection has encrypted fields and no master key is available, creates and updates are **refused** (plaintext is never stored in an encrypted field). If the configuration cannot be loaded even once, writes fail closed too.

Index write failure after the data write returns an error to the caller and logs at Error; the lookup verifies every hit by decrypting, so a stale index row can only cause a missed match, never a wrong record. `toki crypto enable <c> <f>` (resume) and `rotate` rebuild the index.

## Read path

There is no generic "after find" hook, so decryption happens where records leave the system:

- `OnRecordEnrich`: every API response (list, view, create/update response, realtime, expand, auth). Plaintext replaces ciphertext in the in-memory record that is about to be serialized.
- `crypto.Decrypt(app, record)` (Go) for consumers that have a record loaded through `app.Find*`: replaces ciphertext by plaintext in place and returns the joined errors of values it had to blank.

**`record.Get(field)` inside Go/JS hooks and after `app.FindRecordById` returns ciphertext unless `Decrypt` was called.** After `Decrypt`, saving the record again is fine: unchanged plaintext reuses the stored ciphertext, edited plaintext is re-encrypted.

A value that cannot be decrypted (tampered, retired key version, wrong master key, no master key, transient key-load error) becomes the sentinel string `"[undecryptable]"` in the response (for a `json` field the JSON string of it) and is logged at Error with the record id; `crypto.decrypt_failed` goes to the audit sink, sampled at one per minute per (collection, field). Nothing panics, other fields and records are unaffected. It is a sentinel and not `""` on purpose: a client that PATCHes the whole record back sends the sentinel, and the write hook then keeps the stored ciphertext untouched, so a read failure can never erase data. (For `email`/`url` fields the sentinel fails type validation, so such a whole-record PATCH is rejected with 400 instead; send only the fields you change.) Expanded relations are decrypted as well, also by `crypto.Decrypt` (it walks the expand tree).

## Where plaintext must not go

The module registers every configured field in the kernel registry `kernel.RegisterSensitiveField` (kept in sync with `_crypto_fields`). Modules that copy records elsewhere consult it and replace the value with `[encrypted]`: the audit log (`before`, `after`, `diff`; so neither plaintext nor ciphertext is stored, and a change of an encrypted field is no longer visible in the diff), webhook payloads (`data`, `old`; `changed` still names the field) and MCP exports and delete previews. The registry is refreshed whenever the configuration cache reloads (at most 5 s after a change, and on every record write before the audit entry is built).

## Filters, sort, lookup

Ciphertext is random, so most expressions on an encrypted field are **rejected** with HTTP 400:

```json
{"status":400,"message":"Failed to load the records.","data":{"filter":{"code":"validation_encrypted_field","message":"Encrypted field \"patients.diagnosis\" cannot be used in filter."}}}
```

(`sort` for sorts.) The guard is a router middleware on `GET /api/collections/{c}/records` that parses the filter and scans identifiers of the query (string literals are ignored; modifiers like `:lower`, relation paths `author.secret`, back-relations `posts_via_author.secret` and `@collection.x.secret` are followed). It applies to everybody including superusers.

### Equality on `blind-index` fields (PR 2)

Exactly this shape is let through and works:

```
ssn = "123-45"          ssn != "123-45"          ssn ?= "123-45"          ssn ?!= "123-45"
"123-45" = ssn          author.ssn = "123-45"    posts_via_author.ssn ?= "x"   @collection.patients.ssn = "x"
```

- The field is compared (either side) with a **non-empty string literal**. Works inside `&&`, `||` and parentheses, and in collection rules with a string bound from `@request.query.*` / `@request.body.*` / `@request.auth.*` too.
- Everything else on an encrypted field stays rejected: `~ !~ < <= > >=` (and the `?` variants), modifiers (`:lower`, `:length`, ...), `random`-mode fields, numbers, `null`, the empty string, a comparison between two fields, function arguments (`strftime(ssn)`), `sort`.
- How it works: the kernel field resolver asks a `kernel.BlindIndexProvider` (registered by this module in the app store; the kernel knows nothing about encryption, modules do not import each other) whether the field is blind-indexed. If so, its identifier carries a `search.ResolverResult.BeforeBuild` hook; when the expression builder sees `=`, `!=`, `?=` or `?!=` against a bound non-empty string it calls the module with the plaintext, the module computes the HMAC under **every usable key version** (so rows indexed under an older version still match during a rotation), reads `_crypto_index`, verifies every hit by decrypting (a stale index row can only cause a missed match) and the resolver emits `CASE WHEN <alias>.id IN (<matching ids>) THEN <value> ELSE '' END` as the compared identifier. Because only the identifier changes, the legacy and the AST (`TOKI_RULE_AST=1`) paths, relation paths (multi-match `=` needs all related records to match, `?=` any), NULL handling and bound parameters behave exactly as for a plain column. Expressions that do not touch a blind-index field produce byte-identical SQL.
- At most 1000 records may match one comparison (the ids are inlined as bound parameters); more is an error (HTTP 400 in a filter). The comparison needs the index to be complete: while `toki crypto enable` is still sweeping (state `enabling`) rows not yet indexed are not found.
- **No equality oracle.** A client filter of a non-superuser applies the visibility rule of the lookup endpoint: for each matching record the field must survive the `OnRecordEnrich` hooks (`fieldperm` read rules, hooks that hide it). A record whose field the caller cannot read behaves as if it never matched: `ssn = "v"` does not return it and `ssn != "v"` does not exclude it (excluding it would reveal the value just as well). A hidden field is not filterable by non-superusers at all (the usual error, independent of the data). Superusers (and rules, which are written by the admin and resolved with hidden fields allowed) are not gated; note that this means `fieldperm` with `EnforceSuperuser` is not applied to a superuser's filter, unlike the lookup endpoint. The comparison costs one index query and, for non-superusers, one enrich pass over the matching records.
- **Collection rules** (`listRule`, `viewRule`, ...) use the same resolver, so `ssn = "123-45"` or `ssn = @request.query.s` in a rule works the same way, ungated. Other operators in a rule on an encrypted field are not rewritten and compare against ciphertext as before (`toki crypto status` lints such rules).
- Matching is exact and case sensitive, like the lookup endpoint.

For exact match use the lookup endpoint (any `blind-index` field). Send the value in a **POST** JSON body (`{"value":"..."}`; strings, or numbers): request URLs are stored in the request log, request bodies are not. `GET ...?value=` still works for compatibility, but then the looked-up plaintext (SSN, phone, email) lands in the logs table; do not use it for sensitive values. The endpoint has no rate limit of its own: configure one for the path prefix `/api/crypto/` in the settings rate limits, a low-entropy value against a public `listRule` can be enumerated. It returns `{"items":[...],"totalItems":n}`, at most 100 records, each already passing the collection's `listRule` for the caller (superusers see all; a `null` rule is superusers only), enriched like a normal list (decrypted, `fieldperm` read rules and hidden fields applied). A caller who cannot read the looked-up field (hidden field, `fieldperm` denies) gets no records for it: the endpoint is not an equality oracle on fields the caller cannot see. Go: `crypto.FindByBlindIndex(app, collection, field, value)` returns the matching records as stored, without any rule check. Matching is exact and case sensitive; normalise (trim, lowercase) in your own write path if you need otherwise.

## CLI

```
toki crypto status [--json]
toki crypto enable <collection> <field> [--mode random|blind-index] [--background]
toki crypto disable <collection> <field> --i-understand
toki crypto rotate <collection> [--background]
toki crypto retire <collection>
toki crypto verify <collection> [--sample 100] [--json]
toki crypto resume
```

- `status`: master key present or missing, whether the key file is inside `pb_data`, superuser plaintext switch, configured fields and modes, key versions, `PENDING` interrupted operations, lint warnings.
- `enable`: refuses without a master key and for ineligible fields; creates DEK v1, stores the configuration, then encrypts the existing rows in batches of 500 (progress on stderr). Calling it again with the same mode resumes (idempotent); another mode is refused until `disable`. The sweep uses a compare-and-swap `UPDATE` per value and no record hooks (no `updated` bump, no webhooks or realtime events), then waits 6 s for running servers to refresh their cache and sweeps once more. `--background` enqueues kernel job `crypto.reencrypt` (needs a worker; the queue comes from `modules/jobs`).
- `disable`: crash-safe. It first sets the configuration row to `state=disabling` (servers then store new values of the field as plaintext, reads still decrypt), waits for running servers to refresh, decrypts all rows, and deletes the index rows and the configuration row only after a final check finds zero ciphertext. If the process dies or a row cannot be decrypted, the row stays in `disabling`; fix the cause and run `disable` or `resume` again. Plaintext is then in the DB and every later backup, hence `--i-understand`.
- `rotate`: creates DEK version n+1, re-encrypts every encrypted field of the collection and rebuilds the indexes. Old versions stay readable until `retire`.
- `retire`: blanks the wrapped key of every non-active version, but only when no row holds `tkc1:<ver>:` in ANY text-like column of the collection (configured or not, so a hand-deleted config row cannot make it destroy a key in use); otherwise keeps it and says why (exit code 1). It refuses for `2 x 5 s` after the newest key was created, so that a server with a stale key cache cannot still write the old version after the check.
- `resume`: finishes interrupted `enable` (`enabling`), `disable` (`disabling`) and `rotate` runs, and releases stale locks. Rerunning `rotate` does not start another key; `resume` re-encrypts to the existing active key.
- `verify`: decrypts a random sample, checks blind-index rows, reports plaintext values in encrypted fields and failures; exit code 1 on any problem.

## Locking and resuming

`enable`, `disable`, `rotate`, `retire` and `resume` take an advisory lock per collection (a `_crypto_keys` row with `version = -1`; the unique index makes it atomic across processes). A second operation on the same collection is refused with a message naming the running or interrupted one. A lock older than 6 hours counts as abandoned. A failed `rotate` keeps its lock on purpose: that is the persisted "rotating" state that `toki crypto resume` finishes. `enable` and `disable` persist their state in `_crypto_fields.state` instead.

## Schema changes on encrypted fields

The field name is part of the AAD and `_crypto_fields` refers to it by name, so changing it would orphan the ciphertext and make new writes land in plaintext. A hook on collection create/update therefore **refuses** (validation error `validation_encrypted_field_schema`) to rename, delete, retype or replace-by-name a configured field. Run `toki crypto disable <collection> <field> --i-understand` first, change the schema, `enable` again. Renaming the collection itself is fine (configuration, keys and AAD use the collection id). Deleting a whole collection is allowed and removes its configuration, keys and index rows. Saving a view whose query names a table with an encrypted field and either the column or `*` is refused as well (a view returns ciphertext), and so is enabling a field a view already selects.

## Rotation procedure

1. `toki crypto verify <collection>` (baseline is clean).
2. `toki crypto rotate <collection>`. Running servers pick up the new key within 5 s; values written meanwhile with the old version are caught by the second sweep.
3. `toki crypto verify <collection>`.
4. `toki crypto retire <collection>` once no row uses the old version. Backups taken before the rotation still need the old DEK, which retire blanks: keep a pre-rotation backup only if you also keep the old wrapped key (`_crypto_keys` is inside that backup).

Retirement is not erasure. Blanking `wrapped_dek` does not remove older copies of the wrapped key from SQLite free pages, the WAL, backups, Litestream or replicas. Data becomes unrecoverable only when the master key (and every copy of the old wrapped key) is destroyed.

## Lint and boot warnings

At boot the module logs a warning for every collection whose API rule (list/view/create/update/delete/auth/manage) or index references one of its encrypted fields: rules compare against ciphertext, indexes on random data are useless and a unique index cannot work. It logs an Error when fields are configured but no master key is set. `toki crypto status` prints the same findings.

## Audit

`crypto.SetAuditSink` is wired to the audit log in `tokibase.go`: `crypto.enable`, `crypto.disable`, `crypto.rotate`, `crypto.retire` and sampled `crypto.decrypt_failed` (details: field, mode/version/error; never a value).

## Backups and replicas

Ciphertext is what is stored, so `toki backup` archives, WAL replicas and Litestream carry ciphertext only; `toki backup verify` still checks integrity (it never needs plaintext). The master key is not in `pb_data` and so not in backups: store it in your secret manager and back it up separately. Restoring a backup on a node needs the same master key. `_crypto_keys` holds the wrapped DEKs and travels with the data on purpose.

## Limits

- Filters on encrypted fields: only equality on `blind-index` fields (see above); no sort, range, prefix or substring search, no uniqueness on encrypted fields.
- The filter guard parses the request filter and only lets the equality shape through; the rewrite itself happens in the field resolver, so it also applies to rules and Go `FindRecordsByFilter`. The guard covers record list requests; collection rules and view queries that reference an encrypted field are only linted. Fields reached through a relation are checked only when the path resolves from the listed collection.
- Direct SQL, `$app.db()`, exports, the logs of your own hooks and the in-memory process see ciphertext or plaintext according to where they read. Audit, webhooks and MCP replace encrypted fields by `[encrypted]`; your own Go/JS hooks that copy `record` elsewhere must do the same (`kernel.IsSensitive(collectionId, field)`).
- Files are not encrypted. Empty values and value lengths are visible.
- No master-key rotation, no per-tenant/per-user keys (crypto-shredding), no `random` text pattern/unique support.
