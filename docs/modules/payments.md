# Module `payments`

Provider-neutral payments plumbing. Package `modules/payments`. TokiBase does not process money: the payment provider does. The module provides what every integration repeats: signed webhooks, idempotency, a status state machine, reconciliation and entitlements.

- Enabled by default: `tokibase.go` calls `payments.Register(app)` and adds the `payments` command. `TOKI_PAYMENTS=off` disables it at runtime; `-tags no_payments` compiles it out (edge and nano profiles).
- Needs the job queue (`modules/jobs`, through `kernel.Jobs(app)`) for retries and the reconcile cron. Without a queue (`TOKI_JOBS=off`) webhook events are processed inline and the reconcile tick runs inline.
- Out of scope: tax, PDF invoices, accounting, a double-entry ledger.

## Deviation from the design: Go adapters now, WASM later

The design says provider adapters would be WASM plugins. They are Go packages behind the `payments.Provider` interface for now (`providers/mayar`, `providers/paypal`). The interface is kept WASM friendly: inputs and outputs are plain structs/JSON, headers are a lower-cased `map[string]string`, bodies are `[]byte`, there are no `http.Request`/`core.App` types, and credentials are read by the adapter from the environment. An adapter that becomes a WASM guest later implements the same five functions.

```go
type Provider interface {
    Name() string
    CreateIntent(ctx, *IntentSpec) (checkoutURL, providerRef string, err error)
    VerifyWebhook(ctx, headers map[string]string, body []byte) ([]WebhookEvent, error)
    Refund(ctx, *RefundSpec) (*RefundResult, error)
    FetchStatus(ctx, providerRef string, data map[string]any) (*Status, error)
}
type Capturer interface { Capture(ctx, providerRef string, data map[string]any) (*Status, error) } // optional (PayPal)
```

Adapters register a `Factory` in `init()` (`payments.RegisterFactory`); the factory returns `nil, nil` when its env is absent, so an unconfigured provider simply does not exist (webhook route answers 404). `tokibase_payments.go` blank-imports the two reference adapters. `providerRef` may be a comma separated list of ids (first is canonical); webhooks match an intent by any element.

Amounts are integers in the minor unit of the currency (`payments.MinorExponent`: IDR/JPY/KRW and other zero-decimal currencies have exponent 0, everything else 2).

## Collections

All are system collections with rules `null` (superusers only); clients use `/api/payments/*`.

| Collection | Purpose |
| --- | --- |
| `_payment_intents` | provider, `status` (adds `paid_late`), `amount`, `currency`, `idempotency_key`, `order_ref`, `customer` + `customer_collection` (auth record ref), `product`, `checkout_url`, `provider_ref`, `metadata`, `provider_data` (capture ids, never credentials), `grant`, `refunded_amount`, `paid_at`, `last_error` |
| `_payment_events` | one row per webhook event: `raw` body, redacted `headers`, `verified`, `verify_error`, normalized `event`, `status` (`received`, `processed`, `ignored`, `rejected`, `failed`, `pending_order`, `dead`, `invalid`), `attempts`. Unique on `(provider, event_id)`. A rejected delivery (`invalid`) holds only the SHA-256 and size of the body and the header names |
| `_payment_products` | optional catalog: `slug`, price (`amount` > 0 and `currency` are mandatory: a product without a fixed price cannot be bought), `trial_days` (reserved, not used yet), `entitlement_key`, `duration_days`, `grace_days`, `quota`, `balance_add`, `subscription` |
| `_refunds` | refund attempts: `intent`, `amount`, `status` (`pending`, `succeeded`, `failed`), `source` (`api`, `provider`), `provider_ref` (unique when set), `idempotency_key` |
| `_subscriptions` | per subject and product, kept in step with the entitlement of products flagged `subscription` |
| `_entitlement_grants` | ledger: one row per grant (`entitlement`, `intent` (empty = manual), `days`, `balance`, `ends`, `revoked`); a refund takes back exactly its intent's rows |
| `_entitlements` | `subject` + `subject_collection`, `key`, `status` (`active`, `trial`, `grace`, `lapsed`), `until` (end of the paid period; grace is computed from it), `quota`, `balance`, `source_intent`, `source_subscription`. Unique on `(subject, subject_collection, key)` |

## Status state machine

```
created -> pending -> paid -> refunded
                   |      \-> partially_refunded -> partially_refunded | refunded
                   \-> failed | expired
created -> paid | failed | expired   (webhook beating the save of the provider reference)
failed | expired -> paid_late -> paid | refunded | partially_refunded   (late money, see below)
```

Illegal moves (for example `paid -> failed`, `refunded -> paid`) return `ErrIllegalTransition`; a webhook event that would cause one is stored as `rejected` and audited as `payments.rejected` (it is not retried). Moving to the status an intent already has is an idempotent no-op, which makes job retries, duplicate deliveries and replays safe.

**Late payments (`failed`/`expired` intents).** A verified `paid` event (amount and currency match) for an intent that already gave up is never dropped:

- inside `TOKI_PAYMENTS_LATE_WINDOW_HOURS` (default 24) of the intent's last change, and when the event carries a provider reference the intent already holds, the intent is re-opened to `paid` (entitlement granted) and `OnPaid` fires with `Late = true`;
- otherwise (older, or the event names the intent only by the echoed intent id, which is what a timeout during checkout creation looks like) the intent becomes `paid_late`: nothing is granted, `payments.OnLatePayment` fires, the audit entry `payments.late_payment` (`level: error`) is written and the process logs at Error level.

Operator decision path for `paid_late`: `toki payments intents list --attention` (also lists the intents of `rejected` and `dead` webhook events) -> `toki payments intents show <id>` -> either **accept** with `toki payments intents resolve <id>` (becomes `paid`, entitlement granted, `OnPaid` with `Late = true`) or **send the money back** with `toki payments refund <id>` (also `POST /api/payments/intents/{id}/refund`; works on `paid_late`). Webhook events in trouble: `toki payments webhook list --status rejected|dead|failed|pending_order`, then `toki payments webhook replay <eventId>`.

Every status change is written to the audit sink (`payments.status` with `from`, `to`, `source`); entitlement changes (`payments.entitlement`), refunds (`payments.refund`), rejected events, amount mismatches (`payments.amount_mismatch`) and invalid webhooks (`payments.webhook_invalid`) are audited as well.

## Flows

**Create.** `POST /api/payments/intents` (auth required) -> validate (positive integer amount, 3 letter currency, configured provider) -> idempotency lookup on `(customer, customer_collection, idempotency_key)` (same key and parameters: the same intent, HTTP 200 and `idempotent_replay: true`; different parameters: 409; the `Idempotency-Key` header is accepted too) -> insert `created` -> `Provider.CreateIntent` -> `pending` with `checkout_url`. A provider error moves the intent to `failed` and answers 502. With `product` the price and the grant come from `_payment_products`; a client cannot undercut the price and cannot send a grant. Clients cannot set `metadata.return_url`/`cancel_url`.

**Webhook.** `POST /api/payments/webhook/{provider}`: own per-IP rate limit (`TOKI_PAYMENTS_WEBHOOK_RPM`, default 300 per minute) and the `payments:webhook` rate limit tag -> optional IP allowlist -> body limit **64 KiB** (413 above) -> `Provider.VerifyWebhook` -> **store** the raw body with the verification result in `_payment_events` -> duplicate event ids are ignored (a duplicate of an event that is still `received`/`failed` is dispatched again) -> enqueue `payments.process` (unique per event, 8 attempts) -> 200. An unverified delivery answers 401 and leaves at most a tiny `invalid` row (SHA-256 and size of the body, header names, no values), only for the first `TOKI_PAYMENTS_INVALID_STORE_PER_HOUR` (20) failures per source address and hour; the row and its audit entry are skipped beyond that. Verification that could not run (provider API down) is a **503** and stores nothing, so the provider retries. `invalid` rows are deleted after a day (one SQL statement) and capped at 1000. Behind a reverse proxy configure PocketBase's trusted proxy headers, otherwise every client shares the proxy's address and the per-IP limits below throttle everybody together.

Processing reloads the stored event, finds the intent (our id echoed by the provider, else any provider reference), requires amount **and** currency of a `paid` event to equal the intent's (a missing or zero amount never matches), applies the transition and marks the event `processed`. An event whose intent is not known yet is retried by the job queue for about 10 minutes (`orphanGrace`, the span of 8 attempts with the queue's backoff) and then `rejected` with a `payments.rejected` audit entry. Any other failure is retried by the queue and by the reconcile cron; after `TOKI_PAYMENTS_MAX_EVENT_ATTEMPTS` (12) failures the event becomes `dead` (audit `payments.event_dead`, Error log) and waits for `webhook replay`. A `refunded` event that arrives before the payment is parked as `pending_order` and applied as soon as the intent is paid (by the `paid` event or the next reconcile pass); after `TOKI_PAYMENTS_INTENT_TTL_HOURS` it is rejected.

**Reconcile.** The cron `*/15 * * * *` enqueues `payments.reconcile` once per slot. It: asks the provider (`FetchStatus`) about `pending` intents older than `TOKI_PAYMENTS_RECONCILE_MINUTES` (10) and applies the result like a webhook (a `paid` status must carry the intent's exact amount and currency, otherwise nothing changes and the pass counts an error); fails `created` intents that never got a provider reference; expires pending intents the provider never resolves after `TOKI_PAYMENTS_INTENT_TTL_HOURS` (168); re-checks the 50 most recently changed `paid`/`partially_refunded` intents (14 days) for refunds made at the provider; processes stored events that did not finish (`received` and `failed` older than 2 minutes, parked `pending_order`); marks API refunds that were not confirmed within 24 hours as `failed` (Error log, audit); sweeps entitlements; prunes old rejected-webhook rows. `toki payments reconcile` runs the same pass by hand.

**Refund.** `POST /api/payments/intents/{id}/refund` (superuser, body `amount` (0 = all that is left), `reason`, `idempotency_key`; or `toki payments refund`). The remaining amount is checked and the `pending` refund row inserted in one transaction, so concurrent refunds cannot exceed the payment. The provider is asked; a provider that completes immediately moves the intent to `partially_refunded` or `refunded`, otherwise the webhook completes it. Each provider refund id is counted once: the webhook echo is matched to our row by the provider's refund id (an echo that arrives while the API call has not stored that id yet waits and is retried; an echo counted first makes the API answer a no-op), and a refund made in the provider dashboard creates a `source=provider` row keyed by the provider id (unique index). A full refund takes back exactly the contribution of that intent from the entitlement (see below) and its balance. Mayar has no public refund API (`ErrUnsupported`).

## Entitlements

`payment.paid` (the transition to `paid`) grants, inside the same transaction as the status change, the `grant` of the intent: `key`, `days` (0 = no end), `grace_days`, `quota` (set), `balance` (added). Renewals extend from the later of now and the current paid end; an expired or grace row restarts from now. Granting is only ever done by that transition, so replays cannot double grant. Every grant is also written to `_entitlement_grants`, so refunding intent A takes only A's days off `until` and A's balance off `balance`, and the entitlement lapses only when no un-revoked grant is left (entitlements written before the ledger existed lapse whole, by `source_intent`). Statuses:

| Status | Meaning |
| --- | --- |
| `active` | paid period running, `until` is its end (empty = no end) |
| `trial` | granted with `toki payments entitlements grant --status trial` or `GrantEntitlement(..., "trial")`; one trial per subject and key; no grace |
| `grace` | after `until` of an `active` row with `grace_days`; `until` is still the end of the paid period, the grace ends at `until + grace_seconds` |
| `lapsed` | no access |

Whether a subject holds an entitlement is a pure function of the stored row and the clock: status `active`/`trial`/`grace` and `until` empty or in the future, or (`active`/`grace` with `grace_seconds`) `until + grace` in the future. `Module.Entitled` and the `entitled()` rule function use that same predicate, so access neither flickers between the end of the paid period and the sweep nor depends on when the sweep ran. `Sweep` (part of the reconcile pass) only persists the status: `active` -> `grace` -> `lapsed`, `trial` -> `lapsed`; a late sweep goes straight to `lapsed`.

**Rules.** `entitled("pro") = true` (also `@entitled("pro") = true`) is true for the authenticated record holding the entitlement (predicate above). Example list rule of a `lessons` collection: `entitled("pro") = true`. Guests never match. It is a custom `search.TokenFunctions` entry registered by this package's `init()` (additive, independent of other modules), emitted identically by the legacy compiler and the AST path, SQLite only (see `docs/RULE_ENGINE.md`). It must be used as a comparison, not as a bare boolean, because the filter grammar needs `left op right`.

**Go events.** `payments.OnPaid(app)` returns a `*hook.Hook[*PaidEvent]` (`Intent` record, `Late` bool) triggered after the commit of every transition to `paid`; `payments.OnLatePayment(app)` fires for a transition to `paid_late` (nothing granted yet). Both are **at-most-once**: a crash between the commit and the handler, or a handler error (only logged), loses the notification, so fulfilment must be idempotent and able to find paid intents again (`toki payments intents list --status paid`). A transactional outbox is deferred. `modules/webhooks` captures record events of user collections only and has no generic custom-event path, so outbound webhooks and WASM consumers are not wired to it yet (deferred); Go code binds to the hook, others can watch `_payment_intents` through the audit log.

## API

| Route | Auth | Notes |
| --- | --- | --- |
| `POST /api/payments/webhook/{provider}` | none (signature) | rate-limit tag `payments:webhook` |
| `POST /api/payments/intents` | any auth record | body `amount`, `currency`, `provider`, `order_ref`, `description`, `product`, `idempotency_key`, `metadata`; returns `{intent, checkout_url, idempotent_replay}`; tag `payments:intent`; 429 beyond `TOKI_PAYMENTS_INTENT_PER_MINUTE` (30) new intents per minute or `TOKI_PAYMENTS_MAX_OPEN_INTENTS` (20) created/pending ones per customer, even when no rate limit rule is configured |
| `GET /api/payments/intents/{id}` | owner or superuser (others get 404) | no `provider_data`, `grant` or error text |
| `POST /api/payments/intents/{id}/refund` | superuser | tag `payments:refund`; works on `paid`, `partially_refunded` and `paid_late` |
| `GET /api/payments/entitlements/me` | any auth record | the caller's entitlements and whether each is currently valid |

The rate limit tags are matched by the standard rate limit settings (`Settings > Rate limit`, label `payments:webhook`, ...) through `apis.CheckRateLimitTags`; the usual `METHOD /path` labels work as well.

## CLI

```
toki payments intents list [--status s] [--attention] [--limit n] [--json]
toki payments intents show <id>                 # intent + events + refunds as JSON
toki payments intents resolve <id>              # accept a paid_late payment (grants the entitlement)
toki payments refund <id> [--amount n] [--reason r] [--idempotency-key k]
toki payments reconcile                         # one reconcile pass, prints the report
toki payments entitlements grant <subject> <key> [--collection users] [--status active|trial] [--days n] [--grace-days n] [--quota n] [--balance n]
toki payments entitlements revoke <subject> <key> [--collection users]
toki payments entitlements list [--subject id] [--key key]
toki payments webhook list [--status s] [--limit n]   # rejected | dead | failed | pending_order need a look
toki payments webhook replay <eventId>          # process a stored, verified event again (idempotent)
```

## Environment

| Variable | Meaning |
| --- | --- |
| `TOKI_PAYMENTS=off` | disable the module |
| `TOKI_PAYMENTS_WEBHOOK_RPM` | per-IP webhook limit per minute (300) |
| `TOKI_PAYMENTS_WEBHOOK_IPS`, `TOKI_PAYMENTS_<PROVIDER>_WEBHOOK_IPS` | optional comma separated IP/CIDR allowlist (provider list wins) |
| `TOKI_PAYMENTS_RECONCILE_MINUTES`, `TOKI_PAYMENTS_INTENT_TTL_HOURS` | reconcile age (10) and pending TTL (168) |
| `TOKI_PAYMENTS_LATE_WINDOW_HOURS` | a late payment inside this window with a matching provider reference re-opens the intent to `paid`; otherwise `paid_late` (24) |
| `TOKI_PAYMENTS_MAX_EVENT_ATTEMPTS` | failures before a webhook event is dead-lettered (12) |
| `TOKI_PAYMENTS_INVALID_STORE_PER_HOUR` | rejected deliveries stored/audited per source address and hour (20) |
| `TOKI_PAYMENTS_INTENT_PER_MINUTE`, `TOKI_PAYMENTS_MAX_OPEN_INTENTS` | per-customer brakes on creating intents (30, 20) |
| `TOKI_PAYMENTS_ALLOW_PRIVATE=1` | lift the SSRF guard of the adapter HTTP clients (private gateways, tests) |
| `TOKI_PAYMENTS_MAYAR_API_KEY`, `_MODE` (`sandbox` default, `live`), `_BASE_URL`, `_WEBHOOK_TOKEN`, `_WEBHOOK_HEADER` (`x-callback-token`), `_REDIRECT_URL`, `_CONFIRM` | Mayar |
| `TOKI_PAYMENTS_PAYPAL_CLIENT_ID`, `_CLIENT_SECRET`, `_WEBHOOK_ID`, `_MODE` (`sandbox` default, `live`), `_BASE_URL`, `_RETURN_URL`, `_CANCEL_URL` | PayPal |

A per-tenant secret store is a later step.

## Adapters

**Mayar** (`https://api.mayar.id/hl/v1`, IDR only): `POST /payment/create` (single payment request), status through `GET /payment/{id}`. The public documentation does not describe webhook authentication, so the adapter requires a shared token: it compares (constant time) the header `x-callback-token` (configurable with `_WEBHOOK_HEADER`; `Authorization: Bearer` is accepted too) with `_WEBHOOK_TOKEN` and fails closed when the token is unset. The header name must be confirmed against the Mayar dashboard before going live. A token is weaker than an HMAC, so `TOKI_PAYMENTS_MAYAR_CONFIRM=1` makes the adapter re-read the payment from the API before it reports `paid` (default **on in live mode**, `0` turns it off; the amount then comes from the API). A `paid` webhook without a usable amount (absent, 0, decimal) is always confirmed through the API, and without an amount there either it is not paid. `payment.received` is the only event used; its id is derived from the payload so redeliveries dedupe. Only the statuses `paid`, `success`, `settled`, `completed` pay; `failed`/`cancelled` fail, `expired` expires, `unpaid`/`pending`/`created` are ignored, and any other value (missing, `null`, `refunded`, `closed`, ...) is an `unknown` event: stored as ignored, logged as a warning, the intent is left alone. `GET /payment/{id}` maps `closed` (a link closed in the dashboard) and unknown statuses to pending, never paid; this status vocabulary comes from the public docs and is not verified against the live API. The sandbox host (`api.mayar.io`) comes from the docs and can be overridden with `_BASE_URL`. No refund API.

**PayPal** (Orders v2): create order (`intent=CAPTURE`, `user_action=PAY_NOW`, `PayPal-Request-Id` = intent id), the buyer approves, `CHECKOUT.ORDER.APPROVED` makes the framework call `Capture`, `PAYMENT.CAPTURE.COMPLETED` marks the intent paid; `PAYMENT.CAPTURE.DENIED` fails it, `PAYMENT.CAPTURE.REFUNDED` counts a refund. Webhooks are verified with `POST /v1/notifications/verify-webhook-signature` (the `cert_url` host must be `*.paypal.com`; verification that cannot run is a `503`, not a verdict, see Webhook). Refunds use `POST /v2/payments/captures/{id}/refund` with `PayPal-Request-Id` = refund id. Chargebacks (`PAYMENT.CAPTURE.REVERSED`) are ignored for now.

Both adapters use `internal/netguard` clients (timeouts, no proxy, no redirects, private addresses refused) and scrub their secrets from error text. Tests use recorded fixtures and `httptest`, never the network.

## Security notes

- **Credentials never in DB, export or log.** They are read from `TOKI_PAYMENTS_<PROVIDER>_*` by the adapter at start. No collection has a secret field; `_payment_events` stores headers with every name containing `auth`, `token`, `secret`, `key`, `cookie`, `sig` or `password` removed; adapter errors replace the key/token by `***`; `provider_data` only holds provider object ids.
- The webhook is verified before anything is marked verified or processed; forged requests answer 401 and leave at most a hash-only trace, capped per source address. Amount and currency of a `paid` event (and of a `paid` provider status in reconcile/capture) must equal the intent's; missing means mismatch.
- Clients cannot choose grants, prices or currencies of catalog products (a product needs a fixed price) or checkout redirect URLs. For an intent created without `product` (server side code) `amount`, `metadata` and `order_ref` are whatever the client sent: consumers of `OnPaid` must not trust them for such intents. `GET` of an intent hides other users' intents (404).
- `toki payments webhook replay` refuses unverified events.
- `no_payments` stub: the boot guard refuses to start on a data dir that has the collections or `TOKI_PAYMENTS_MAYAR_API_KEY`/`TOKI_PAYMENTS_PAYPAL_CLIENT_ID` set, unless `TOKI_ALLOW_STUBBED_MODULES=1`.

## Deferred

WASM adapters; per-tenant secret store; subscription lifecycle events from providers (renewals come from new paid intents today); chargebacks/disputes (`PAYMENT.CAPTURE.REVERSED` is ignored, the entitlement stays); pay-what-you-want products; transactional outbox for `OnPaid`; an indexed provider reference table (lookup by reference is an exact indexed match first, then a scan); operator identity in CLI audit entries; outbound webhook/WASM event bridge for `payment.paid`; PostgreSQL support of `entitled()` (custom functions are SQLite only); Admin UI screens.
