# Embedding TokiBase (nano as a library)

Package `embed` runs the server inside your process: no CLI, no signal handlers, an ephemeral loopback port, in-process requests. Package `mobile` wraps it for gomobile (Android AAR, iOS XCFramework). Nothing uses cgo.

## Go

```go
inst, err := embed.Start(embed.Options{
    DataDir: dir,                 // required; app sandbox on mobile
    Listen:  "127.0.0.1:0",       // default for embed; "-" = no TCP listener at all (mobile default)
    Profile: "nano",              // default
    Env:     map[string]string{"TOKI_LOCKOUT": "off"},
    LogLevel: "warn",
})
if err != nil { /* ... */ }
defer inst.Stop(ctx)

inst.Superuser("me@example.com", "long-password")          // first run
status, hdr, body, err := inst.Call("GET", "/api/health", nil, nil) // no TCP round trip
cancel, err := inst.Subscribe("posts/*", func(ev []byte) { /* {"action":"create","record":{...}} */ })
inst.Export(ctx, file)                                      // backup zip
```

| Option | Meaning |
| --- | --- |
| `DataDir` | Data directory. One running `Instance` per directory per process; a second `Start` fails. Freed by `Stop`. |
| `Listen` | Default `127.0.0.1:0` in `embed`. `-` serves nothing over TCP: only `Call`/`Subscribe` work (best for mobile: no port other apps or web pages can reach; `mobile.Start` uses it when `listen` is empty). On a TCP listener no CORS origin is granted (`AllowedOrigins`, default none) and a `Host` other than `localhost`, `127.0.0.1`, `[::1]` or `AllowedHosts` gets 403 (DNS rebinding). Any local app can still connect to an open port: prefer `-`. |
| `Profile` | `nano` (default), `edge`, `solo`, `team`, `cluster`. Run time only: turns compiled-in modules off through `TOKI_*` switches. What is compiled in is decided by build tags (`profiles.txt`). Switches of modules already compiled out are not set (the stubbed module boot guard would refuse them). |
| `HooksDir` | JS `pb_hooks` directory (no file watching). Build with `-tags no_jsvm` (part of the nano set) or `-tags no_embed_jsvm` to drop the JS engine (about 7 MiB). |
| `Env` | Environment variables, applied with `os.Setenv` at `Start` together with the profile switches. Process wide: the previous values come back when the last running instance stops, and `Start` resets switches of other profiles first (a `team` start after `nano` does not inherit `TOKI_AUDIT=off`). Starts are serialised; use ONE profile per process while instances run. Wins over profile defaults. |
| `EncryptionEnv` | Name of the env var holding the settings encryption key. `--encryptionEnv` is not parsed in an embedded app (`SkipFlagParse`), so set it here. |
| `AllowedOrigins`, `AllowedHosts` | See `Listen`. |
| `LogLevel` | `debug`, `info` (default), `warn`, `error`; stored in the app log settings. |
| `MaxBodyBytes` | Request body cap for TCP and `Call`, 413 above it. Default 4 MiB (`DefaultMaxBodyBytes`), negative = unlimited. Raise it if the app uploads larger files. |

Notes:

- `Call` runs the real router and middlewares (auth, rules, rate limit, hooks); `RemoteAddr` is `127.0.0.1`, `Host` is `localhost`. `/api/realtime` (SSE, any method or case) is refused: use `Subscribe`. `Call` times out after 60 s (`DefaultCallTimeout`); `CallContext(ctx, ...)` takes your own context. The response is buffered in memory (no streaming or large downloads), multiple header values are joined with `, ` (so `Set-Cookie` is merged) and `RemoteAddr` is always loopback: do not proxy untrusted requests into `Call` and rely on loopback checks.
- `Subscribe` is anonymous (public rules only). `SubscribeAs(token, topic, fn)` uses the access of an auth token. `fn` runs on its own goroutine, panics in it are recovered, and it must not block. `Subscribe` fails after `Stop`.
- `Export` currently writes the standard backup zip (data.db, auxiliary.db, storage). A schema + JSONL export is planned; the zip is restorable with the usual restore.
- `Stop` runs `OnTerminate` (graceful HTTP shutdown, replica flush when compiled in), closes the databases and waits for in-flight `Call`s. When `ctx` expires first, `Stop` returns an error and keeps the DataDir reserved (a second `Start` on it is refused); call `Stop` again to finish. The DataDir is released only after the server really stopped.
- Several instances may run in one process (one per DataDir). `kernel.OnBatchFor(app)` handlers are per instance.
- `Export` uses the configured backups filesystem (with S3 backups configured the temporary zip is uploaded there and deleted best effort) and is not exposed in `mobile`. `LogLevel` is stored in the app settings.
- On case-insensitive file systems `/Data` and `/data` count as two DataDirs: always pass the same spelling.
- No installer link is created; use `Superuser`.
- `Instance.App()` exposes the app to Go callers (hooks, direct queries). It is not in the mobile bindings.

## Mobile bindings

```
mobile.Start(dataDir, listen, envJSON) (*Handle, error)
Handle.URL() / Call(method, path, headersJSON, body) (*Response, error)
Handle.Superuser(email, password) / Stop()
Handle.Subscribe(topic, EventCallback) (int, error) / SubscribeAs(token, topic, cb) / Unsubscribe(id)
type EventCallback interface{ OnEvent(data []byte) }
Response{Status int; HeadersJSON string; Body []byte}
```

`listen` empty or `-` = no TCP listener (opt in with e.g. `127.0.0.1:0`). `Subscribe` returns an error after `Stop` or with a nil callback. `envJSON` is a JSON object of strings. The keys `profile`, `hooksDir` and `logLevel` select those options instead of being exported. Callbacks arrive on a background thread: hop to the UI thread yourself.

Build (needs gomobile, plus Android SDK/NDK or macOS with Xcode; not run in CI):

```sh
go install golang.org/x/mobile/cmd/gomobile@latest && gomobile init
make aar           # out/tokibase.aar          (androidapi 24)
make xcframework   # out/TokiBase.xcframework
TAGS="no_mcp no_ui" mobile/build.sh android   # other tag set
```

`mobile/build.sh` passes the nano tags from `profiles.txt` (`-tags`) to `gomobile bind`, and prints install instructions when a tool is missing.

## Flutter sketch

Two workable shapes:

| | Platform channel (recommended first) | FFI |
| --- | --- | --- |
| How | Kotlin/Swift wrapper around the AAR/XCFramework, `MethodChannel` for `call`, `EventChannel` for `subscribe` | `dart:ffi` to a C shim (needs cgo `-buildmode=c-shared`, which this repo does not ship) |
| Pros | Works with the generated bindings as is | No channel hop, one binary interface |
| Cons | Bytes cross two bridges (JSON strings) | Own C ABI to maintain |

Simplest integration: start with `listen = "127.0.0.1:0"`, read `URL()` over the channel and point the regular PocketBase/Dart HTTP client (and its SSE realtime) at it. Switch to `Call` through the channel (with `listen = "-"`) when the open loopback port is not acceptable. Sketch:

```dart
final url = await channel.invokeMethod<String>('start', {'dataDir': dir});
final pb = PocketBase(url!);
```

## Platform lifecycle

- **Android**: the server lives with the process. Start it in a foreground service (or in `Application.onCreate` for foreground use only) and call `Stop` from `onDestroy`/`onTrimMemory` at the latest. The OS can kill the process anytime, SQLite WAL makes that safe, but there is no replica flush. Without a foreground service the process is frozen in the background and loopback connections stall. DataDir: `Context.getFilesDir()`, not external storage.
- **iOS**: no background processes. The server runs only while the app is active; stop it on `applicationDidEnterBackground` (or after `beginBackgroundTask` finishes) and start it again on foreground; `Start` after `Stop` on the same directory is supported. DataDir: Application Support (excluded from backup if the data is rebuildable). `mobile.Start` has no TCP listener unless you pass an address; keep it that way where possible: ATS allows loopback HTTP, but any other local app could reach an open port.
- Both: JS hooks and cron run only while the process is alive. Realtime clients must resubscribe after a restart.

## What nano excludes

Tags `no_mcp no_passkey no_push no_webhooks no_ui no_adminlock no_replica no_backupcheck no_audit no_wasm no_jsvm no_ghupdate no_migratecmd no_totp no_geo no_thumbs no_oauth2 no_s3fs` (the nano line of `profiles.txt`, plus `no_payments no_roles`): no admin UI, no MCP, passkeys, push, webhooks, audit log, WASM hooks, WAL replication, backup verification, JS hooks (`HooksDir` is refused), TOTP, geo, image thumbnails, the OAuth2 browser-flow providers (use `nativeauth`, Google/Apple id_token) and the S3 file system driver. Kept: REST, realtime, auth (password/OTP/native id_token), sessions, lockout, rules and ruleguard, fieldperm, crypto, computed, jobs, timelint. Both `no_jsvm` and `no_embed_jsvm` drop the JS engine from the embed package.

## Android AAR on Linux

Verified on Debian 12 x86_64 (8 cores): `mobile/build.sh android` builds the AAR in about 55 s cold (18 s with warm caches), Go 1.27, gomobile `golang.org/x/mobile@v0.0.0-20260908204917-8b95e45f8d3e`, NDK r27c.

```sh
# 1. JDK 17 (headless Temurin tarball is enough; javac is required)
mkdir -p /root/jdk17 && curl -sL "https://api.adoptium.net/v3/binary/latest/17/ga/linux/x64/jdk/hotspot/normal/eclipse" | tar xz -C /root/jdk17 --strip-components=1
export JAVA_HOME=/root/jdk17 PATH=/root/jdk17/bin:$PATH

# 2. Android SDK (command line tools: https://developer.android.com/studio#command-line-tools-only)
mkdir -p /root/android-sdk/cmdline-tools
unzip commandlinetools-linux-11076708_latest.zip -d /root/android-sdk/cmdline-tools
mv /root/android-sdk/cmdline-tools/cmdline-tools /root/android-sdk/cmdline-tools/latest
yes | /root/android-sdk/cmdline-tools/latest/bin/sdkmanager --sdk_root=/root/android-sdk --licenses
/root/android-sdk/cmdline-tools/latest/bin/sdkmanager --sdk_root=/root/android-sdk \
    "platforms;android-34" "build-tools;34.0.0" "ndk;27.2.12479018" "platform-tools"
export ANDROID_HOME=/root/android-sdk ANDROID_NDK_HOME=/root/android-sdk/ndk/27.2.12479018   # NDK is auto-detected under $ANDROID_HOME/ndk when unset

# 3. gomobile
export PATH=/usr/local/go/bin:/root/go/bin:$PATH GOFLAGS=-mod=mod
go install golang.org/x/mobile/cmd/gomobile@latest && go install golang.org/x/mobile/cmd/gobind@latest && gomobile init

# 4. build (only the ABIs you ship; default is all four)
ANDROID_TARGETS=android/arm64,android/amd64 make aar      # out/tokibase.aar
```

Notes: `/tmp` mounted `noexec` needs `export GOTMPDIR=$HOME/gotmp`. `GOFLAGS=-mod=mod` lets gomobile add `golang.org/x/mobile/bind` to `go.mod` for the build; `mobile/build.sh` restores `go.mod`/`go.sum` on exit. `make aar` has never been run without `ANDROID_TARGETS`, which also builds `arm` and `386` (about twice the time and size). Do not run an emulator for this; test the facade with `go test ./mobile/...` (the same code, host build) and finish on a real device or Play internal testing.

Result with the nano tags (`-trimpath -ldflags "-s -w"`, androidapi 24, arm64 + amd64):

| | before this change | now |
| --- | --- | --- |
| `tokibase.aar` | 18.0 MiB (18,912,662 B) | 17.2 MiB (18,076,060 B) |
| `jni/arm64-v8a/libgojni.so` | 23.0 MiB (24,160,224 B) | 21.9 MiB (22,946,080 B) |
| `jni/x86_64/libgojni.so` | 24.4 MiB (25,600,832 B) | 23.2 MiB (24,315,776 B) |
| `classes.jar` | 13 KiB | 13 KiB |

Java API (package `mobile`, from `classes.jar`): `Mobile.start(String dataDir, String listen, String envJSON) -> Handle`; `Handle.call(method, path, headersJSON, byte[] body) -> Response`, `url()`, `superuser(email, password)`, `stop()`, `subscribe(topic, EventCallback) -> long`, `subscribeAs(token, topic, cb) -> long`, `unsubscribe(long)`; `Response.getStatus()/getHeadersJSON()/getBody()` (plus setters); `interface EventCallback { void onEvent(byte[]) }`. All of them have their `Java_mobile_*` JNI symbols in `libgojni.so` (`readelf --dyn-syms`). Runtime proof needs a device; the host test `TestFacade` exercises Start, Superuser, Call, Subscribe and Stop of the same Go code.

XCFramework (`make xcframework`) needs macOS with Xcode and is not verified yet (pending).

## Size

Stripped (`-trimpath -s -w`, `CGO_ENABLED=0`), current nano tags (see [NANO_SIZE.md](NANO_SIZE.md)):

| Binary | linux/amd64 | linux/arm64 | Android `libgojni.so` (arm64 / x86_64) |
| --- | --- | --- | --- |
| `examples/base` | 21.1 MiB | 19.9 MiB | n/a |
| `mobile` (gomobile) | n/a | n/a | 21.9 / 23.2 MiB |

The earlier table (29.3 MiB for `examples/base`) was measured before `no_jsvm no_ghupdate no_migratecmd no_totp no_geo`; `no_embed_jsvm` is only needed when you build `examples/embed` without the nano tag set. The 14 MiB goal is not reached: see NANO_SIZE.md.
