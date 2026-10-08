# backupcheck

Verifies backups by restoring them into a temp dir and checking integrity, so "a backup exists but restore was never tested" cannot happen silently. Does not change the REST contract.

## What is checked

For a backup zip `name` (fetched from `app.NewBackupsFilesystem()`, local or S3):

1. The zip is extracted (`data.db`, `auxiliary.db`) into a temp dir; the storage files are only indexed from the zip, not extracted. The temp dir is always removed.
2. Both databases are opened read-only (modernc driver) and checked with `PRAGMA integrity_check` and `PRAGMA quick_check` (must return `ok`). A query error such as "file is not a database" counts as a failed check.
3. `_collections` rows and the total records of all non-view collections in the backup are counted and compared with the live app. The collections count must match when the backup is the latest one; records may differ and are reported both ways.
4. File fields of up to 200 sampled records are checked: every referenced file must exist under `storage/<collectionId>/<recordId>/` in the zip. Skipped when S3 file storage is enabled (files are not part of the zip).

Report fields: `name`, `sizeBytes`, `integrityOk`, `quickCheckOk`, `collections`, `records`, `liveCollections`, `liveRecords`, `missingFiles`, `sampledRecords`, `isLatest`, `duration`, `error`.

`duration` is in nanoseconds in the JSON output.

## What is not checked

- Restoring into a live process (`RestoreBackup`) is not simulated: the check opens the databases standalone.
- Missing files are reported but do not fail the check or the exit code; inspect `missingFiles`.
- S3 backups are downloaded in full into the temp dir.
- Only the first 200 records are sampled for files; files beyond that are not checked.

## Hook

`backupcheck.Register(app)` (called by `tokibase.New*`) binds `OnBackupCreate`: after a backup is created successfully, `Verify` runs in a goroutine. The result is logged at Info (ok) or Error (failed). `TOKI_BACKUP_VERIFY=off` disables the hook.

Audit: modules do not import each other, so the hook exposes `backupcheck.OnResult func(app kernel.App, r Report)`. Wire the audit module (action `backup.verify`) there from the root package once both modules are on main.

## API backups under load

`POST /api/backups` is synchronous (204 when the zip is stored, as upstream). The time is dominated by `VACUUM INTO` and zipping `auxiliary.db`, so a small `auxiliary.db` (`TOKI_LOGS_MAX_MB`, default 512) keeps it short; the automatic verification runs after the response and does not add to it. For long backups add `?async=true` (or the header `Prefer: respond-async`): the call answers `202` with `{"state":"running","name":...}` and `GET /api/backups/status` (superuser) returns `idle`, `running`, `done` or `failed` (with `error`) for the last asynchronous backup. See `docs/COMPAT.md`.

## CLI

```
toki backup create [name]
toki backup list [--json]
toki backup verify <name|latest> [--json]
toki backup verify-all [--json]
```

Exit codes: `0` ok; `1` when `integrityOk` or `quickCheckOk` is false, the verification errored, or (latest backup) the collections count differs from the live app. `verify-all` exits `1` if any backup fails and prints a summary table.

## CI / cron

```sh
toki backup verify latest --dir /var/lib/app/pb_data || notify-ops "backup verify failed"
```

Cron, nightly after the autobackup:

```
30 3 * * * /usr/local/bin/toki backup verify latest --json >> /var/log/backup-verify.log
```

## Go API

`backupcheck.Verify(ctx, app, name) (Report, error)`, `backupcheck.List(ctx, app)`, `backupcheck.Latest(ctx, app)`, `backupcheck.Register(app)`, `Report.OK()`.
