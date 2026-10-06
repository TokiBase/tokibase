# walreplica

Continuous replication of `data.db` and `auxiliary.db` to a local path or an S3 compatible bucket, with point-in-time restore. It embeds [Litestream](https://github.com/benbjohnson/litestream) (v0.5.x, pure Go, same `modernc.org/sqlite` driver as TokiBase) as a library. It replaces rsync based HA copies: rsync of a live SQLite file can capture a torn database, WAL replication cannot.

The REST contract, settings and collection JSON are unchanged. The module is inactive unless `TOKI_REPLICA_URL` is set.

## How it works

1. After a successful bootstrap the module starts one Litestream database per file: `<pb_data>/data.db` replicates to `<url>/data`, `<pb_data>/auxiliary.db` to `<url>/aux`.
2. Litestream holds a long running read transaction on each database (so SQLite cannot checkpoint past what was not yet shipped) and, every `TOKI_REPLICA_SYNC_INTERVAL`, copies the new WAL pages into an LTX file and uploads it. Writers are not blocked.
3. In the background LTX files are compacted (30 s, 5 min, 1 h levels), a full snapshot is written every `TOKI_REPLICA_SNAPSHOT_INTERVAL`, and snapshots older than `TOKI_REPLICA_RETENTION` are deleted. The retention is the point-in-time restore window.
4. On shutdown (`OnBootstrapClear`, which `OnTerminate`, `app.Restart()` and `ClearBootstrap` all go through) the module syncs one last time, lets the app close its own connections, then closes Litestream. Litestream must be closed after the app released its handles (Litestream library constraint), so the module wraps the clear hook.

Local state: Litestream keeps a metadata directory next to each database (`pb_data/.data.db-litestream`, `pb_data/.auxiliary.db-litestream`). These are machine specific and are excluded from TokiBase backups.

## Configuration

| Variable | Default | Meaning |
| --- | --- | --- |
| `TOKI_REPLICA_URL` | empty (inactive) | `file:///abs/path` or `s3://bucket/prefix` |
| `TOKI_REPLICA_SYNC_INTERVAL` | `1s` | upload interval, the RPO |
| `TOKI_REPLICA_RETENTION` | `24h` | how long snapshots (restore points) are kept |
| `TOKI_REPLICA_SNAPSHOT_INTERVAL` | `1h` | full snapshot interval |

Durations use Go syntax (`500ms`, `30s`, `24h`). An invalid value makes the app refuse to start.

S3 credentials and endpoint use the standard AWS variables (the default AWS credential chain, so shared credentials files and instance roles also work):

| Variable | Use |
| --- | --- |
| `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` | credentials |
| `AWS_REGION` | region (`auto` is used when only an endpoint is set) |
| `AWS_ENDPOINT_URL` (or `AWS_ENDPOINT_URL_S3`) | custom endpoint for R2, MinIO, etc.; path style addressing is used for custom endpoints |

The endpoint and region can also be given in the URL (`s3://bucket/prefix?endpoint=https://host&region=auto`); the URL wins over the environment. Never put credentials in the URL; the module strips query and userinfo from every URL it prints.

### Examples

```sh
# local path (another disk, NFS mount)
TOKI_REPLICA_URL=file:///mnt/backup/tokibase ./toki serve

# AWS S3
AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=... AWS_REGION=ap-southeast-1 \
TOKI_REPLICA_URL=s3://my-bucket/prod ./toki serve

# Cloudflare R2
AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=... \
AWS_ENDPOINT_URL=https://<account>.r2.cloudflarestorage.com \
TOKI_REPLICA_URL=s3://my-bucket/prod ./toki serve

# MinIO
AWS_ACCESS_KEY_ID=minio AWS_SECRET_ACCESS_KEY=minio12345 AWS_ENDPOINT_URL=http://127.0.0.1:9000 \
TOKI_REPLICA_URL=s3://tokibase/prod ./toki serve
```

## Checkpoints and PocketBase maintenance

PocketBase runs `PRAGMA wal_checkpoint(TRUNCATE)` nightly (cron `__pbDBOptimize__`) and before backups. A TRUNCATE checkpoint waits for all readers, which includes Litestream's read lock, and holds writers back while it waits. Litestream manages checkpoints itself (passive checkpoints by page count and time, truncating when the WAL grows large).

While replication is active the module therefore sets the app store key `kernel.StoreKeyDisableCheckpoint` (`@disableWALCheckpoint`). The sqlite store then skips the manual checkpoint in the nightly maintenance and in backups; `PRAGMA optimize` still runs and `VACUUM INTO` based backups are unaffected. The flag is removed when replication stops.

## Backups

Built-in backups (`/api/backups`, autobackup) keep working and are independent: they are zip files made with `VACUUM INTO`. Replication is not a backup of file storage (`pb_data/storage`): only the two databases are replicated. Keep using S3 file storage or your own copy for uploaded files. Backups exclude the Litestream metadata directories.

## Status and health

- Superusers get `data.replica` in `GET /api/health` while replication is active: `{healthy, reason?, databases: [{name, path, replicaUrl, localTxid, replicaTxid, lastSync, lagSeconds, lastError, lastErrorAt}]}`. Guests, regular users and superusers on a server without replication see the unchanged response (see `docs/COMPAT.md`).
- `lagSeconds` is 0 while the replica holds every captured transaction, otherwise the seconds since the last successful sync. `healthy` turns false on an unresolved error or when pending changes are older than `max(30s, 10 x sync interval)`.
- Sync failures are logged at Error (at most one line per 30 s) as `walreplica` entries; the first error also shows up as `lastError` until a later sync succeeds.
- `toki replica status [--url <url>] [--json]` reads the replica itself (no running app needed): newest transaction and its age, snapshots, oldest restore point, file count and bytes per database.

## CLI

```
toki replica status [--url URL] [--json]
toki replica restore --dir <pb_data> [--url URL] [--timestamp RFC3339] [--overwrite]
toki replica snapshot
```

`--url` defaults to `$TOKI_REPLICA_URL`. `status` and `restore` do not bootstrap the app. `--dir` is the global flag and is required for `restore`.

`snapshot` starts replication in its own process, syncs, writes a snapshot of both databases and exits. Run it only when no server is replicating the same `pb_data` (two replicators on one database corrupt the replica history); a running server snapshots on its own every `TOKI_REPLICA_SNAPSHOT_INTERVAL`. For the same reason the short lived commands `superuser`, `rule`, `migrate`, `version` and `replica status|restore` never start replication.

## Restore procedure

1. Stop the TokiBase server that uses the target `pb_data` (and make sure nothing else replicates to the same URL).
2. Check what is available: `toki replica status --url <url>`.
3. Restore into an empty directory (the safe default; `restore` refuses when `data.db` already exists):
   `toki replica restore --url <url> --dir ./pb_data_restored`
   For a point in time (within the retention window): add `--timestamp 2026-10-07T10:00:00Z`.
4. To replace the live directory instead, move it aside (`mv pb_data pb_data.old`) and restore into `pb_data`, or add `--overwrite` to delete the existing `data.db`/`auxiliary.db` (and their `-wal`/`-shm`) first. `--overwrite` destroys the local databases; use it only when they are known bad.
5. Copy `pb_data/storage` back from your file backup if you use local file storage (it is not replicated).
6. Start the server on the restored directory. If it should resume replicating into the same URL, set `TOKI_REPLICA_URL` again; to avoid mixing histories, point it at a new, empty URL (or clear the old prefix) after a restore from an older point in time. Replicating a restored database back into the URL it was restored from has not been verified.

`auxiliary.db` is skipped silently when the replica holds no copy of it; `data.db` is required. Each restored file gets a quick integrity check.

## RPO and RTO

- RPO: about `TOKI_REPLICA_SYNC_INTERVAL` (default 1 s) plus upload time while the replica is reachable and healthy. With an S3 outage the primary keeps serving and the lag grows until the store is back (local LTX files are retained meanwhile; see `lagSeconds`/`/api/health`).
- RTO: dominated by download and restore time (snapshot plus the LTX files since). Seconds for small databases; for large ones about the database size divided by your bandwidth. Restore is a manual procedure today.

## Library notes

- Litestream's Go API is explicitly unstable; the version is pinned in `go.mod` (v0.5.17).
- Only `modernc.org/sqlite` may be used in the process (the TokiBase default); mixing drivers causes lock problems on POSIX.
- Only the `file` and `s3` backends are linked in (binary size).

## Limitations

- Single primary: exactly one running process may replicate to a given URL.
- The replica is a read-only copy of LTX files, not a live database you can query; there is no replica read mode.
- No automatic promotion or failover yet: recovering means running the restore procedure and starting a server on the result.
- Only `data.db` and `auxiliary.db`; `pb_data/storage`, `backups/` and `pb_hooks` are not replicated.
- `toki replica snapshot` cannot talk to a running server.

## Go API

`walreplica.FromEnv()`, `walreplica.Register(app)`, `walreplica.RegisterWithConfig(app, cfg)`, `walreplica.Status(app)`, `walreplica.Healthy(app)`, `walreplica.Active(app)`, `walreplica.Snapshot(ctx, app)`, `walreplica.Inspect(ctx, url)`, `walreplica.Restore(ctx, url, destDataDir, RestoreOptions{Timestamp, Overwrite})`. `tokibase.New*` calls `Register`.
