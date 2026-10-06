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
