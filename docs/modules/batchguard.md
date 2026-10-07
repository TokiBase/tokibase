# Module `batchguard`

Cross-record validation for atomic batches. `/api/batch` is already one transaction, but collection rules see one record at a time. Checkout style invariants (order + items + stock in one call, wallet transfers) need a check that sees the whole batch. Package `modules/batchguard`.

- Always on: `tokibase.go` calls `batchguard.Register(app)` and adds the `batch` command. With no rows in `_batch_rules` and no `kernel.OnBatchFor(app)` handler it adds no work to a batch. Build tag `no_batchguard` drops it, but the binary then REFUSES TO BOOT when a `_batch_rules` table already exists (guards would be silently off); set `TOKI_ALLOW_STUBBED_MODULES=1` to override (logged at ERROR, guards OFF). Superusers are NOT exempt from rules.

## Model

System collection `_batch_rules` (main db, all API rules `null` = superusers only).

| Field | Meaning |
| --- | --- |
| `name` | unique name, returned to the client |
| `enabled` | bool; disabled rules never apply |
| `match` | json list of `{"collection": "orders", "method": "POST"}`. ALL entries must be present in the batch for the rule to apply. `method` empty = any. A rule with an empty or missing `match` never applies. Collection names or ids are accepted. A `PUT` upsert counts as `POST` (new id) or `PATCH` (existing id); the batch is replayed in order, so `DELETE X` followed by `PUT {id: X}` is a `POST` |
| `assert` | expression evaluated BEFORE the sub-requests |
| `assert_post` | expression evaluated AFTER the last sub-request, on the stored records |
| `message` | error message returned to the client |
| `created`, `updated` | autodate |

At least one of `assert`/`assert_post` is required. Every save (CLI, API, any path) parses both expressions and refuses an invalid one (`validation_batch_rule_definition`).

## Phases

All of this runs in the `OnBatchRequest` hook (priority -10000, outermost) and inside ONE transaction:

1. The hook opens `RunInTransaction` and replaces `e.App` with the transaction app for the rest of the chain. Upstream's own `RunInTransaction` then joins this transaction (nested calls reuse it), so a failure anywhere rolls back everything.
2. `assert` of every applicable rule runs against the submitted bodies. Database reads (`stock()`) go through the transaction app. Then `kernel.OnBatch` emits `batch.before`.
3. `e.Next()` runs the upstream batch. The response it writes is buffered, so nothing reaches the client before the transaction commits.
4. If a rule has `assert_post` (or `kernel.OnBatch` has handlers), the records written are re-read inside the transaction (ids come from the batch response; a `fields=` query of a sub-request gets `,id` appended so it cannot hide the id (the `id` is removed from the response again when the client did not ask for it), and a written record whose id cannot be determined rejects the batch) and `assert_post` runs against the STORED values, so server side hooks, defaults and `computed` rollups done by the sub-requests are visible. Then `batch.after` is emitted with PUBLIC data: hidden fields and credential fields (`tokenKey`, `passwordHash`) are removed from the event bodies (the `assert_post` expressions still see the stored values). The hook count is read once per request; a batch hook that gets bound mid-request (hot reload) fails that request closed with `503` instead of silently skipping `batch.after`.
5. On success the transaction commits and the buffered response is sent. On failure the transaction rolls back and the client gets the 400 below.

Record hooks that upstream defers to "after commit" (`OnRecordAfter*Success`, realtime, webhooks) still fire after the outer commit and never for a rejected batch.

## Rejection

```json
{"status":400,"message":"Batch rejected.","data":{"batch":{"code":"validation_batch_rule","message":"<rule message>","rule":"<name>"}}}
```

An expression that cannot be evaluated (type error, missing request index, division by zero, 100 ms timeout) FAILS CLOSED with the same code and a message that names the rule and the reason. A `_batch_rules` table that cannot be read (any error other than the collection not existing yet) refuses the batch with a 500 and logs it.

### `assert` sees the submitted body, `assert_post` the stored values

`assert` evaluates the bodies as sent. PocketBase applies body keys such as `qty+`, `qty-`, `+tags`, `slug:autogenerate`, so a stored value can differ from the raw one. A body that uses such a modifier key for a field that an `assert` expression names (as a string or in `req(i).body.<field>`) is therefore REJECTED with a clear message. Money, stock and quota rules should use `assert_post`, which reads the stored values. Hooks that run after `assert` (JS `OnBatchRequest` at priority above -10000, `batch.before` handlers) can still rewrite `e.Batch`; they are trusted code and are not re-checked.

## Expression language

Small and safe: no loops, no assignment, no user code, no access to anything but the batch, `@request.auth` and the `stock()` lookup. Max 2048 bytes per expression, nesting depth 32, evaluation deadline 100 ms (checked at every node).

```
expr    = or
or      = and { "||" and }
and     = cmp { "&&" cmp }
cmp     = add [ ("==" | "!=" | "<" | "<=" | ">" | ">=") add ]
add     = mul { ("+" | "-") mul }
mul     = unary { ("*" | "/") unary }
unary   = ("!" | "-") unary | primary
primary = number | string | true | false | null | bareword | "(" expr ")"
        | "@request.auth." name
        | name "(" [ expr { "," expr } ] ")" [ "." name { "." name } ]   (the path only after req())
```

Numbers compare numerically. A numeric string is parsed when the other side is a number. Two strings compare numerically only in `all()`/`exists()` on a number-typed field, and only when both parse; all other string pairs compare lexically (`'10' < '5'` is true). NaN and infinity never occur: an overflowing calculation is an error.

Strings use `'...'` or `"..."`. A bareword that is not a keyword or a call is a string, so `sum(order_items, qty)` equals `sum('order_items', 'qty')`. `&&` and `||` short-circuit and need booleans. The whole expression must be boolean.

Comparison: numbers and numeric strings compare numerically (multipart bodies arrive as strings), other strings lexically. `null` supports only `==`/`!=`; an ordering with a missing value is `false`, so a missing field never satisfies a bound.

| Function | Result |
| --- | --- |
| `count(coll [, method])` | number of requests for `coll` |
| `sum(coll, field [, method])` | sum of `field` over requests for `coll` (missing/null skipped, non numeric = error). DELETE requests are skipped |
| `all(coll, field, op, value)` | `op` is a quoted operator (`'>='`); true when every non-DELETE request of `coll` satisfies it (true when there is none) |
| `exists(coll, field, value)` | true when some non-DELETE request of `coll` has `field == value` |
| `req(i).body.<field>` | field of request `i` (0 based). Also `req(i).method`, `.collection`, `.id` |
| `stock(items, id_field, qty_field, stock_coll, stock_field)` | smallest slack over the ids named in the batch: CURRENT `stock_field` of the record `stock_coll/<id>` (read in the transaction) minus the total `qty_field` requested for that id. `stock(...) >= 0` means every item can be served. 0 when the batch has no such items |
| `@request.auth.id` | id of the authenticated record, `''` when anonymous. Also `.collectionName` and any field of the auth record |

In `assert`, request data is the submitted body. In `assert_post`, it is the stored record (all fields, defaults applied; deleted records are skipped). `stock()` reads the live value in both phases: in `assert_post` it already includes the batch's own writes.

## Examples

Checkout (header total must match the lines, stock must cover them):

```sh
toki batch rules add checkout --match orders:POST --match order_items:POST \
    --assert 'sum(order_items, qty) == req(0).body.total_qty && stock(order_items, product, qty, products, stock) >= 0' \
    --message 'Order total or stock is wrong'
```

Transfer between wallets, balance never negative (the batch PATCHes both wallets):

```sh
toki batch rules add wallet-non-negative --match wallets:PATCH \
    --assert-post "all(wallets, balance, '>=', 0)" --message 'Insufficient balance'
```

Dry run against a batch body, nothing is executed (`assert` only):

```sh
toki batch rules test --file batch.json
```

## Go and WASM API

`kernel.OnBatchFor(app)` (`kernel/batch.go`) returns the `hook.Hook[*kernel.BatchEvent]` of ONE app instance (pass the same app that was given to `batchguard.Register`); handlers never see batches of another embedded instance. Subscribers do not import `batchguard`.

```go
kernel.OnBatchFor(app).BindFunc(func(e *kernel.BatchEvent) error {
    // e.Name: kernel.BatchBefore ("batch.before") or kernel.BatchAfter ("batch.after")
    // e.Requests: []kernel.BatchRequest{Index, Collection, Method, ID, Body, Deleted}
    // e.Auth *kernel.Record (nil when anonymous), e.App = the transaction app
    return e.Next() // returning an error rejects the batch and rolls back
})
```

`batch.before` runs inside the transaction before any sub-request (Body = submitted body); `batch.after` runs inside it after the last one (Body = stored values). A non-`ApiError` error becomes a 400 `validation_batch_rule`. `modules/wasm` can map these two names to guest events later; it is not wired yet.

## CLI

`toki batch rules list [--json] | add <name> --match coll[:METHOD]... [--assert E] [--assert-post E] [--message M] [--disabled] | rm <name> | test --file batch.json [--rule name]`

## Limits

- Rules that match by collection NAME stop applying after a rename; use ids to survive renames.
- Only requests that target `/api/collections/{c}/records[/{id}]` have a collection; other URLs never match a rule.
- Rules are read from the database on every batch (one small query), so changes apply at once.
- `assert_post` re-reads one record per sub-request; very large batches pay that cost only when a rule uses `assert_post` or a `kernel.OnBatchFor` handler exists.
- The batch timeout of upstream (`settings.batch.timeout`) still applies to the sub-requests; rule evaluation has its own 100 ms per expression.
