# walreplica

> Build note: default binaries link only the `file://` backend. For `s3://` (AWS S3, Cloudflare R2, MinIO) build with `go build -tags replica_s3 ./examples/base`; the AWS SDK adds about 10 MB and is kept out of `nano`/`edge` by default.

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
- `replica.lease` (same object) shows the lease: `{held, supported, nodeId, hostname, pid, startedAt, heartbeatAt}`. When another node holds the lease and replication was refused, `healthy` is false with `reason` `lease held by <hostname> since <t>` and `lease.held` is false.
- `toki replica status [--url <url>] [--json]` reads the replica itself (no running app needed): newest transaction and its age, snapshots, oldest restore point, file count and bytes per database.

## CLI

```
toki replica status [--url URL] [--json]
toki replica restore --dir <pb_data> [--url URL] [--timestamp RFC3339] [--overwrite]
toki replica snapshot
toki replica promote --url URL --dir <pb_data> [--timestamp RFC3339] [--force]
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
6. Start the server on the restored directory. If it should resume replicating into the same URL, set `TOKI_REPLICA_URL` again; to avoid mixing histories, point it at a new, empty URL (or clear the old prefix) after a restore from an older point in time. Do not replicate a restored database back into the URL it was restored from (see Failover).

`auxiliary.db` is skipped silently when the replica holds no copy of it; `data.db` is required. Each restored file gets a quick integrity check.

## Failover: promote a replica

`toki replica promote` turns the replica into a standalone `pb_data` on a standby host. It restores both databases (`walreplica.Restore`), runs `PRAGMA integrity_check` on each, counts the collections, writes `<dir>/.toki-promoted.json` (`from_url`, `restored_txid` or `timestamp`, `promoted_at`, `hostname`, `collections`) and records a `replica.promote` audit entry (when the audit module is enabled, written into the promoted database). Exit code 0 on success, 1 when refused or failed.

Procedure:

1. Make sure the old primary is stopped or isolated (power off, firewall, or stop the service). If it may still be running, it can keep writing to the replica URL.
2. On the standby host run `toki replica promote --url <replica url> --dir ./pb_data` (add `--timestamp 2026-10-07T10:00:00Z` for a point in time inside the retention window). It refuses when `<dir>/data.db` exists; `--force` first MOVES the directory to `<dir>.pre-promote-<unixts>` (nothing is deleted).
3. Copy `pb_data/storage` from your file backup if you use local file storage (not replicated).
4. Start the new primary with a NEW, empty replica URL: `TOKI_REPLICA_URL=<new url> toki serve --dir ./pb_data`.
5. Point traffic (DNS, proxy) at the standby. Keep the old URL untouched until the old primary is confirmed dead; then archive or delete it.

Why a new URL (Litestream caveat): replication is a linear history of LTX files per database. A node restored from the replica starts a new local history that continues at the restored transaction id. If the old primary is still alive, or later comes back, and both write to the same URL, the two histories interleave and the replica becomes unrestorable. Litestream's own guidance is the same: restore, then replicate to a fresh URL. The promoted node therefore must not reuse the URL it was restored from; the lease guard below is a second line of defence, not a replacement for this rule.

## Lease: one replicator per URL

When replication starts the node writes a lease object `<url>/.toki-lease.json`: `{node_id, hostname, pid, started_at, heartbeat_at}`. `node_id` is random, persisted in `<pb_data>/.toki-node-id`, so a restart of the same `pb_data` is never blocked. The heartbeat is refreshed every `max(10s, 10 x sync interval)`.

- On start, a lease with a different `node_id` whose heartbeat is newer than `max(60s, 3 x heartbeat interval)` makes the module refuse to replicate: the server keeps serving, an Error is logged, and `/api/health` reports `replica.healthy=false`, reason `lease held by <hostname> since <t>`.
- `TOKI_REPLICA_TAKEOVER=1` overrides the refusal (use it only after the other node is confirmed stopped).
- A lease older than that threshold is treated as dead and replaced. A clean shutdown removes the lease.
- The heartbeat never overwrites a lease another node took over; it logs an Error instead.
- Backend support: the Litestream `ReplicaClient` has no generic object put/get, so the lease is implemented for `file://` replicas only. For `s3://` replicas the lease is NOT implemented yet (a warning is logged at start) and the single primary rule is on the operator.
- The lease is advisory: it cannot stop a node that ignores it (for example one started before the lease existed) and file systems without atomic rename or shared visibility (some network mounts) weaken it.

## Failover drill

`tests/e2e/failover.sh` (CI job `failover`) builds the binary, starts a primary with `TOKI_REPLICA_URL=file://<tmp>/rep`, creates a superuser, a `posts` collection and 10 records, waits until `/api/health` shows the replica caught up, kills the primary with SIGKILL, runs `toki replica promote`, starts the standby on another port with a fresh replica URL, asserts the 10 records and prints the RTO (kill until the standby answers `/api/health`, promote included). It fails above `TOKI_RTO_MAX` seconds (default 30).

## RPO and RTO

- RPO: about `TOKI_REPLICA_SYNC_INTERVAL` (default 1 s) plus upload time while the replica is reachable and healthy. With an S3 outage the primary keeps serving and the lag grows until the store is back (local LTX files are retained meanwhile; see `lagSeconds`/`/api/health`).
- RTO: dominated by download and restore time (snapshot plus the LTX files since). Seconds for small databases; for large ones about the database size divided by your bandwidth. Promotion is a manual command (no automatic failover). Measured by the drill on a small database (10 records, file:// replica, local disk): RTO about 0.2-1 s from kill to a healthy standby; RPO 0 records because the replica was in sync before the kill (in general up to the sync interval).

## Library notes

- Litestream's Go API is explicitly unstable; the version is pinned in `go.mod` (v0.5.17).
- Only `modernc.org/sqlite` may be used in the process (the TokiBase default); mixing drivers causes lock problems on POSIX.
- Only the `file` and `s3` backends are linked in (binary size).

## Limitations

- Single primary: exactly one running process may replicate to a given URL. The lease guards `file://` replicas only.
- The replica is a read-only copy of LTX files, not a live database you can query; there is no replica read mode.
- No automatic failure detection or failover: a human or your orchestrator runs `toki replica promote`.
- Only `data.db` and `auxiliary.db`; `pb_data/storage`, `backups/` and `pb_hooks` are not replicated.
- `toki replica snapshot` cannot talk to a running server.
- A forced `walreplica.Snapshot` can collide with Litestream's own snapshot monitor writing the same snapshot file (the file client stages uploads in a fixed `<name>.tmp`, the loser gets ENOENT on rename). `Snapshot` retries up to 6 times with backoff on that error.

## Go API

`walreplica.FromEnv()`, `walreplica.Register(app)`, `walreplica.RegisterWithConfig(app, cfg)`, `walreplica.Status(app)`, `walreplica.Healthy(app)`, `walreplica.Active(app)`, `walreplica.Snapshot(ctx, app)`, `walreplica.Inspect(ctx, url)`, `walreplica.Restore(ctx, url, destDataDir, RestoreOptions{Timestamp, Overwrite})`, `walreplica.Promote(ctx, url, dir, PromoteOptions{Timestamp, Force})`, `walreplica.SetAuditSink(fn)`, `walreplica.LeaseInfo(app)`. `tokibase.New*` calls `Register`.
