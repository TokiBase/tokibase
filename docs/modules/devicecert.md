# Module `devicecert`

X.509 identity for edge nodes. The hub keeps a private ECDSA P-256 certificate authority; a node gets a short-lived **server leaf** from it over the existing sync session and serves HTTPS with it on a second port. Package `modules/devicecert` (hub endpoint and renewal call in `modules/sync`: `devcert.go`, `client/devcert.go`). Plan: `docs/EDGE_MODULES_PLAN.md` section 6. This is PR 6 (hub CA, edge TLS listener, CLI); client certificates for LAN peers, the route allowlist, revocation sync, `rotate-ca` and `/api/device/*` are PR 7.

- Opt in with `TOKI_DEVICECERT=on` (default off). The `toki devicecert` command is always present.
- Build tag `no_devicecert` compiles the module out (the `nano` profile does). Setting `TOKI_DEVICECERT` on such a binary is refused at boot by the stubbed-module guard.
- Needs sync: the CA lives on the hub (`TOKI_SYNC_ROLE=hub`) and is unlocked by the hub key. A node that is not a hub gets its leaf from the hub; without sync the module has nothing to issue.
- The existing JWS node certificate is unchanged and still the hub-to-spoke credential. Upstream `--https` (autocert) is unchanged too.

## How it works

1. **Hub CA.** On first use the hub creates an ECDSA P-256 root (10 years, self-signed, `pathlen 1`). The private key is wrapped with AES-256-GCM and stored in the plain table `_devicecert_state` (key `ca`). The wrapping key is `HKDF-SHA256(Ed25519 signature of "toki_devicecert/ca/v1" by the hub key)`, info `toki_devicecert/ca/v1`. Ed25519 signatures are deterministic, so the key can be derived again after a restart, and only whoever holds the hub key (`TOKI_SYNC_HUB_KEY_FILE` or `_sync_state`) can produce it. A copy of `data.db` alone reveals nothing, and the CA follows hub failover because the hub key does. Keep the hub key in a key file (`TOKI_SYNC_HUB_KEY_FILE`) so it is not in the same database as the wrapped CA key.
2. **Leaf key.** Each node (hub and spokes) generates a P-256 key in `<dataDir>/devicecert_leaf.key` (mode 0600). It never leaves the node.
3. **Issuing.** A spoke sends `POST /api/sync/devcert {spki, sans}` with its session token (see `docs/modules/sync.md`). The hub issues a 14-day leaf (`serverAuth` + `clientAuth`), records it in `_device_certs`, audits `devicecert.issue` and answers with the leaf and the root. The hub also issues its own leaf locally.
4. **Names.** The leaf carries `<node_id>.edge.toki.local` (always the authenticated node's own id, never a name from the body), the LAN IPs the node reports (every non-link-local address of its interfaces plus `127.0.0.1`) and the entries of `TOKI_DEVICECERT_SANS`. The hub drops DNS names that are not under `.local`, `.lan`, `.home.arpa` or `.internal`, drops names under `.edge.toki.local` (they belong to nodes), and keeps at most 12 SANs.
5. **Renewal.** A spoke checks every 10 s (a local check) whether less than one third of the leaf's life is left, or an address of the node is missing from the leaf, and then fetches a new one through the sync client session, which runs a handshake first when there is none. A failed renewal is retried after 5 minutes. A node must reach the hub at least once per leaf life (14 days by default); raise `TOKI_DEVICECERT_LEAF_DAYS` (at most 90) for nodes that are offline for long.
6. **Bad clocks.** A leaf is valid from one hour before it was issued, so a device whose RTC is a little behind still accepts it.
7. **Listener.** On `OnServe` the module starts a second `http.Server` on `TOKI_DEVICECERT_LISTEN` (for example `:8443`) with the same handler as the main server (`apis/serve.go` is untouched). `tls.Config.GetCertificate` reads the current leaf on every handshake, so a renewed leaf is served without restarting. Until a spoke has its first leaf, handshakes fail with "no edge certificate yet".
8. **tlscheck.** With `TOKI_DEVICECERT=on` and `TOKI_DEVICECERT_LISTEN` set, `modules/tlscheck` logs an info line instead of the plain-HTTP warning (and does not refuse in `strict` mode). The plain port stays open: bind it to loopback if only the kiosk on the same host needs it.

## Env

| Variable | Meaning |
| --- | --- |
| `TOKI_DEVICECERT` | `on` enables the module (default off) |
| `TOKI_DEVICECERT_LISTEN` | TLS listen address, for example `:8443`. Empty = no listener |
| `TOKI_DEVICECERT_LEAF_DAYS` | lifetime of an edge leaf, default 14, at most 90 |
| `TOKI_DEVICECERT_SANS` | extra comma-separated names and IPs for the leaf |
| `TOKI_DEVICECERT_MTLS` | `off` (default), `optional` or `require`. `optional` = `VerifyClientCertIfGiven`, `require` = `RequireAndVerifyClientCert`, both against the CA pool; revoked serials (the deny list) are refused. A verified client certificate grants nothing yet: the route allowlist and client certificate issuing arrive in PR 7 |

## Collections and files

- `_device_certs` (system collection, rules `null`): `serial` (hex, unique), `name`, `kind` (`client`, `server`), `node`, `not_after`, `revoked_at`, `route_scope`. Config only; not synced in this PR.
- `_devicecert_state` (plain table in `data.db`): the wrapped CA key and the root PEM.
- In the data dir of a node: `devicecert_leaf.key` (0600), `devicecert_leaf.pem`, `devicecert_ca.pem` (the root the hub sent).

## CLI

```
toki devicecert ca [--fingerprint]   # root PEM on stdout, fingerprint on stderr
toki devicecert status [--json]      # leaf serial, not_before, not_after, names; listener settings
toki devicecert list [--json]        # rows of _device_certs
```

`ca` creates the CA on the hub when it does not exist; on a node it prints the root that came with the leaf. `issue`, `revoke` and `rotate-ca` follow in PR 7. Health: the superuser `GET /api/health` has a `devicecert` block (`role`, `listen`, `listening`, `leaf.not_after`, `renew_due`, `ca_fingerprint`, `last_error`).

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

## Limits

- No CA rotation, intermediate CA for offline issuing, or revocation of a node leaf when its node is revoked yet (a revoked node cannot renew; the leaf lapses within its lifetime). PR 7.
- A node may ask for any IP address as a SAN (the hub only drops unspecified, multicast and link-local ones), so an enrolled node can obtain a certificate for a LAN address that is not its own. Enrolled nodes are trusted; do not install the root on devices that must distrust a node.
- Without a hub in the process the module can only be used to print a root that was received earlier.
