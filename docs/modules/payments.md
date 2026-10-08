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
| `_payment_intents` | provider, `status`, `amount`, `currency`, `idempotency_key`, `order_ref`, `customer` + `customer_collection` (auth record ref), `product`, `checkout_url`, `provider_ref`, `metadata`, `provider_data` (capture ids, never credentials), `grant`, `refunded_amount`, `paid_at`, `last_error` |
| `_payment_events` | one row per webhook event: `raw` body, redacted `headers`, `verified`, `verify_error`, normalized `event`, `status` (`received`, `processed`, `ignored`, `rejected`, `failed`, `invalid`), `attempts`. Unique on `(provider, event_id)` |
| `_payment_products` | optional catalog: `slug`, price, `entitlement_key`, `duration_days`, `grace_days`, `quota`, `balance_add`, `subscription` |
| `_refunds` | refund attempts: `intent`, `amount`, `status` (`pending`, `succeeded`, `failed`), `source` (`api`, `provider`), `provider_ref`, `idempotency_key` |
| `_subscriptions` | per subject and product, kept in step with the entitlement of products flagged `subscription` |
| `_entitlements` | `subject` + `subject_collection`, `key`, `status` (`active`, `trial`, `grace`, `lapsed`), `until`, `quota`, `balance`, `source_intent`, `source_subscription`. Unique on `(subject, subject_collection, key)` |

## Status state machine

```
created -> pending -> paid -> refunded
                   |      \-> partially_refunded -> partially_refunded | refunded
                   \-> failed | expired
created -> paid | failed | expired   (webhook beating the save of the provider reference)
```

Illegal moves (for example `expired -> paid`, `failed -> paid`, `paid -> failed`) return `ErrIllegalTransition`; a webhook event that would cause one is stored as `rejected` and audited as `payments.rejected` (it is not retried). Moving to the status an intent already has is an idempotent no-op, which makes job retries, duplicate deliveries and replays safe. A payment that arrives for an expired intent therefore needs a manual decision (look at `toki payments intents show <id>`); automating it is deferred.

Every status change is written to the audit sink (`payments.status` with `from`, `to`, `source`); entitlement changes (`payments.entitlement`), refunds (`payments.refund`), rejected events, amount mismatches (`payments.amount_mismatch`) and invalid webhooks (`payments.webhook_invalid`) are audited as well.

## Flows

**Create.** `POST /api/payments/intents` (auth required) -> validate (positive integer amount, 3 letter currency, configured provider) -> idempotency lookup on `(customer, customer_collection, idempotency_key)` (same key and parameters: the same intent, HTTP 200 and `idempotent_replay: true`; different parameters: 409; the `Idempotency-Key` header is accepted too) -> insert `created` -> `Provider.CreateIntent` -> `pending` with `checkout_url`. A provider error moves the intent to `failed` and answers 502. With `product` the price and the grant come from `_payment_products`; a client cannot undercut the price and cannot send a grant. Clients cannot set `metadata.return_url`/`cancel_url`.

**Webhook.** `POST /api/payments/webhook/{provider}`: IP allowlist (optional) -> own per-IP rate limit (`TOKI_PAYMENTS_WEBHOOK_RPM`, default 300 per minute) and the `payments:webhook` rate limit tag -> body limit 1 MiB -> `Provider.VerifyWebhook` -> **store** the raw body with the verification result in `_payment_events` (a failed verification is stored as `invalid`, 16 KiB at most, pruned after 7 days, and answers 401) -> duplicate event ids are ignored -> enqueue `payments.process` (unique per event, 8 attempts) -> 200. Processing reloads the stored event, finds the intent (our id echoed by the provider, else any provider reference), checks amount and currency, applies the transition and marks the event `processed`. An event whose intent is not known yet is retried for 15 minutes, then `rejected`.

**Reconcile.** The cron `*/15 * * * *` enqueues `payments.reconcile` once per slot. It asks the provider (`FetchStatus`) about `pending` intents older than `TOKI_PAYMENTS_RECONCILE_MINUTES` (10), applies the result like a webhook, fails `created` intents that never got a provider reference, expires pending intents the provider never resolves after `TOKI_PAYMENTS_INTENT_TTL_HOURS` (168), sweeps entitlements and prunes old invalid webhook rows. `toki payments reconcile` runs the same pass by hand.

**Refund.** `POST /api/payments/intents/{id}/refund` (superuser, body `amount` (0 = all that is left), `reason`, `idempotency_key`). The refund row is created `pending`, the provider is asked, a provider that completes immediately moves the intent to `partially_refunded` or `refunded`, otherwise the webhook completes it. The webhook echo of our own refund is recognized by `provider_ref` and not counted twice; a refund made in the provider dashboard creates a `source=provider` row. A full refund lapses the entitlements granted by the intent and takes back the granted balance. Mayar has no public refund API (`ErrUnsupported`).

## Entitlements

`payment.paid` (the transition to `paid`) grants, inside the same transaction as the status change, the `grant` of the intent: `key`, `days` (0 = no end), `grace_days`, `quota` (set), `balance` (added). Renewals extend from the later of now and the current end; an expired row restarts from now. Granting is only ever done by that transition, so replays cannot double grant. Statuses:

| Status | Meaning |
| --- | --- |
| `active` | paid period running, `until` is its end (empty = no end) |
| `trial` | granted with `toki payments entitlements grant --status trial` or `GrantEntitlement(..., "trial")`; one trial per subject and key |
| `grace` | after `until` of an `active` row with `grace_days`; `until` now is the end of the grace |
| `lapsed` | no access |

`Sweep` (part of the reconcile pass) moves `active` -> `grace` -> `lapsed` and `trial` -> `lapsed`. Access never depends on the sweep: the rule function and `Module.Entitled` also compare `until` with the current time.

**Rules.** `entitled("pro") = true` (also `@entitled("pro") = true`) is true for the authenticated record holding the entitlement in status `active`, `trial` or `grace` with `until` empty or in the future. Example list rule of a `lessons` collection: `entitled("pro") = true`. Guests never match. It is a custom `search.TokenFunctions` entry registered by this package's `init()` (additive, independent of other modules), emitted identically by the legacy compiler and the AST path, SQLite only (see `docs/RULE_ENGINE.md`). It must be used as a comparison, not as a bare boolean, because the filter grammar needs `left op right`.

**Go event.** `payments.OnPaid(app)` returns a `*hook.Hook[*PaidEvent]` (`Intent` record) triggered after the commit of every transition to `paid`. `modules/webhooks` captures record events of user collections only and has no generic custom-event path, so outbound webhooks and WASM consumers are not wired to it yet (deferred); Go code binds to the hook, others can watch `_payment_intents` through the audit log.

## API

| Route | Auth | Notes |
| --- | --- | --- |
| `POST /api/payments/webhook/{provider}` | none (signature) | rate-limit tag `payments:webhook` |
| `POST /api/payments/intents` | any auth record | body `amount`, `currency`, `provider`, `order_ref`, `description`, `product`, `idempotency_key`, `metadata`; returns `{intent, checkout_url, idempotent_replay}`; tag `payments:intent` |
| `GET /api/payments/intents/{id}` | owner or superuser (others get 404) | no `provider_data`, `grant` or error text |
| `POST /api/payments/intents/{id}/refund` | superuser | tag `payments:refund` |
| `GET /api/payments/entitlements/me` | any auth record | the caller's entitlements and whether each is currently valid |

The rate limit tags are matched by the standard rate limit settings (`Settings > Rate limit`, label `payments:webhook`, ...) through `apis.CheckRateLimitTags`; the usual `METHOD /path` labels work as well.

## CLI

```
toki payments intents list [--status s] [--limit n] [--json]
toki payments intents show <id>                 # intent + events + refunds as JSON
toki payments reconcile                         # one reconcile pass, prints the report
toki payments entitlements grant <subject> <key> [--collection users] [--status active|trial] [--days n] [--grace-days n] [--quota n] [--balance n]
toki payments entitlements revoke <subject> <key> [--collection users]
toki payments entitlements list [--subject id] [--key key]
toki payments webhook replay <eventId>          # process a stored, verified event again (idempotent)
```

## Environment

| Variable | Meaning |
| --- | --- |
| `TOKI_PAYMENTS=off` | disable the module |
| `TOKI_PAYMENTS_WEBHOOK_RPM` | per-IP webhook limit per minute (300) |
| `TOKI_PAYMENTS_WEBHOOK_IPS`, `TOKI_PAYMENTS_<PROVIDER>_WEBHOOK_IPS` | optional comma separated IP/CIDR allowlist (provider list wins) |
| `TOKI_PAYMENTS_RECONCILE_MINUTES`, `TOKI_PAYMENTS_INTENT_TTL_HOURS` | reconcile age (10) and pending TTL (168) |
| `TOKI_PAYMENTS_ALLOW_PRIVATE=1` | lift the SSRF guard of the adapter HTTP clients (private gateways, tests) |
| `TOKI_PAYMENTS_MAYAR_API_KEY`, `_MODE` (`sandbox` default, `live`), `_BASE_URL`, `_WEBHOOK_TOKEN`, `_WEBHOOK_HEADER` (`x-callback-token`), `_REDIRECT_URL`, `_CONFIRM=1` | Mayar |
| `TOKI_PAYMENTS_PAYPAL_CLIENT_ID`, `_CLIENT_SECRET`, `_WEBHOOK_ID`, `_MODE` (`sandbox` default, `live`), `_BASE_URL`, `_RETURN_URL`, `_CANCEL_URL` | PayPal |

A per-tenant secret store is a later step.

## Adapters

**Mayar** (`https://api.mayar.id/hl/v1`, IDR only): `POST /payment/create` (single payment request), status through `GET /payment/{id}`. The public documentation does not describe webhook authentication, so the adapter requires a shared token: it compares (constant time) the header `x-callback-token` (configurable with `_WEBHOOK_HEADER`; `Authorization: Bearer` is accepted too) with `_WEBHOOK_TOKEN` and fails closed when the token is unset. The header name must be confirmed against the Mayar dashboard before going live. A token is weaker than an HMAC, so `TOKI_PAYMENTS_MAYAR_CONFIRM=1` makes the adapter re-read the payment from the API before it reports `paid`. `payment.received` is the only event used; its id is derived from the payload so redeliveries dedupe. The sandbox host (`api.mayar.io`) comes from the docs and can be overridden with `_BASE_URL`. No refund API.

**PayPal** (Orders v2): create order (`intent=CAPTURE`, `user_action=PAY_NOW`, `PayPal-Request-Id` = intent id), the buyer approves, `CHECKOUT.ORDER.APPROVED` makes the framework call `Capture`, `PAYMENT.CAPTURE.COMPLETED` marks the intent paid; `PAYMENT.CAPTURE.DENIED` fails it, `PAYMENT.CAPTURE.REFUNDED` counts a refund. Webhooks are verified with `POST /v1/notifications/verify-webhook-signature` (the `cert_url` host must be `*.paypal.com`; verification that cannot run is a server error, not a verdict). Refunds use `POST /v2/payments/captures/{id}/refund` with `PayPal-Request-Id` = refund id. Chargebacks (`PAYMENT.CAPTURE.REVERSED`) are ignored for now.

Both adapters use `internal/netguard` clients (timeouts, no proxy, no redirects, private addresses refused) and scrub their secrets from error text. Tests use recorded fixtures and `httptest`, never the network.

## Security notes

- **Credentials never in DB, export or log.** They are read from `TOKI_PAYMENTS_<PROVIDER>_*` by the adapter at start. No collection has a secret field; `_payment_events` stores headers with every name containing `auth`, `token`, `secret`, `key`, `cookie`, `sig` or `password` removed; adapter errors replace the key/token by `***`; `provider_data` only holds provider object ids.
- The webhook is verified before anything is marked verified or processed; forged requests are stored as `invalid` (truncated, pruned) and answered 401. Amount and currency of a `paid` event must match the intent.
- Clients cannot choose grants, prices of catalog products or checkout redirect URLs. `GET` of an intent hides other users' intents (404).
- `toki payments webhook replay` refuses unverified events.
- `no_payments` stub: the boot guard refuses to start on a data dir that has the collections or `TOKI_PAYMENTS_MAYAR_API_KEY`/`TOKI_PAYMENTS_PAYPAL_CLIENT_ID` set, unless `TOKI_ALLOW_STUBBED_MODULES=1`.

## Deferred

WASM adapters; per-tenant secret store; subscription lifecycle events from providers (renewals come from new paid intents today); chargebacks/disputes; automatic handling of a payment on an already expired intent; outbound webhook/WASM event bridge for `payment.paid`; PostgreSQL support of `entitled()` (custom functions are SQLite only); Admin UI screens.
