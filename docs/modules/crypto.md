# Module `crypto`

Per-field encryption at rest with envelope keys. Package `modules/crypto`. The collection JSON schema is not changed, so the Admin UI and the SDKs keep working. Scope of PR 1: field modes `random` and `blind-index` for text-like fields, key management, rotation. Crypto-shredding by tenant/user comes later.

- Always registered by `tokibase.go`, but inactive without a master key. With no rows in `_crypto_fields` it does nothing.
- Only `text`, `editor`, `json`, `email` and `url` fields can be encrypted. System fields (`id`, auth `email`/`password`/`tokenKey`, ...), view collections and the system collections are refused. `blind-index` is not available on `json` fields.

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

`_crypto_keys` (main db, system collection, rules `null`): `collection` (id), `version`, `wrapped_dek`, `created`, `retired_at`; unique `(collection, version)`. Retiring a version blanks `wrapped_dek` (crypto-erase of that key).

`_crypto_fields` (main db, system collection, rules `null`): `collection` (id; names are accepted when written by hand), `field`, `mode` (`random|blind-index`), `created`; unique `(collection, field)`. Cached in memory like `fieldperm` (5 s TTL plus invalidation on change; CLI changes reach a running server within 5 s).

`_crypto_index` (plain table in the main db, not a collection): `collection, field, record, hmac, ver`, primary key `(collection, field, record)`, index on `(collection, field, hmac)`. The HMAC is HMAC-SHA256 over `field NUL value` with a key derived from the DEK, so it never equals across collections or fields. A lookup computes the HMAC under every usable key version, so rows indexed under an older version stay findable until rotation rewrites them.

## Modes

| Mode | Stored | Filter/sort | Lookup |
| --- | --- | --- | --- |
| `random` | fresh nonce per write, same value gives different ciphertexts | rejected | none |
| `blind-index` | like `random`, plus an HMAC row in `_crypto_index` | rejected in PR 1 | exact match: `crypto.FindByBlindIndex` and `GET /api/crypto/lookup/{collection}/{field}?value=` |

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

A value that cannot be decrypted (tampered, retired key version, wrong master key, no master key) becomes `""` in the response and is logged at Error with the record id; `crypto.decrypt_failed` goes to the audit sink, sampled at one per minute per (collection, field). Nothing panics, other fields and records are unaffected. A later update that does not touch the broken field leaves its stored value as it is.

## Filters, sort, lookup

Ciphertext is random, so `filter=` and `sort=` on an encrypted field are **rejected in PR 1** with HTTP 400:

```json
{"status":400,"message":"Failed to load the records.","data":{"filter":{"code":"validation_encrypted_field","message":"Encrypted field \"patients.diagnosis\" cannot be used in filter."}}}
```

(`sort` for sorts.) The guard is a router middleware on `GET /api/collections/{c}/records` that scans identifiers of the query (string literals are ignored; modifiers like `:lower`, relation paths `author.secret`, back-relations `posts_via_author.secret` and `@collection.x.secret` are followed). It applies to everybody including superusers.

For exact match use the lookup endpoint (any `blind-index` field): it returns `{"items":[...],"totalItems":n}`, at most 100 records, each already passing the collection's `listRule` for the caller (superusers see all; a `null` rule is superusers only), enriched like a normal list (decrypted, field rules applied). Go: `crypto.FindByBlindIndex(app, collection, field, value)` returns the matching records as stored, without any rule check. Matching is exact and case sensitive; normalise (trim, lowercase) in your own write path if you need otherwise.

## CLI

```
toki crypto status [--json]
toki crypto enable <collection> <field> [--mode random|blind-index] [--background]
toki crypto disable <collection> <field> --i-understand
toki crypto rotate <collection> [--background]
toki crypto retire <collection>
toki crypto verify <collection> [--sample 100] [--json]
```

- `status`: master key present or missing, whether the key file is inside `pb_data`, superuser plaintext switch, configured fields and modes, key versions, lint warnings.
- `enable`: refuses without a master key and for ineligible fields; creates DEK v1, stores the configuration, then encrypts the existing rows in batches of 500 (progress on stderr). Calling it again with the same mode resumes (idempotent); another mode is refused until `disable`. The sweep uses a compare-and-swap `UPDATE` per value and no record hooks (no `updated` bump, no webhooks or realtime events), then waits 6 s for running servers to refresh their cache and sweeps once more. `--background` enqueues kernel job `crypto.reencrypt` (needs a worker; the queue comes from `modules/jobs`).
- `disable`: decrypts all rows, removes the configuration (and index rows). Plaintext is then in the DB and every later backup, hence `--i-understand`.
- `rotate`: creates DEK version n+1, re-encrypts every encrypted field of the collection and rebuilds the indexes. Old versions stay readable until `retire`.
- `retire`: destroys (blanks) every non-active key version, but only when a count of rows still holding `tkc1:<ver>:` is zero for all fields; otherwise keeps it and says why (exit code 1).
- `verify`: decrypts a random sample, checks blind-index rows, reports plaintext values in encrypted fields and failures; exit code 1 on any problem.

## Rotation procedure

1. `toki crypto verify <collection>` (baseline is clean).
2. `toki crypto rotate <collection>`. Running servers pick up the new key within 5 s; values written meanwhile with the old version are caught by the second sweep.
3. `toki crypto verify <collection>`.
4. `toki crypto retire <collection>` once no row uses the old version. Backups taken before the rotation still need the old DEK, which retire destroys: keep a pre-rotation backup only if you also keep the old wrapped key (`_crypto_keys` is inside that backup).

## Lint and boot warnings

At boot the module logs a warning for every collection whose API rule (list/view/create/update/delete/auth/manage) or index references one of its encrypted fields: rules compare against ciphertext, indexes on random data are useless and a unique index cannot work. It logs an Error when fields are configured but no master key is set. `toki crypto status` prints the same findings.

## Audit

`crypto.SetAuditSink` is wired to the audit log in `tokibase.go`: `crypto.enable`, `crypto.disable`, `crypto.rotate`, `crypto.retire` and sampled `crypto.decrypt_failed` (details: field, mode/version/error; never a value).

## Backups and replicas

Ciphertext is what is stored, so `toki backup` archives, WAL replicas and Litestream carry ciphertext only; `toki backup verify` still checks integrity (it never needs plaintext). The master key is not in `pb_data` and so not in backups: store it in your secret manager and back it up separately. Restoring a backup on a node needs the same master key. `_crypto_keys` holds the wrapped DEKs and travels with the data on purpose.

## Limits (PR 1)

- No filter/sort on encrypted fields (rejected), no range or prefix search, no uniqueness on encrypted fields.
- The filter guard scans the request, it does not rewrite the filter. It covers record list requests; collection rules and view queries that reference an encrypted field are only linted. Fields reached through a relation are checked only when the path resolves from the listed collection.
- Direct SQL, `$app.db()`, exports, the logs of your own hooks and the in-memory process see ciphertext or plaintext according to where they read; webhooks and audit entries built from `record` inside create/update hooks carry ciphertext.
- Files are not encrypted. Empty values and value lengths are visible.
- No master-key rotation, no per-tenant/per-user keys (crypto-shredding), no `random` text pattern/unique support.
