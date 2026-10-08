# End-to-end compat suites

Both suites serve an unchanged `pb_data` created by upstream PocketBase v0.40.4
(`seed.sh`) with the TokiBase binary and drive it with the official SDKs.

| Suite | Run | Needs |
| --- | --- | --- |
| JS SDK (`sdk/`) | `tests/e2e/run.sh` | Go, Node 22 |
| Dart SDK (`dart/`) | `tests/e2e/run-dart.sh` | Go, Dart SDK 3.x |

Both start the server on a random loopback port with a throwaway copy of the
seeded `pb_data`. `run-dart.sh` builds the binary once, or reuses `TOKI_BIN=/path/to/toki`.
The seed is cached in `${TOKI_E2E_DIR:-$TMPDIR/toki-e2e}`.

Dart suite scenarios: health, superuser/user password auth, rules and CRUD,
error shapes (400/401/403/404, validation), realtime create/update/delete,
file upload, protected file token, auth refresh, impersonate, batch,
expand/filter/sort/fields, `@request.auth` filters, `authStore` persistence callbacks.

## Edge gate

`edge-gate.sh` (Linux only, CI job `e2e-edge-gate`, about 25 s): a solo hub and a spoke built from the `edge` tags of `profiles.txt`, with a TCP stub printer (`edgegate/stubprinter.py`) and a pty scanner (`ptyscanner.py`). Covers kiosk pairing, scans, a printed ticket, a client certificate on the TLS port, its revocation, five offline tickets with reserved numbers, convergence and the kiosk lock. Walkthrough: `docs/EDGE_GATE.md`. `TOKI_BIN_HUB` / `TOKI_BIN_EDGE` reuse prebuilt binaries.
