# Edge gate walkthrough (parking gate / POS on a Pi or mini PC)

An operator guide for one gate: a Raspberry Pi 4/5 (or a mini PC) that runs the `edge` profile, a Chromium kiosk in front of the driver, a thermal ticket printer, a barcode scanner, and a gate controller on the LAN. The gate keeps working when the uplink or the hub is down and converges when it comes back. Reference pages: [printer](modules/printer.md), [scanner](modules/scanner.md), [kiosk](modules/kiosk.md), [devicecert](modules/devicecert.md), [sync](modules/sync.md); sizes and tags: [PROFILES.md](PROFILES.md); plan: [EDGE_MODULES_PLAN.md](EDGE_MODULES_PLAN.md).

The whole path (hub, enrollment, kiosk, scan, print, client certificate, revoke, offline tickets, lock) is exercised by `tests/e2e/edge-gate.sh` (CI job `e2e-edge-gate`) with a TCP stub printer and a pty instead of a scanner. Everything below that touches real hardware (udev rules, USB printer, Chromium, a real RTC) is documented from the module docs and is not run in CI.

```
 hub (VPS, solo build, TOKI_SYNC_ROLE=hub, devicecert on)
   |  sync over HTTPS (spoke -> hub only)
 gate-1 (Pi, edge build, TOKI_SYNC_ROLE=spoke)
   |- 127.0.0.1:8090   plain HTTP: Chromium kiosk, pairing, the app (--publicDir /opt/app)
   |- 0.0.0.0:8443     TLS (hub CA leaf), mTLS for LAN peers: gate controller, handhelds
   |- /dev/input/by-id/...-event-kbd or serial   barcode scanner
   '- /dev/usb/lp0 or 192.168.x.y:9100          ESC/POS printer
```

## 0. Decisions before you start

| Question | Default in this guide | Why |
| --- | --- | --- |
| Binary | `make edge GOOS=linux GOARCH=arm64` (25.6 MiB) or `GOARCH=amd64` (27.4 MiB) | no admin UI, no JS hooks, no WASM; see PROFILES.md |
| Hub binary | `make solo` or any profile with sync and devicecert | the CA and the issue/revoke CLI live on the hub |
| App | static files in `/opt/app` served with `--publicDir` | the edge profile has no JS runtime; all logic is in the page and the REST API |
| Who writes tickets | see "Who writes" (section 7) | sync has to know the actor |
| Plain port | loopback only | the kiosk is on the same machine; LAN peers use `:8443` |

The edge build refuses to start on a data dir that was used by a build with more modules (stubbed-module guard in PROFILES.md): use a fresh `/var/lib/toki`.

## 1. OS preparation

Raspberry Pi OS Lite 64-bit (Bookworm) or Debian 12 on a mini PC. As root:

```sh
apt update
apt install -y chromium libnss3-tools chrony usbutils unzip curl
# a desktop session or a kiosk compositor for the browser (one of):
apt install -y labwc seatd            # Wayland kiosk session
# apt install -y cage                 # alternative: single-app compositor

useradd --system --home /var/lib/toki --shell /usr/sbin/nologin toki
usermod -aG input,dialout,lp toki      # scanner (evdev), serial scanner/printer, /dev/usb/lp*
useradd --create-home --shell /bin/bash kiosk   # runs Chromium; own NSS database and profile
usermod -aG video,render,input kiosk           # GPU/input access for the compositor (details depend on your image)
install -d -o toki -g toki -m 0750 /var/lib/toki
install -d -m 0755 /opt/toki /opt/app
install -d -m 0750 -g toki /etc/toki
```

Clock: sync, TLS and tickets all use time. Use NTP (`chrony` above) and, on a Pi, an RTC module or accept that the first minutes after boot offline have a wrong clock. A leaf certificate is valid from one hour before issue, and the sync client corrects its clock from the hub when it can (docs/modules/sync.md, "Clock drift"), but a gate that boots offline with a clock off by days stamps its first tickets wrongly.

Install the binary and the app:

```sh
install -m 0755 toki-edge /opt/toki/toki          # keep the previous one as /opt/toki/toki.prev (section 11)
rsync -a app/ /opt/app/                           # your static app; see "App page" below
```

### udev rules

Stable names matter: `/dev/input/eventN` and `/dev/ttyUSBn` change between boots. Find the ids with `udevadm info -a -n /dev/input/event3` and `ls -l /dev/serial/by-id /dev/input/by-id`.

```
# /etc/udev/rules.d/90-toki-gate.rules

# HID barcode scanner read through evdev (docs/modules/scanner.md); change idVendor/idProduct
SUBSYSTEM=="input", ATTRS{idVendor}=="05e0", ATTRS{idProduct}=="1200", MODE="0660", GROUP="input", SYMLINK+="input/barcode0"

# USB receipt printer with the usblp driver: /dev/usb/lp0, writable by group lp
SUBSYSTEM=="usbmisc", KERNEL=="lp[0-9]*", MODE="0660", GROUP="lp", SYMLINK+="usb/receipt"

# serial scanner or serial printer on a USB-serial adapter (serial number from udevadm info)
SUBSYSTEM=="tty", ATTRS{idVendor}=="0403", ATTRS{idProduct}=="6001", ATTRS{serial}=="A50285BI", MODE="0660", GROUP="dialout", SYMLINK+="ttyUSB-receipt"
```

```sh
udevadm control --reload && udevadm trigger
toki scan devices            # lists /dev/input/by-id and /dev/serial/by-id
```

Constraints that come from the modules, not from udev:

- A scanner `device` is a clean path under `/dev/`: `/dev/input/...` for `evdev`; `/dev/tty*`, `/dev/rfcomm*`, `/dev/pts/*` or `/dev/serial/...` for `serial`. Prefer the `by-id` links.
- A printer `address` with transport `file` or `serial` must start with `/dev/usb/`, `/dev/lp`, `/dev/ttyUSB`, `/dev/ttyACM`, `/dev/ttyS` or `/dev/ttyAMA` (the check is on the path string). `/dev/serial/by-id/...` is NOT accepted for a printer, which is why the rule above creates `usb/receipt` and `ttyUSB-receipt`. A TCP printer (`192.168.1.50:9100`) must be inside `TOKI_PRINT_ALLOW_CIDRS` (RFC 1918 and loopback by default).
- With `grab` on an evdev scanner takes the device exclusively, so the digits do not also type into Chromium. A USB keyboard-wedge scanner that you do NOT grab types into the focused page; the kiosk can use the browser wedge (`/scan/wedge.js`) instead, see section 6.

## 2. The server unit

`/etc/toki/edge.env` (mode 0640, group `toki`):

```sh
# --- sync: this box is a spoke ---
TOKI_SYNC_ROLE=spoke
# TOKI_SYNC_INSECURE=1            # only for an http hub URL on a private network (tests); production uses https
# TOKI_SYNC_HUB_PIN=<sha256 of the hub TLS SPKI>   # optional extra pin
TOKI_SYNC_INTERVAL=30s            # idle interval; local writes and hub pokes sync sooner

# --- edge modules (all opt in) ---
TOKI_PRINTER=on
TOKI_SCANNER=on
TOKI_KIOSK=on
TOKI_DEVICECERT=on

# --- who may use the print and scan APIs (the kiosk actor: an auth collection of your app) ---
TOKI_PRINT_ALLOW_COLLECTIONS=gate_devices
TOKI_SCAN_READ_AUTH=gate_devices
TOKI_SCAN_POST_COLLECTIONS=gate_devices

# --- TLS listener for LAN peers (leaf from the hub CA) and client certificates ---
TOKI_DEVICECERT_LISTEN=:8443
TOKI_DEVICECERT_MTLS=optional      # "require" locks out browsers without a client certificate; the kiosk uses the plain loopback port
TOKI_DEVICECERT_LEAF_DAYS=30       # default 14, at most 90: how long the gate may be cut off from the hub before the TLS leaf lapses
TOKI_DEVICECERT_SANS=gate-1.lan    # extra names/IPs for the leaf (*.lan, *.local, *.home.arpa, *.internal)

# --- resources (a 1-2 GB Pi) ---
GOMEMLIMIT=256MiB
TOKI_DB_MAX_CONNS=8
TOKI_DB_CACHE_KB=4096
TOKI_LOGS_MAX_MB=64
TOKI_LOGS_SAMPLE_OK=10
TOKI_WAL_MAX_MB=64
TOKI_SCAN_RETENTION_HOURS=72
TOKI_PRINT_RETENTION_DAYS=7
```

`/etc/systemd/system/toki-gate.service`:

```ini
[Unit]
Description=TokiBase edge gate
After=network-online.target time-sync.target
Wants=network-online.target

[Service]
User=toki
Group=toki
SupplementaryGroups=input dialout lp
EnvironmentFile=/etc/toki/edge.env
WorkingDirectory=/var/lib/toki
ExecStart=/opt/toki/toki serve --dir /var/lib/toki --http 127.0.0.1:8090 --publicDir /opt/app
Restart=always
RestartSec=3
KillSignal=SIGTERM
TimeoutStopSec=30
LimitNOFILE=8192
MemoryMax=512M
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/toki
PrivateTmp=true
# do NOT set PrivateDevices=true: the process opens /dev/input, /dev/ttyUSB* and /dev/usb/lp*

[Install]
WantedBy=multi-user.target
```

Do not start it with `TOKI_SYNC_ROLE=spoke` yet: the app collection and the local rows come first (section 4), then the enrollment (section 5). The firewall should let only the gate controller subnet reach `:8443` (for example `ufw allow from 192.168.10.0/24 to any port 8443 proto tcp`); the plain port is on loopback and needs no rule. `TOKI_TLS_CHECK=strict` is not usable while the plain port exists on a non-loopback address; with `--http 127.0.0.1:8090` the check stays quiet.

The edge data dir (`/var/lib/toki`) holds `data.db`, `auxiliary.db`, the node key `sync_node.key` (0600), the TLS leaf key `devicecert_leaf.key` (0600) and the bundle files `devicecert_*.pem`. All CLI commands below run as the service user with the same environment so they see the same modules:

```sh
alias tokigate='sudo -u toki env $(grep -v "^#" /etc/toki/edge.env | xargs) /opt/toki/toki'
tokigate sync status --dir /var/lib/toki
```

## 3. The hub (once)

On the hub (a `solo` or `team` build, a real hostname with HTTPS in front):

```sh
export TOKI_SYNC_ROLE=hub TOKI_DEVICECERT=on TOKI_SYNC_HUB_KEY_FILE=/etc/toki/hub.key   # key outside data.db: it also wraps the CA key
toki serve --dir /var/lib/toki-hub --http 127.0.0.1:8090            # behind your reverse proxy

# the business collection, with offline ticket numbers
toki sync reserve create-seq tickets --block 1000 --max-open 2 --dir /var/lib/toki-hub
# (create the `tickets` collection through the Admin UI/API with a `no` text field and a unique index on it)
toki sync policies set tickets --direction both --field-type no=reserve:tickets --dir /var/lib/toki-hub

# the deny list for client certificates: BEFORE any gate enrolls (nodes read policies at their handshake)
toki sync policies set _device_certs --direction pull --dir /var/lib/toki-hub

# print the root once and note the fingerprint (you compare it on the gate)
toki devicecert ca --dir /var/lib/toki-hub > toki-root.pem        # fingerprint on stderr
```

`--block 1000` is how many ticket numbers a gate holds locally. Size it as (tickets per day) times (days of outage you want to ride out); a gate that runs dry creates nothing (503 `sync_reservation_exhausted`) rather than a duplicate. A gate may hold `--max-open` ranges at once and tops up when 20 % is left.

Keep `TOKI_SYNC_HUB_KEY_FILE` out of backups of `data.db`; without it a copy of the hub database can mint device certificates (docs/modules/devicecert.md).

## 4. App collections and local rows on the gate (before enrollment)

An enrolled spoke refuses to create or change non-system collections (the hub ships the schema, docs/modules/sync.md "Spoke schema lock"). Everything the hub does not ship must therefore exist before `sync join`: here the auth collection of the kiosk actor. The local hardware rows of section 6 are system collections and can be created at any time, but doing them in the same pass is simplest. Order:

1. Set `TOKI_SYNC_ROLE=off` in `/etc/toki/edge.env` for now and create the operator: `tokigate superuser upsert ops@example.com 'a-long-password' --dir /var/lib/toki`.
2. `systemctl start toki-gate`, then create the actor collection and record:

```sh
B=http://127.0.0.1:8090
T=$(curl -s $B/api/collections/_superusers/auth-with-password -H 'Content-Type: application/json' \
      -d '{"identity":"ops@example.com","password":"a-long-password"}' | jq -r .token)
H=(-H "Authorization: $T" -H 'Content-Type: application/json')

# the service actor of the kiosk: an auth record that may only read itself
curl -s "${H[@]}" $B/api/collections -d '{"name":"gate_devices","type":"auth","viewRule":"@request.auth.id = id"}'
curl -s "${H[@]}" $B/api/collections/gate_devices/records \
  -d '{"email":"gate1@example.com","password":"<random>","passwordConfirm":"<random>"}'    # note the id
```

3. Do section 6 (printer, template, scanner rows, kiosk provisioning), `systemctl stop toki-gate`, set `TOKI_SYNC_ROLE=spoke`, continue with section 5.

The actor must be a low-privilege auth record: never a superuser, never a system collection (the kiosk refuses both). The `ops` superuser is the operator of the box; sync captures its writes as the `node` actor (section 7).

## 5. Enrollment

On the hub, one code per gate (24 h, shown once):

```sh
toki sync enroll --name gate-1 --profile edge --param branch=B12 --actor gate_service/<hub record id> --dir /var/lib/toki-hub
# prints: code: ABCD-EFGH-...
```

- `--param branch=B12` feeds a partition policy such as `branch = @node.branch`: the gate only receives and may only push records of its branch.
- `--actor` is the service actor of the node: everything the node writes without a user grant is replayed on the hub with this record's rights. Create a dedicated low-privilege auth record on the HUB for it (here `gate_service`) and give it exactly the create/update rules the gate needs. `--actor _superusers/<id> --allow-superuser-actor` works (the e2e uses it) but bypasses every rule for what the gate pushes; do not do that in production.

On the gate, role `spoke`:

```sh
tokigate sync join https://hub.example.com ABCD-EFGH-... --dir /var/lib/toki
sudo systemctl enable --now toki-gate
tokigate sync status --dir /var/lib/toki
```

`join` stores the node certificate (365 days, renewed by the handshake) in `_sync_cursors`. The next handshake brings the policies, the `tickets` schema bundle and a first number range. Within about 10 s of the first session the gate fetches its TLS leaf from the hub (`devicecert_leaf.pem`).

Check on the hub: `toki sync peers` (the gate is `active`), `toki devicecert list` (a `server` row named after the node id).

## 6. Local hardware rows, kiosk, PIN

`_printers`, `_print_templates`, `_scanners` and `_kiosk_devices` are system collections. Sync currently admits exactly one system collection, `_device_certs` (explicit allowlist in `modules/sync/syscollections.go`; `eligible()` in `policy.go`). So a pull-only policy for `_printers`/`_print_templates`/`_scanners` is NOT possible yet, and a fleet must be provisioned by running the same script on every box (idempotent: GET the name first, PATCH when it exists). The policy to use once sync admits them is `direction=pull` for `_printers`, `_print_templates` and `_scanners`; never a policy for `_print_jobs`, `_scan_events` or `_kiosk_devices` (device state, local by design).

The snippets assume `B` and `H` from section 4. Printer (TCP 9100 here; for USB use `"transport":"file","address":"/dev/usb/receipt"`):

```sh
curl -s "${H[@]}" $B/api/collections/_printers/records -d '{
  "name":"counter","transport":"tcp","address":"192.168.1.50:9100","enabled":true,"default":true,
  "qr_native":true,"cut":true,"cols":32,"codepage":"cp437","timeout_ms":3000,"status_timeout_ms":1000}'

curl -s "${H[@]}" $B/api/collections/_print_templates/records -d '{
  "name":"ticket",
  "body":"@center\n@size 2 2\nPARKIR\n@size 1 1\n@left\nNo:   {{.no}}\nPlat: {{.plate}}\nMasuk: {{date .in \"02 Jan 15:04\"}}\n@qr {{.qr}}\n@feed 3\n@cut"}'

tokigate print test counter --qr --dir /var/lib/toki      # direct test page + DLE EOT status
tokigate print status counter --dir /var/lib/toki
```

Scanner (evdev here; serial: `"kind":"serial","device":"/dev/ttyUSB-scan","baud":9600`; browser wedge: `"kind":"web"`):

```sh
curl -s "${H[@]}" $B/api/collections/_scanners/records -d '{
  "name":"belt","kind":"evdev","device":"/dev/input/by-id/usb-Symbol_Technologies_Inc-event-kbd",
  "grab":true,"enabled":true,"charset":"^[A-Z0-9-]+$","min_len":4,"dedupe_ms":1500}'
tokigate scan listen belt --dir /var/lib/toki     # stop the server's reader first; Ctrl-C to leave
```

Kiosk device and PIN:

```sh
tokigate kiosk provision --name gate-1 --actor gate_devices/<actor id> --pin 1234 --lock-after 120 \
  --bind-ip 127.0.0.1 --expires 15m --url http://127.0.0.1:8090 --dir /var/lib/toki
# prints http://127.0.0.1:8090/kiosk/pair#<one-time code>
tokigate kiosk set-pin gate-1 --pin 4821 --dir /var/lib/toki     # change later; --clear removes the lock
```

Open the pairing URL once in the kiosk browser (section 8). The code is single use, expires, and travels in the URL fragment, so it never reaches a log; the PIN only keeps a walk-up customer out (it is not a security boundary against a shell).

### App page

```html
<!doctype html>
<script src="/kiosk/kiosk.js"></script>   <!-- pairing, session, offline pill, PIN lock -->
<script>
  // window.toki.token is the actor token; new PocketBase() picks it up from localStorage.
  document.addEventListener('toki:scan', e => console.log('scan', e.detail));
  async function printTicket(no, plate) {
    return fetch('/api/print', { method: 'POST',
      headers: { 'Content-Type': 'application/json', Authorization: window.toki.token },
      body: JSON.stringify({ template: 'ticket', data: { no, plate, qr: 'GATE-' + no }, idempotency_key: 'ticket-' + no }) });
  }
</script>
```

Printing is a durable queue job: an `idempotency_key` per ticket stops double enqueue; a paper-out printer parks the job as `waiting_paper` and resumes when the roll is changed. Print jobs and scan events are local to the box and never synced.

## 7. Offline behaviour and who writes

- **What keeps working with the hub down:** the kiosk session (a local token, refreshed against the box), scanning, printing, local reads and writes, reserved ticket numbers, client-certificate access from the LAN (the deny list is the local copy of `_device_certs`).
- **Reservations.** A create on the gate fills an empty `no` from a range the hub issued (`field_types no=reserve:tickets`), in the same transaction as the record. Ranges are used lowest first; at 20 % left the sync loop fetches another. When all are used the create fails closed with HTTP 503 `data.code=sync_reservation_exhausted`. The hub rejects a pushed number outside the ranges of the node (`reservation_out_of_range`).
- **Pending changes.** Writes queue in `_changes`. The kiosk pill shows green "Online", amber "Hub offline - N pending" (from `GET /api/kiosk/status`, `pending_changes`), or red "Edge offline" when the box itself does not answer. `tokigate sync status` shows `pending`, `last error`, `state`.
- **Coming back.** The loop backs off (1 s doubling to 5 min, +-20 %), handshakes, pushes in order, pulls, and acks. `tokigate sync verify` compares record counts and digests with what the hub has (`--against-hub` for the metadata digest); a converged gate prints no mismatches and `pending: 0`.
- **Limits of being offline.** The TLS leaf needs a hub contact once per `TOKI_DEVICECERT_LEAF_DAYS` (14 default, 90 max); a node certificate lasts 365 days; a gate offline longer than the hub's retention (`TOKI_SYNC_RETENTION`, 90 d) is marked `stale` and bootstraps from a snapshot when it returns (`tokigate sync rebootstrap`). A revoked certificate stays valid on a gate that never got the new deny list; rely on short certificate lifetimes for LAN peers that must be cut off while the gate is offline.
- **Who writes (important).** A write is replayed on the hub as "the user who made it". The kiosk session token belongs to the local `gate_devices` record, which the hub does not know: sync captures such a write as `rec:<collection>:<id>` and the hub PARKS it (`_changes.status=parked`, conflict kind `actor_revoked`/`actor_unknown`) until someone decides, so the ticket exists only on the gate meanwhile. Pick one of:
  1. Write from the operator side of the box (a superuser token held by the app backend on the gate, or Go code in the process): captured as the `node` actor and replayed as the service actor of `--actor`. This is what `tests/e2e/edge-gate.sh` does for the ticket rows.
  2. Give the kiosk actor a sync grant: the actor record exists on the hub, logs in there, and the app calls `AddActor` (Go: `Module.AddActor`; `docs/modules/sync.md`, "Actor grants"). Then offline writes carry the grant and the hub applies them with that user's rights. A device offline longer than `TOKI_SYNC_GRACE` (24 h) after the grant expires needs a new one.
  3. Accept parking for audit-style data and review with `toki sync conflicts --open` on the hub.

  Mapping the kiosk actor automatically is a follow-up (the parking scenario of SYNC_DESIGN PR10 covers the parked path).

## 8. Chromium kiosk and the root CA

The browser needs the hub root once if it will ever open `https://<node>:8443`; the kiosk itself uses the plain loopback port and needs nothing, but staff tablets and the gate controller do.

Install the root for the kiosk user (Chromium reads the per-user NSS database). Compare the fingerprint with the one the hub printed:

```sh
sudo -u kiosk -H bash -c '
  mkdir -p $HOME/.pki/nssdb
  [ -f $HOME/.pki/nssdb/cert9.db ] || certutil -d sql:$HOME/.pki/nssdb -N --empty-password
  certutil -d sql:$HOME/.pki/nssdb -A -t "C,," -n "Tokibase Edge CA" -i /root/toki-root.pem
  certutil -d sql:$HOME/.pki/nssdb -L'
openssl x509 -in toki-root.pem -noout -fingerprint -sha256     # also: tokigate devicecert ca --fingerprint
```

System wide (curl, Go, Python on the box): `cp toki-root.pem /usr/local/share/ca-certificates/toki-root.crt && update-ca-certificates`.

`/etc/systemd/system/kiosk-browser.service` (a system unit that runs Chromium for user `kiosk` inside `cage`; adjust to your session manager; this unit shape is the common cage pattern and is not exercised in CI):

```ini
[Unit]
Description=Chromium kiosk
After=toki-gate.service systemd-user-sessions.service
Wants=toki-gate.service

[Service]
User=kiosk
PAMName=login
TTYPath=/dev/tty1
StandardInput=tty
StandardOutput=journal
Environment=XDG_RUNTIME_DIR=/run/user/%U
ExecStartPre=+/usr/bin/install -d -o kiosk -g kiosk -m 0700 /var/lib/kiosk/chromium
ExecStart=/usr/bin/cage -s -- /usr/bin/chromium --kiosk --noerrdialogs --disable-infobars \
  --disable-session-crashed-bubble --no-first-run --disable-translate --disable-pinch \
  --overscroll-history-navigation=0 --check-for-update-interval=31536000 \
  --user-data-dir=/var/lib/kiosk/chromium http://127.0.0.1:8090/
Restart=always
RestartSec=3

[Install]
WantedBy=graphical.target
```

On a normal desktop session use the user unit from [kiosk.md](modules/kiosk.md#chromium-kiosk-unit-ops) instead. Rules that matter in both:

- `--user-data-dir` on a persistent path: the pairing cookie lives in that profile. Never use `--incognito` or a tmpfs profile, or the gate must be re-paired at every boot.
- Pair from the box itself (the pairing route accepts loopback peers only). Once: `sudo -u kiosk chromium --user-data-dir=/var/lib/kiosk/chromium --app='http://127.0.0.1:8090/kiosk/pair#<code>'`, wait for the page to say paired, close it, start the unit. The code is visible in the process list for `--expires` (15 min default); use `--expires 2m`.
- Re-pair after `toki kiosk rotate gate-1` (prints a new URL) and after `revoke`.
- The device cookie is SameSite=Strict but SameSite is per site, not per origin: serve only trusted apps on this host.

## 9. Gate controller with a client certificate

A PLC/handheld/controller on the LAN calls the scan or print API on `:8443` without any token, authenticated by a certificate the hub issued. On the hub:

```sh
toki devicecert issue --name gate-ctrl-1 --days 90 --scope /api/scan,/api/print --out ./gate-ctrl-1 --dir /var/lib/toki-hub
# writes gate-ctrl-1.key.pem (0600), gate-ctrl-1.crt.pem, ca.pem; add --p12 for a .p12 (needs the openssl binary)
```

Copy the three files to the controller over a trusted channel. The call:

```sh
curl --cacert ca.pem --cert gate-ctrl-1.crt.pem --key gate-ctrl-1.key.pem \
  -H 'Content-Type: application/json' -d '{"scanner":"door","code":"GATE-CTRL-IN"}' \
  https://gate-1.lan:8443/api/scan
```

Facts to rely on (docs/modules/devicecert.md has the rest):

- The scope is a list of path prefixes with at least two segments; it can never cover `/api/collections`, `/api/sync`, `/api/batch`, `/api/settings`, `/api/backups`, `/api/files`, `/api/realtime`, `/api/logs`, `/api/mcp`, `/api/device`, `/api/health` or `/_`. A certificate without a scope identifies the controller and grants nothing.
- Scanner, printer and `GET /api/kiosk/status` serve a trusted device; its actor in the scan/print records is `device:<name>`. It is not a user and not the kiosk actor.
- The gate learns the certificate row (the scope) with the next pull of `_device_certs`; the first call can fail for a few seconds after `issue`.
- Revoke: `toki devicecert revoke gate-ctrl-1` (or by serial). The deny list reaches the gate by the pull-only policy (section 3) within the sync interval, and the TLS handshake refuses the certificate, resumed sessions included. If the gate cannot read the list for 5 minutes, it refuses every client certificate (fail closed).
- Rotating the CA: `toki devicecert rotate-ca --overlap-days 30`, reinstall the root on browsers and controllers, reissue client certificates in the overlap; details in devicecert.md.

## 10. Operating and troubleshooting

Status surfaces:

| What | Command / URL (on the gate) |
| --- | --- |
| Sync | `tokigate sync status [--json]` (role, pending, last handshake, last error, state); `tokigate sync verify [--against-hub]` |
| Hub view | `toki sync peers` (lag, last seen, schema, clock offset), `toki sync conflicts --open` |
| Printer | `tokigate print printers`, `print status counter`, `print jobs --state failed`, `print retry <id>`; `GET /api/print/printers` |
| Scanner | `tokigate scan list`, `scan devices`, `scan listen <name>`; `GET /api/scan/scanners`, `GET /api/scan/events?scanner=belt` |
| Kiosk | `tokigate kiosk list`; `GET /api/kiosk/status` (cookie): edge, hub reachable, pending changes, printers |
| Certificates | `tokigate devicecert status`; `GET /api/device/identity`; hub: `toki devicecert list` |
| Everything | `GET /api/health` with a superuser token: blocks `sync`, `printer`, `scanner`, `devicecert` (leaf `not_after`, `renew_due`, `listening`, `deny_list`, `last_error`) |
| Logs | `journalctl -u toki-gate -f` |

| Symptom | Look at |
| --- | --- |
| Pill amber "Hub offline - N pending" | `tokigate sync status`: `last error`; hub reachable from the box (`curl -I https://hub...`); clock (`timedatectl`); hub `toki sync peers` shows `revoked`? A revoked node (`403 sync_node_revoked`) stops its loop |
| `sync_clock_drift` 409 | the box clock is off by more than `TOKI_SYNC_MAX_DRIFT` (5 min): fix NTP; the handshake corrects the HLC and re-stamps pending changes |
| Ticket create returns 503 `sync_reservation_exhausted` | the range is used up and the hub was unreachable: reconnect, or raise `--block` / `--max-open` on the hub |
| A kiosk write is missing on the hub | parked (section 7): `toki sync conflicts --open` on the hub |
| Scanner reads nothing | `health.scanner[].state` (`error`, `last_error`); group membership (`id toki`, restart the unit after `usermod`); `tokigate scan devices`; a second process holding the device (`scan listen` while the server runs) |
| Digits appear in the page and the scanner also posts | `grab` is off for an evdev scanner |
| Print job `waiting_paper` | roll or cover; it resumes by itself (polls every 10 s up to 1 h, then `dead`); `print retry <id>` after `dead` |
| Print `done_unconfirmed` | everything was written and the printer reported a fault afterwards; the ticket may be missing: check, then `print retry <id>` on purpose |
| Printer `failed` / `offline` | `print status counter`; address inside `TOKI_PRINT_ALLOW_CIDRS`; USB: `ls -l /dev/usb`, group `lp` |
| Browser shows a certificate error on :8443 | root not installed for that user (NSS), or the name is not in the leaf (`tokigate devicecert status`; add `TOKI_DEVICECERT_SANS`), or no leaf yet ("no edge certificate yet": the first hub contact has not happened) |
| Client certificate refused | `toki devicecert list` on the hub: revoked or expired; the deny list policy missing at enrollment time; `TOKI_DEVICECERT_MTLS` still `off` |
| Kiosk says "Not paired" | cookie lost (profile path changed): `tokigate kiosk rotate gate-1` and pair again |
| Kiosk locked after a restart | by design: `locked` is stored; unlock with the PIN. 5 wrong PINs lock attempts for 60 s, doubling to 1 h |

## 11. Upgrade, rollback and backup

Upgrade (the schema, the keys and the hub contact are not touched by a binary swap):

1. Upgrade the hub first, then the gates, one gate as a canary. Read `docs/COMPAT.md` for entries that mention spokes or sync before upgrading a fleet.
2. On the gate: take a backup (below), and copy `sync_node.key` off the box if you have not already.
3. `install -m 0755 /opt/toki/toki /opt/toki/toki.prev && install -m 0755 toki-edge-new /opt/toki/toki`.
4. `systemctl restart toki-gate`; then `tokigate sync status`, `curl -s http://127.0.0.1:8090/api/health`. A schema lock applies: if the new release adds a LOCAL collection by migration on an enrolled spoke, start once with `TOKI_SYNC_SCHEMA_LOCK=off` (docs/COMPAT.md, sync PR8).
5. Wait for one sync cycle and confirm `pending: 0`, the pill is green, a test print works (`print test counter`).

Rollback: `systemctl stop toki-gate; install -m 0755 /opt/toki/toki.prev /opt/toki/toki; systemctl start toki-gate`. If the new release migrated the database and the old binary refuses to start, restore the pre-upgrade backup instead; that discards everything written on the gate since the backup that had not been pushed yet, so roll back early.

Backup of the edge data dir:

- While serving: `curl -s -X POST "http://127.0.0.1:8090/api/backups?async=true" -H "Authorization: $T" -H 'Content-Type: application/json' -d '{"name":"nightly.zip"}'` (superuser token; `GET /api/backups/status` shows progress), or `tokigate backup create nightly --dir /var/lib/toki` with the unit stopped. The zip lands in `/var/lib/toki/backups`; copy it off the box and prune old ones (the SD card is small). Check what the zip holds: `unzip -l /var/lib/toki/backups/<file>.zip | grep -E 'sync_node.key|devicecert_'`; if the key files are not in it, copy them yourself.
- What matters most: `sync_node.key` (the node identity; losing it means a new node id and a re-enrollment), `data.db`. The TLS leaf and bundle files are re-fetched from the hub when missing, but delete all three of `devicecert_bundle.pem`, `devicecert_ca.pem`, `devicecert_leaf.pem` together if you must. Treat the backup as secret: it holds the node key.
- A gate is mostly a cache of the hub plus whatever it created offline. If the SD card dies with unsynced tickets, those tickets are gone: this is the reason to keep `pending` near zero and the uplink monitored. A gate restored from an older backup pushes again from the hub's `push_from`, and its reserved-number cursor moves forward to the hub's high-water mark at the next handshake (no number is issued twice).
- A new Pi replacing a dead one: new node id, so `toki sync enroll --name gate-1b ...` on the hub, `sync join` on the box, `toki sync revoke gate-1` for the old node (this also revokes certificates named after it), re-provision rows (section 6) and pair the kiosk again.

## 12. What is not done yet

- Sync of `_printers`, `_print_templates`, `_scanners`, `_kiosk_devices` (pull-only provisioning): needs `modules/sync/syscollections.go` to admit them.
- Automatic mapping of the kiosk actor (and of a client certificate) to a sync grant / actor with rights: devicecert v1.1 actor mapping.
- An intermediate CA so that an edge offline for weeks can issue LAN client certificates itself: devicecert v1.1.
- Real-device checks of the evdev reader and the udev rules on several scanner models (edge plan PR 5), and a tested image for the Chromium kiosk unit.
