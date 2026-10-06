# Phase 0: kernel split inventory

Produced while splitting `core/` into `kernel/` (see `docs/decisions/0001-fork-and-kernel-split.md`).
Rule: everything that does not import `net/http` or `tools/router` (directly, or through a package that does) is kernel. `go list -deps ./kernel | grep -E '^net/http$|tools/router'` must print nothing (enforced by `kernel/deps_test.go`).

## Files in `core/` (non-test)

| File | Destination | Reason |
| --- | --- | --- |
| `app.go` | split | `kernel.App` keeps every non-HTTP member; `core.App` = `kernel.App` + `OnServe` + all `*Request` hooks (event types embed `RequestEvent`). |
| `auth_origin_model.go` | kernel | System models/queries, settings, migrations runner, logs, backups, cron-related. |
| `auth_origin_query.go` | kernel | System models/queries, settings, migrations runner, logs, backups, cron-related. |
| `backup.go` | kernel | System models/queries, settings, migrations runner, logs, backups, cron-related. |
| `backup_create.go` | kernel | Uses `tools/filesystem` and `tools/archive`. |
| `backup_restore.go` | kernel | Uses `tools/filesystem` and `tools/archive`. |
| `base.go` | split | `kernel.BaseApp` keeps DB, settings, cron, kernel hooks, filesystem/mail factories; `core.BaseApp` embeds `*kernel.BaseApp` and owns the request hooks, S3/SMTP wiring and the `OnServe` cron starter. |
| `collection_import.go` | kernel | Collection model, validation, import and table sync. |
| `collection_model.go` | kernel | Collection model, validation, import and table sync. |
| `collection_model_auth_options.go` | split | Kernel keeps `OAuth2ProviderConfig` and validation; `InitProvider()` returns `auth.Provider` (`tools/auth` imports `net/http`) so it became `core.InitOAuth2Provider`. Provider names come from a registry seam (`kernel.OAuth2Providers`) filled by core. |
| `collection_model_auth_templates.go` | kernel | Collection model, validation, import and table sync. |
| `collection_model_base_options.go` | kernel | Collection model, validation, import and table sync. |
| `collection_model_view_options.go` | kernel | Collection model, validation, import and table sync. |
| `collection_query.go` | kernel | Collection model, validation, import and table sync. |
| `collection_record_table_sync.go` | kernel | Collection model, validation, import and table sync. |
| `collection_validate.go` | kernel | Collection model, validation, import and table sync. |
| `db.go` | kernel | Database connect/builder/retry/tx/table helpers. |
| `db_builder.go` | kernel | Database connect/builder/retry/tx/table helpers. |
| `db_connect.go` | kernel | Database connect/builder/retry/tx/table helpers. |
| `db_connect_nodefaultdriver.go` | kernel | Database connect/builder/retry/tx/table helpers. |
| `db_model.go` | kernel | Database connect/builder/retry/tx/table helpers. |
| `db_retry.go` | kernel | Database connect/builder/retry/tx/table helpers. |
| `db_table.go` | kernel | Database connect/builder/retry/tx/table helpers. |
| `db_tx.go` | kernel | Database connect/builder/retry/tx/table helpers. |
| `event_request.go` | core | `RequestEvent` embeds `router.Event` (`net/http`, `tools/router`). `RequestInfo` + `RequestInfoContext*` constants are split into `kernel/request_info.go` (pure data used by the rule engine). |
| `event_request_batch.go` | core | `BatchRequestEvent`/`InternalRequest`: imports `net/http` (method names), embeds `RequestEvent`. |
| `events.go` | split | Kernel keeps Bootstrap/Terminate/Backup/Model/Record/Collection/Mailer/Filesystem/SettingsReload events; core keeps `ServeEvent` (Router, `http.Server`, autocert) and every event embedding `*RequestEvent`. |
| `external_auth_model.go` | kernel | Provider names validated through the `kernel.OAuth2Providers` seam instead of `auth.Providers`. |
| `external_auth_query.go` | kernel | System models/queries, settings, migrations runner, logs, backups, cron-related. |
| `field.go` | kernel | Field types and field list. |
| `field_autodate.go` | kernel | Field types and field list. |
| `field_bool.go` | kernel | Field types and field list. |
| `field_date.go` | kernel | Field types and field list. |
| `field_editor.go` | kernel | Field types and field list. |
| `field_email.go` | kernel | Field types and field list. |
| `field_file.go` | kernel | Uses `tools/filesystem` (now `net/http`-free). |
| `field_geo_point.go` | kernel | Field types and field list. |
| `field_json.go` | kernel | Field types and field list. |
| `field_number.go` | kernel | Field types and field list. |
| `field_password.go` | kernel | Field types and field list. |
| `field_relation.go` | kernel | Field types and field list. |
| `field_select.go` | kernel | Field types and field list. |
| `field_text.go` | kernel | Field types and field list. |
| `field_url.go` | kernel | Field types and field list. |
| `fields_list.go` | kernel | Field types and field list. |
| `log_model.go` | kernel | System models/queries, settings, migrations runner, logs, backups, cron-related. |
| `log_printer.go` | kernel | System models/queries, settings, migrations runner, logs, backups, cron-related. |
| `log_query.go` | kernel | System models/queries, settings, migrations runner, logs, backups, cron-related. |
| `mfa_model.go` | kernel | System models/queries, settings, migrations runner, logs, backups, cron-related. |
| `mfa_query.go` | kernel | System models/queries, settings, migrations runner, logs, backups, cron-related. |
| `migrations_list.go` | kernel | System models/queries, settings, migrations runner, logs, backups, cron-related. |
| `migrations_runner.go` | kernel | System models/queries, settings, migrations runner, logs, backups, cron-related. |
| `notify_watcher.go` | kernel | System models/queries, settings, migrations runner, logs, backups, cron-related. |
| `otp_model.go` | kernel | System models/queries, settings, migrations runner, logs, backups, cron-related. |
| `otp_query.go` | kernel | System models/queries, settings, migrations runner, logs, backups, cron-related. |
| `record_field_resolver.go` | kernel | Record model, tokens, queries and rule field resolver. |
| `record_field_resolver_replace_expr.go` | kernel | Record model, tokens, queries and rule field resolver. |
| `record_field_resolver_runner.go` | kernel | Record model, tokens, queries and rule field resolver. |
| `record_model.go` | kernel | Record model, tokens, queries and rule field resolver. |
| `record_model_auth.go` | kernel | Record model, tokens, queries and rule field resolver. |
| `record_model_superusers.go` | split | Kernel keeps the superusers hooks/constants; the delete guard returns `router.NewBadRequestError` (an `ApiError`), so that single hook is registered by core. |
| `record_proxy.go` | kernel | Record model, tokens, queries and rule field resolver. |
| `record_query.go` | kernel | Record model, tokens, queries and rule field resolver. |
| `record_query_expand.go` | kernel | Record model, tokens, queries and rule field resolver. |
| `record_tokens.go` | kernel | Record model, tokens, queries and rule field resolver. |
| `settings_model.go` | kernel | System models/queries, settings, migrations runner, logs, backups, cron-related. |
| `settings_query.go` | kernel | System models/queries, settings, migrations runner, logs, backups, cron-related. |
| `syscall.go` | kernel | OS process helpers (`Restart`). |
| `syscall_wasm.go` | kernel | OS process helpers (wasm). |
| `system_alert.go` | kernel | System models/queries, settings, migrations runner, logs, backups, cron-related. |
| `view.go` | kernel | System models/queries, settings, migrations runner, logs, backups, cron-related. |
| `validators/` (package) | kernel | Moved to `kernel/validators`; pure validation helpers. |

## Result

`kernel/` holds everything from the table above marked `kernel` (plus the kernel halves of the split files). `core/` keeps only:

| File | What stays in core |
| --- | --- |
| `app.go` | `core.App` = `kernel.App` + `OnServe` + all `*Request` hooks |
| `base.go` | `core.BaseApp` (embeds `*kernel.BaseApp`), `NewBaseApp` wiring, `AsApp`, mail client wiring |
| `base_hooks.go` | `requestHooks` (storage and accessors of the request hooks) |
| `events.go` | `ServeEvent`, `UIExtension` and every event embedding `*RequestEvent` |
| `event_request.go` | `RequestEvent` (+ `RealIP`, `RequestInfo()` builder) |
| `event_request_batch.go` | `BatchRequestEvent`, `InternalRequest` |
| `record_model_superusers.go` | superusers delete guard (returns an `ApiError`) |
| `oauth2_provider.go` | `InitOAuth2Provider`, OAuth2 provider registry for the kernel |
| `kernel_aliases.go` | aliases of every moved exported identifier |
| tests | `base_test.go`, `event_request*_test.go`, `record_model_superusers_test.go`, `oauth2_provider_test.go`, `kernel_outer_test.go` |

Nothing was left in core because of an import cycle. Everything that stayed does so because it is HTTP bound.

## Seams added (needed to keep `net/http` out of the kernel)

`net/http` was not only imported by `core` itself: `tools/filesystem` (HTTP serving, S3 client), `tools/mailer` (mailyak), `tools/auth` (OAuth2 providers) and `tools/router` all pull it in. The kernel needs the first two as libraries and the third only for provider names, so:

| Package | Change |
| --- | --- |
| `tools/filesystem/blob` | stdlib content sniffer vendored (`sniff.go`) instead of `http.DetectContentType` |
| `tools/filesystem` | `Serve`, `NewFileFromURL`, `NewS3` moved to `tools/filesystem/fshttp` as functions (`fshttp.Serve(fsys, res, req, key, name)`); `NewFromDriver` added. JS `$filesystem.*` bindings unchanged |
| `tools/mailer` | `SMTPClient` and `Sendmail` moved to `tools/mailer/clients`; `SMTPAuth*` constants stay in `mailer`; `AddressesToStrings`, `HTML2Text`, `DetectReaderMimeType` exported for the clients |
| kernel config | `BaseAppConfig.MailClientFactory` and `BaseAppConfig.S3FilesystemFactory` (set by `core.NewBaseApp`) |
| kernel registry | `kernel.OAuth2Providers` (names only), filled by `core` on init from `tools/auth` |
| outer app | `kernel.BaseApp.SetOuter(outer, wrap)`: hook handlers, transaction callbacks and `UnsafeWithoutHooks()` receive the outer app (`*core.BaseApp`), so `core.AsApp(e.App)` always succeeds in a server |

## Go API changes for callers (behavior unchanged)

- Transaction callbacks and the `App` field of kernel events (`ModelEvent`, `RecordEvent`, `CollectionEvent`, `BootstrapEvent`, `TerminateEvent`, `BackupEvent`, `MailerEvent`, ...) are typed `kernel.App`. Use `core.AsApp(x)` when the server hooks are needed. `core.RequestEvent.App` stays `core.App`.
- `core.AppMigrations` / `core.SystemMigrations` are now pointers to the kernel lists. `migrations.Register` keeps accepting `func(app core.App) error`, so generated and user migration files compile unchanged.
- `OAuth2ProviderConfig.InitProvider()` is now `core.InitOAuth2Provider(config)`.
- `filesystem.System.Serve`, `filesystem.NewFileFromURL`, `filesystem.NewS3`, `mailer.SMTPClient`, `mailer.Sendmail` moved as described above.
- `kernel/validators` replaces `core/validators`.

## Verification

- `go list -deps ./kernel | grep -E '^net/http$|tools/router'` prints nothing; `kernel/deps_test.go` enforces it (and forbids a direct `os/exec` import). `os/exec` is still reachable transitively through `tools/osutils` and the SQLite driver; the kernel itself does not spawn processes.
- `golangci.yml` has a `depguard` rule for `kernel/**` (net/http, tools/router, os/exec, core, apis).

## Known leftovers

- `plugins/jsvm/internal/types/generated/types.d.ts` was not regenerated (the generator output already differed from the committed file before this change). `kernel` was added to the generator package list for the next regeneration.
- `TestNotifyWatcher_CollectionsUpdate` and `TestNotifyWatcher_SettingsUpdate` fail on `main` as well (file watcher timing); they are unrelated.
