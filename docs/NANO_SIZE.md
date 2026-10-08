# nano size: where the bytes are, and the plan

Measured on linux/amd64, Go 1.27.1, `CGO_ENABLED=0 -trimpath -ldflags "-s -w"`, `./examples/base`, nano tags from `profiles.txt`.

| | MiB |
| --- | --- |
| nano at main before this change | 22.2 (23,253,152 B) |
| nano now (`no_thumbs no_oauth2 no_s3fs` added) | 21.1 (22,130,848 B) |
| linux/arm64 now | 19.9 |
| Android `libgojni.so` arm64 / x86_64 now (before: 23.0 / 24.4) | 21.9 / 23.2 |
| design goal | 14 |

Honest result: with the tags that exist or are cheap, nano is about 21 MiB. 14 MiB is not reachable without removing `net/http` from the dependency graph or replacing SQLite, both outside this phase.

## Anatomy of the binary

Unstripped sections of the previous nano build: `.text` 11.0 MiB, `.gopclntab` 7.9 MiB (kept by `-s -w`; the runtime needs it for stack traces, GC maps and `defer`; it grows with code size, roughly 0.7 B per byte of text), `.rodata` 0.8 MiB, `.data`/`.noptrdata` 1.4 MiB. `.symtab`/DWARF are the only parts `-s -w` removes. So every KiB of removed code saves about 1.7 KiB of binary.

Top packages by `go tool nm -size` (text + rodata of the symbol, pclntab not included; script: aggregate by import path), nano before this change:

| KiB | Package |
| ---: | --- |
| 1860 | modernc.org/sqlite/lib (transpiled SQLite) |
| 1229 | go: data (`go:func.*`, string tables) |
| 608 | tokibase/kernel |
| 539 | runtime |
| 349 | crypto/tls |
| 302 | net/http |
| 300 | tokibase/apis |
| 215 | tokibase/core |
| 203 | net/http/internal/http2 |
| 177 | net |
| 175 | encoding/json/v2 |
| 158 | reflect |
| 155 | crypto/internal/fips140/nistec |
| 153 | crypto/x509 |
| 131 | tokibase/modules/crypto |
| 125 | spf13/pflag |
| 117 | pocketbase/dbx |
| 112 | modernc.org/libc |
| 100 | math/big |
| 98 | modernc.org/sqlite |
| 89 | tokibase/modules/sync |
| 88 | spf13/cobra |
| 88 | tokibase/tools/auth (32 OAuth2 providers) |
| 86 | encoding/json/jsontext |
| 84 | time |
| 82 | database/sql |
| 77 | encoding/xml (used by `tools/router`) |
| 76 | golang.org/x/net/html (mailer html2text, picker) |
| 74+74 | x/text/unicode/norm (vendored twice: std and module) |
| 74 | html/template |
| 71 / 68 | text/template, text/template/parse |
| 71 | compress/flate |
| 62 | tokibase/migrations |
| 62 | regexp/syntax |
| 62 / 61 | modules/computed, modules/batchguard |
| 59 | fmt |
| 58 | tools/search |
| 53 / 50 / 35 | mimetype/magic, x/crypto/acme, acme/autocert |
| 24+33+32+20+10 | imaging, x/image/tiff, vp8, vp8l, bmp (removed by `no_thumbs`) |

Groups (new binary, text+rodata): SQLite + libc about 2.1 MiB; net/http + TLS + HTTP/2 + x509 + net + nistec about 1.4 MiB; TokiBase kernel/core/apis/router about 1.1 MiB; modules (crypto, sync, computed, batchguard, ...) about 0.5 MiB; JSON v1/v2 0.25 MiB; templates (html + text) 0.3 MiB; cobra + pflag 0.2 MiB. The module list (`go version -m`) has 29 dependencies; goja, webauthn, the AWS SDK, `fexpr` aside (17 KiB), nothing large is left: `no_jsvm` really removes goja (verified: not in `go list -deps`), the admin UI is out.

## Done in this change (nano only; solo/team/cluster/edge untouched)

| Tag | Saves (estimate from symbol sizes; only the total was measured) | Notes |
| --- | --- | --- |
| `no_thumbs` | about 0.4 MiB | `imaging` + `x/image` (tiff, vp8, vp8l, webp, bmp) |
| `no_oauth2` | about 0.3 MiB | provider registration moved to `tools/auth/providers_register.go`; without it the linker drops all 32 providers (`oauth2` itself stays: `apis` uses its types) |
| `no_s3fs` | about 0.2 MiB | custom S3 client in `tools/filesystem/internal/s3blob` |
| together | 1.07 MiB (23,253,152 to 22,130,848 B) | AAR `.so`: -1.15 MiB (arm64) |

`no_mailer` was evaluated and not done: SMTP client + mailyak are 30 KiB, `mails` 11 KiB, and `html/template`/`x/net/html` are shared with `kernel` and `picker`. No single remaining optional item is at or above 300 KiB.

## What `no_http` (kernel + `embed.Call` only) would save

Estimate, not implemented. `embed.Call` runs the real router and `apis`, so a kernel-only build means: no `apis` (300 KiB), no `tools/router` (32 KiB + xml 77 KiB), no autocert/acme (85 KiB), no HTTP/2 server side (about 100 KiB of the 203 KiB), no `serve`/`cmd` CLI (cobra + pflag 213 KiB). About 0.8 MiB of text, so about 1.4 MiB of binary. `net/http`, TLS, x509 (about 1 MiB text, 1.7 MiB binary) stay as long as any module uses `http.Client`: `sync` (two-way sync is the point of nano), OAuth is already out, mailer, webhooks (out in nano). Dropping them too needs a tiny HTTP client behind an interface for `sync`; it would be a further 1.5 to 2 MiB but is a large refactor (the REST surface that mobile apps rely on is `apis`).

Realistic ceiling with a `no_http` + `no_cli` refactor: about 18 to 19 MiB. 14 MiB needs also replacing the transpiled SQLite (2.1 MiB text, 3.5 MiB binary) or a stripped-down SQLite, which is not realistic.

## Plan (ordered by return on effort)

1. `no_cli` for the mobile target: `embed` links `cmd` (cobra, pflag, about 0.35 MiB binary). Move the bootstrap that embed needs out of `cmd`. Small refactor.
2. De-duplicate `x/text/unicode/norm` (std vendors its own copy; the module copy comes from `x/net/idna`/`x/text` users): find the importer, about 0.25 MiB.
3. `no_http` kernel-only package (above), about 1.4 MiB, then replace `http.Client` use in `sync`, about 1.5 to 2 MiB more.
4. Per-ABI: ship only `arm64-v8a` in the AAR for release (`ANDROID_TARGETS=android/arm64`), x86_64 only for emulators/CI; this halves the AAR without touching code. The Play Store delivers one ABI per device anyway (AAB).
5. Revisit the 14 MiB goal in `docs/ARCHITECTURE.md`: re-baseline to about 19 MiB after items 1 to 3.
