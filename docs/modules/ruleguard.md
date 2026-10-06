# ruleguard

Makes public API rules explicit and visible. In PocketBase a rule of `""` means "public to anyone" and `null` means "superusers only"; a forgotten `""` exposes data. ruleguard reports every `""` rule that is not allowlisted. It does not change the REST contract or the collection JSON shape.

Scope: only empty-rule detection (`""` or whitespace) on non-system collections, for `list`, `view`, `create`, `update`, `delete` and, on auth collections, `manage`, `auth`. Rules that are `null` or any expression (e.g. `@request.auth.id != ""`) are never reported.

Note: PocketBase creates new auth collections with `authRule: ""` and the default `users` collection with `createRule: ""`; these show up until allowlisted.

## Policy file

`<dataDir>/ruleguard.json` (usually `pb_data/ruleguard.json`):

```json
{
  "policy": "warn",
  "public": {
    "posts": ["list", "view"],
    "users": ["create", "auth"]
  }
}
```

- `policy`: `warn` (default), `strict` or `off`.
- `public`: collection name to the rule kinds that are intentionally public. Kinds: `list`, `view`, `create`, `update`, `delete`, `manage`, `auth`.
- Missing file = `{"policy":"warn","public":{}}`.

## Behavior

| Policy | Boot | After a collection save (Admin UI / API) |
| --- | --- | --- |
| `off` | nothing | nothing |
| `warn` | one log line per error finding, a summary line, and a colored summary on stderr | log line for each new non-allowlisted `""` rule |
| `strict` | Bootstrap fails and the app refuses to start, listing the findings | same as `warn`: only logged |

Collection saves are never blocked, even in `strict`, to keep the PocketBase Admin UI contract. Only a rule that was not public before the save is reported.

`toki rule lint` and `toki rule allow` skip the boot check, so they work even with a `strict` policy that currently fails.

## CLI

```
toki rule lint [--strict] [--json]
toki rule allow <collection> <kind>...
```

- `lint` prints the findings (`error` = public and not allowlisted, `info` = public and allowlisted). `--json` prints a JSON array of `{collection, rule, severity, message}`.
- `allow` adds kinds to `ruleguard.json` (creates the file if missing).

Exit codes: `0` clean; `1` when there is any error finding, or with `--strict` when there is any finding at all (including `info`).

## CI

```sh
toki rule lint --strict --dir ./pb_data
```

## Go API

`ruleguard.Load(dataDir)`, `ruleguard.Save(dataDir, pol)`, `ruleguard.Lint(app kernel.App, pol)`, `ruleguard.Register(app core.App)` (called by `tokibase.New*`).
