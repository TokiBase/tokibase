# Module `tlscheck`

Boot check for servers that probably serve clients over plain HTTP. Package `modules/tlscheck`. No REST contract change.

- Enabled by default: `tokibase.go` calls `tlscheck.Register(app)`.
- Env `TOKI_TLS_CHECK`: `warn` (default), `strict`, `off` (also `false`, `0`, `disabled`).

## Condition

The check runs on `OnServe` (before the listener is created) and triggers only when all hold:

1. The server does not terminate TLS itself: the `serve` command line has no `--https` flag and no positional domain.
2. The listen address is not loopback. `127.0.0.0/8`, `::1`, `localhost` and unix sockets never trigger. `0.0.0.0`, `::`, an empty host (`:8090`) and any other address do.
3. `Settings.TrustedProxy.Headers` is empty.

If a custom `ServeEvent.Listener` is set, its address is used instead of `Server.Addr`.

## Behavior

| Mode | Effect |
| --- | --- |
| `warn` | `Logger.Warn` line `tlscheck: serving plain HTTP on ...` (attr `addr`) and a colored `WARNING` line on stderr |
| `strict` | `serve` fails with the same message before listening |
| `off` | nothing is registered |

## Running behind nginx, Caddy or Cloudflare

This is fine. Terminate TLS at the proxy, bind TokiBase to `127.0.0.1` (never warns) or keep a private address, and set the trusted proxy headers in Settings (for example `X-Forwarded-For`, or `CF-Connecting-IP` for Cloudflare). Once `TrustedProxy.Headers` is non-empty the check stays silent, even on `0.0.0.0`, because the header is the signal that a proxy is in front.

The headers are read from the settings at boot. Changing them requires a restart for the check to see the new value.

## Limits

The check cannot verify that the proxy really terminates TLS or that the port is firewalled; it only detects the unconfigured case.
