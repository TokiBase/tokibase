# Module `devicecert`

X.509 identity for edge nodes. The hub keeps a private ECDSA P-256 certificate authority; a node gets a short-lived **server leaf** from it over the existing sync session and serves HTTPS with it on a second port. Package `modules/devicecert` (hub endpoint and renewal call in `modules/sync`: `devcert.go`, `client/devcert.go`). Plan: `docs/EDGE_MODULES_PLAN.md` section 6. PR 6 added the hub CA, the edge TLS listener and the CLI; PR 7 adds client certificates for LAN peers, the route allowlist, revocation through sync, `rotate-ca` and `/api/device/*`.

- Opt in with `TOKI_DEVICECERT=on` (default off). The `toki devicecert` command is always present.
- Build tag `no_devicecert` compiles the module out (the `nano` profile does). Setting `TOKI_DEVICECERT` on such a binary is refused at boot by the stubbed-module guard.
- Needs sync: the CA lives on the hub (`TOKI_SYNC_ROLE=hub`) and is unlocked by the hub key. A node that is not a hub gets its leaf from the hub; without sync the module has nothing to issue.
- The existing JWS node certificate is unchanged and still the hub-to-spoke credential. Upstream `--https` (autocert) is unchanged too.

## How it works

1. **Hub CA.** On first use the hub creates an ECDSA P-256 root (10 years, self-signed, `pathlen 1`). The private key is wrapped with AES-256-GCM and stored in the plain table `_devicecert_state` (key `ca`). The wrapping key is `HKDF-SHA256(Ed25519 signature of "toki_devicecert/ca/v1" by the hub key)`, info `toki_devicecert/ca/v1`. Ed25519 signatures are deterministic, so the key can be derived again after a restart, and only whoever holds the hub key (`TOKI_SYNC_HUB_KEY_FILE` or `_sync_state`) can produce it. A copy of `data.db` alone reveals nothing, and the CA follows hub failover because the hub key does. **This protects the CA only when the hub key is kept outside `data.db`** (`TOKI_SYNC_HUB_KEY_FILE`). Without it the hub key and the wrapped CA key live in the same `data.db`, so a copy or backup of the database can mint certificates; the hub logs a warning when it creates the CA, and `toki devicecert status` / the health block (`hub_key_in_db`) say so.
2. **Leaf key.** Each node (hub and spokes) generates a P-256 key in `<dataDir>/devicecert_leaf.key` (mode 0600). It never leaves the node.
3. **Issuing.** A spoke sends `POST /api/sync/devcert {spki, sans}` with its session token (see `docs/modules/sync.md`). The hub issues a 14-day leaf (`serverAuth` only: a server leaf can never be used as a client certificate), records it in `_device_certs`, audits `devicecert.issue` and answers with the leaf and the root. The hub also issues its own leaf locally.
4. **Names.** The leaf carries `<node_id>.edge.toki.local` (always the authenticated node's own id, never a name from the body), the LAN IPs the node reports (every non-link-local address of its interfaces plus `127.0.0.1`) and the entries of `TOKI_DEVICECERT_SANS`. The hub drops DNS names that are not under `.local`, `.lan`, `.home.arpa` or `.internal`, drops names under `.edge.toki.local` (they belong to nodes), and keeps at most 12 SANs.
5. **Renewal.** A spoke checks every 10 s (a local check) whether less than one third of the leaf's life is left, or an address of the node is missing from the leaf, and then fetches a new one through the sync client session, which runs a handshake first when there is none. A failed renewal is retried after 5 minutes. A node must reach the hub at least once per leaf life (14 days by default); raise `TOKI_DEVICECERT_LEAF_DAYS` (at most 90) for nodes that are offline for long.
6. **Bad clocks.** A leaf is valid from one hour before it was issued, so a device whose RTC is a little behind still accepts it.
7. **Listener.** On `OnServe` the module starts a second `http.Server` on `TOKI_DEVICECERT_LISTEN` (for example `:8443`) with the same handler as the main server (`apis/serve.go` is untouched). `tls.Config.GetCertificate` reads the current leaf on every handshake, so a renewed leaf is served without restarting. Until a spoke has its first leaf, handshakes fail with "no edge certificate yet".
8. **tlscheck.** With `TOKI_DEVICECERT=on` and `TOKI_DEVICECERT_LISTEN` set, `modules/tlscheck` still warns about plain HTTP on a non-loopback address and `TOKI_TLS_CHECK=strict` still refuses to start: the second port does not close the plain one (same routes, superuser login included). Bind `--http` to `127.0.0.1` when only the kiosk on the same host needs the plain port.

## Env

| Variable | Meaning |
| --- | --- |
| `TOKI_DEVICECERT` | `on` enables the module (default off) |
| `TOKI_DEVICECERT_LISTEN` | TLS listen address, for example `:8443`. Empty = no listener |
| `TOKI_DEVICECERT_LEAF_DAYS` | lifetime of an edge leaf, default 14, at most 90 |
| `TOKI_DEVICECERT_SANS` | extra comma-separated names and IPs for the leaf |
| `TOKI_DEVICECERT_MTLS` | `off` (default), `optional` or `require`. `optional` = `VerifyClientCertIfGiven`, `require` = `RequireAndVerifyClientCert`, both against the CA pool (current CA plus rotated-out CAs inside the overlap); revoked serials (the deny list) are refused on every connection, resumed sessions included. `require` locks out browsers without a client certificate (the kiosk): use `optional` or the loopback port for them |
| `TOKI_DEVICECERT_CA_OVERLAP_DAYS` | days a rotated-out root stays trusted, default 30 |

## Collections and files

- `_device_certs` (system collection, rules `null`): `serial` (hex, unique), `name`, `kind` (`client`, `server`), `node`, `not_after`, `revoked_at`, `route_scope`. Pulled to the nodes by the pull-only policy below (the deny list).
- `_devicecert_state` (plain table in `data.db`): the wrapped CA key and the root PEM.
- In the data dir of a node: `devicecert_leaf.key` (0600), `devicecert_leaf.pem`, `devicecert_ca.pem` (the root the hub sent).

## CLI

```
toki devicecert ca [--fingerprint]   # root PEM on stdout, fingerprint on stderr
toki devicecert status [--json]      # leaf serial, not_before, not_after, names; listener settings
toki devicecert list [--json]        # rows of _device_certs
```

`ca` creates the CA on the hub when it does not exist; on a node it prints the root that came with the leaf. The commands need `TOKI_DEVICECERT=on` (they create no tables while it is off; `status` is then read-only). Health: the superuser `GET /api/health` has a `devicecert` block (`role`, `listen`, `listening`, `leaf.not_after`, `renew_due`, `ca_fingerprint`, `deny_list` (revoked serials), `cas` (roots trusted now), `hub_key_in_db`, `last_error`).

```
toki devicecert issue --name gate-ctrl-1 --days 90 [--scope /api/scan,/api/print,/api/kiosk/status] [--out dir] [--p12 [--p12-pass pw]]
toki devicecert revoke <serial|name>
toki devicecert rotate-ca [--overlap-days N]
```

## Installing the root

A browser or tablet must trust the root once. Get it with `toki devicecert ca > toki-root.pem` on the hub (or `curl` it from a node's data dir) and compare the fingerprint with `toki devicecert ca --fingerprint` before trusting it.

### Chromium / Chrome on a Raspberry Pi (Linux, NSS)

Chromium on Linux uses the per-user NSS database:

```
sudo apt install libnss3-tools
mkdir -p $HOME/.pki/nssdb && certutil -d sql:$HOME/.pki/nssdb -N --empty-password   # only if it does not exist yet
certutil -d sql:$HOME/.pki/nssdb -A -t "C,," -n "Tokibase Edge CA" -i toki-root.pem
certutil -d sql:$HOME/.pki/nssdb -L        # check
```

Run these as the user that runs the kiosk browser, then restart it. Firefox keeps its own store: add the root under Settings > Privacy & Security > Certificates > View Certificates > Authorities > Import (tick "Trust this CA to identify websites"), or set the policy `Certificates.Install`.

For the whole system (curl, Go, Python): `sudo cp toki-root.pem /usr/local/share/ca-certificates/toki-root.crt && sudo update-ca-certificates`. Android needs the root installed as a CA certificate in Settings > Security > Encryption & credentials; iOS needs the profile installed and then enabled under Settings > General > About > Certificate Trust Settings.

Open the node by one of the names in its leaf: `https://<node_id>.edge.toki.local:8443` (add a hosts or DNS entry) or `https://<lan-ip>:8443`.

## Client certificates for LAN peers (PR 7)

`toki devicecert issue` runs on the hub. It generates a P-256 key and a `clientAuth`-only certificate (at most 365 days), writes `<name>.key.pem` (0600), `<name>.crt.pem` and `ca.pem` (the root bundle) to `--out`, and records a `_device_certs` row (`kind=client`, `route_scope`). `--p12` also writes `<name>.p12` using the `openssl` binary (the password is `--p12-pass` or a random one printed to stderr). Install the key and certificate on the peer; it trusts `ca.pem`.

### Route scope

With `TOKI_DEVICECERT_MTLS=optional|require`, a request on the TLS port with a verified, unrevoked client certificate and NO auth token is a **trusted device** when the `kind=client` row exists on that node and its `route_scope` (comma separated path prefixes) covers the path:

- Route code asks `edgeguard.Device(e)`; the scanner (`/api/scan`, `/api/scan/events`, `/api/scan/scanners`), the printer (`/api/print*`) and `GET /api/kiosk/status` serve it. The actor of a scan or print job is `device:<name>`.
- Rules can test `@request.headers.x_toki_device != ""`; the header is set by the server only inside the scope and is removed from every inbound request, on every port.
- It is NOT a user, NOT a superuser and NOT the service actor. A certificate without a scope proves identity and grants nothing. Scopes must have at least two path segments and can never cover `/api/collections`, `/api/sync`, `/api/batch`, `/api/settings`, `/api/backups`, `/api/files`, `/api/realtime`, `/api/logs`, `/api/crons`, `/api/mcp`, `/api/device`, `/api/health` or `/_` (nor a parent such as `/api`).
- A request that carries an auth token keeps its normal identity. Mapping a certificate to an actor is v1.1.

### Revocation and the deny list

`toki devicecert revoke <serial|name>` sets `revoked_at` (a name revokes every unrevoked row of it). Nodes learn it through sync: add a pull-only policy on the hub **before the nodes enroll** (nodes read the policies at their handshake, up to 15 minutes later for a node that is already running):

```
toki sync policies set _device_certs --direction pull
```

`_device_certs` is the only system collection a sync policy may name (explicit allowlist in `modules/sync/syscollections.go`; it is pulled without the view-rule check because its rules are `null`). The in-memory deny set is re-read at most every 15 s and by a background refresh; the check runs in `tls.Config.VerifyConnection`, so a revoked certificate is refused on resumed sessions too. If the table cannot be read for 5 minutes, every client certificate is refused (fail closed). Offline nodes rely on the certificate lifetime. `toki sync revoke <node>` also revokes the certificates named after that node, so a revoked node's leaf is on the deny list. Rows that expired more than 30 days ago are pruned on the hub (the deletes sync to the nodes).

### CA rotation

`toki devicecert rotate-ca` creates a new root; it signs from now on, and the previous root stays trusted for `--overlap-days` (default `TOKI_DEVICECERT_CA_OVERLAP_DAYS`, 30) on the hub and in the root bundle sent to nodes (PEM header `Toki-Retire-At`). Steps: (1) run it on the hub; (2) install the new root (`toki devicecert ca`, fingerprint check) on browsers, tablets and peers, see below; (3) nodes receive the new root and a leaf signed by it with their next renewal (at most one leaf life, 14 days by default; to force it, delete `devicecert_bundle.pem`, `devicecert_ca.pem` and `devicecert_leaf.pem` on the node and restart it); (4) peers keep working with old-signed client certificates until the overlap ends, so re-issue them within that window. A node pins the first root it received: a bundle without any root it trusts is refused (a node offline for longer than the overlap needs `devicecert_bundle.pem`, `devicecert_ca.pem`, `devicecert_leaf.pem` deleted, then it enrolls its leaf again). If the CA key is lost or stolen, run `rotate-ca --overlap-days 0`, revoke what must go and reinstall roots everywhere.

### Device identity

- `GET /api/device/identity` returns `{node_id, hub_id, cert, leaf_fp}`: `cert` is the compact JWS node certificate issued by the hub (empty on the hub itself), `leaf_fp` the SHA-256 fingerprint of the edge TLS leaf.
- `POST /api/device/attest {"nonce": "<16 to 128 bytes>", "ts": <unix seconds, optional>}` returns `{node_id, hub_id, nonce, ts, alg: "Ed25519", sig (base64), cert}` where `sig = Ed25519(node_key, "toki-attest/v1|<node_id>|<nonce>|<ts>")`. `ts` must be within 5 minutes of the node clock (the node uses its own clock when it is absent). The app verifies the JWS `cert` against the hub public key it trusts (`proto.VerifyCert`) and `sig` with the node public key (`pub` claim), so it knows which enrolled device answered. No key leaves the process. Both endpoints are public, throttled (30 requests per minute and address each), need sync for the identity and answer 501 without it.

## Security notes

- The deny check, the EKU split and the route scope are three layers: a server leaf has no `clientAuth` (and is rejected as a client in `VerifyConnection` and in the scope middleware), a client certificate has no `serverAuth`, and the middleware requires a `kind=client` row.
- A node pins the first root it receives (trust on first use) and refuses an expired or not yet valid leaf and a leaf without its own node name. The hub refuses SANs for public addresses, its own addresses, the bare `edge.toki.local` and more than 4 DNS names; a revoked node gets 403 on `/api/sync/devcert` and its poller stops.
- The deny list is read per process: `toki devicecert revoke` in the CLI reaches a running server within 15 s.
- A node and the hub write the leaf and the roots in one file (`devicecert_bundle.pem`, one atomic rename); `devicecert_leaf.pem` and `devicecert_ca.pem` are derived copies. A damaged `devicecert_leaf.key` is regenerated.

## Limits

- An intermediate CA for offline issuing on the edge is v1.1.
- A node may ask for any private or loopback IP address as a SAN that is not an address of the hub, so an enrolled node can obtain a certificate for another node's LAN address. Enrolled nodes are trusted; do not install the root on devices that must distrust a node.
- Without a hub in the process the module can only be used to print a root that was received earlier.
