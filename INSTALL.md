# Installing foxess-local

foxess-local gives you local monitoring and control of a FoxESS inverter:
live power, battery, history, work mode, limits and schedule, from a web
UI and an API (Home Assistant), without the FoxESS cloud. The cloud
stays optional: you can keep it fully connected, give it read-only data,
or cut it off.

## How it fits into your network

The inverter talks to the FoxESS cloud over its Wi-Fi/LAN dongle.
foxess-local runs on a small Linux machine that becomes the inverter's
**network gateway**. On that machine:

- the **egress gate** (nftables) lets the inverter reach only what it
  needs, and hands its cloud connection to the app;
- the **app** (`foxess-local`) speaks the inverter's protocol, reads its
  values, sends your commands, and — if you allow it — relays the
  connection on to the real cloud.

Nothing else on your network is affected: every rule matches only the
inverter's address.

## What you need

- A Linux machine with systemd on the same network as the inverter:
  a Raspberry Pi 4/5 (64-bit OS), a Pi 2/3 (32-bit), a mini PC, a VM,
  or an [OpenStick](#on-an-openstick-a-low-cost-dedicated-gateway) USB
  stick.
  Architectures: amd64, arm64, armv7.
- `nftables` with TPROXY support (standard on current Debian, Ubuntu,
  Raspberry Pi OS, Fedora) and `iproute2`. Optional: `conntrack`, which
  lets the 5-minute gate check also look for an inverter connection that
  got past the rules (without it, only that scan is skipped).
- Storage: the raw message log grows about 12 MB a day and is kept for
  `[retention] raw_hours` (default 72 h, so ~40 MB); the 15-minute history
  is small (~1 MB a year). On a device with little flash, keep
  `raw_hours` short: if free space still runs low, the app discards the
  oldest raw data itself (`min_free_mb`, default 200 MB) and says so in
  the web UI.
- The inverter on a fixed IP address, and a way to set its **gateway**
  to this machine (in the dongle's network settings, or with a DHCP
  reservation whose gateway option points here).
- Supported inverters: the KH series is verified (KH10, firmware
  1.73 / 1.03 / 1.71). Other FoxESS models are supported **read-only and
  tentatively** using community register maps; settings stay locked
  until verified. See [other models](#other-inverter-models).

## Install (recommended: native)

1. **Download** from the [latest release](../../releases/latest):
   `installer.sh`, the binary for your machine
   (`foxess-local_linux_arm64` for a 64-bit Pi, `…_amd64` for a PC,
   `…_armv7` for a 32-bit Pi), `SHA256SUMS` and `SHA256SUMS.minisig`.

2. **Verify** the download. Releases are signed offline with the project's
   minisign key (key ID `553C34FE26291351`):

   ```bash
   minisign -Vm SHA256SUMS -P RWRREykm/jQ8VVPcIxLpOEKOVyXC6B4/XP6i9qdkHRbDVlOTlm8vcIb3
   sha256sum -c --ignore-missing SHA256SUMS
   ```

   `minisign` is packaged by most distributions (`apt install minisign`,
   `dnf install minisign`). The key printed here is the one every
   foxess-local binary carries; it should match the one in the release
   notes. If it differs, do not install, and report it.

   This check is what makes a first install trustworthy: do it with
   minisign. A downloaded binary can check its own release
   (`./foxess-local_linux_arm64 verify-release .`), but only against
   the key inside itself, so that catches a damaged download, not a
   forged one. Updates do not have this gap: the installed binary checks
   every new release against the key it already holds.

3. **Install** (replace the address with your inverter's):

   ```bash
   sudo bash installer.sh install --inverter-ip 192.168.1.50
   ```

   The installer checks the requirements — offering to install a missing
   `nftables`, `iproute2` or `conntrack` and to turn on IPv4 forwarding,
   one yes/no each (`--fix` says yes in a script), and printing the fix
   for any firewall in the way — asks **what the FoxESS cloud may receive** —
   nothing, readings only, or everything as before (`--forward-mode`
   answers it in a script; without one it is nothing) — creates a
   `foxess` system user, installs the gate and the app, starts them,
   checks that each part came up, and prints your **installer token
   once**: keep it in a password manager. Run `sudo bash installer.sh
   check` first to see the requirements only; `--no-start` installs
   without starting.

4. **Point the inverter at this machine.** The inverter's Wi-Fi data
   logger (the dongle) needs a fixed address, with this machine as its
   gateway. For the FoxESS WLan / Smart WB data logger (its user manual,
   "Web Configuration"):
   1. From a phone or laptop, join the data logger's own Wi-Fi: its name is
      `W-` followed by the last 7 digits of the data logger's serial
      number, and its password is `mtmt2020` until you change it.
   2. Open `https://192.168.1.1` and log in with the same password. On
      the first login it asks for a new one (8–16 characters, mixing
      letters, digits and symbols), which also becomes the hotspot's
      Wi-Fi password.
   3. Open **WiFi Configuration**. Under **WiFi Settings**, choose your
      router's network (SSID) and enter its password, then **Submit**.
   4. Under **Network Configuration**, set **Type** to **Static IP** and
      enter:
      - **IP:** the fixed address you chose for the inverter (the
        `--inverter-ip` you gave the installer). Pick one outside your
        router's DHCP range, so nothing else is handed it.
      - **Subnet Mask:** your network's, usually `255.255.255.0`.
      - **SGateway:** **this machine's address.**

      Then **Submit**. There is no DNS setting; none is needed (the
      inverter reaches the cloud by IP).
   5. The **Status** page shows "Enter transparent mode" once the network
      is configured. It reconnects within minutes (or power-cycle the
      inverter).

   The same page shows the data logger's **WiFi Mac**. Giving it to the
   installer (`--inverter-mac`, then re-run the install) lets the gate
   also match the dongle by MAC, not only by address.

   Other data loggers have a similar page: set a static address, with
   this machine as the gateway. Or leave the dongle on DHCP and give it
   the fixed address and this machine as gateway from your router (a DHCP
   reservation with its own gateway option), where the router supports
   that.

5. **Open the web UI** at `http://<this machine>:18080` and sign in with
   the token. The **setup** page opens: it checks that the inverter is
   connected and identified and that the clocks agree (each failed check
   says what to fix), and asks for the inverter's time zone, the battery
   capacity, what the cloud may receive, and what visitors without a
   token may see. It is under System → Setup afterwards. The header shows
   `live` once data flows.

   The time zone is where the inverter is; this machine's own zone does
   not matter (an OpenStick can stay on UTC). The inverter's clock has
   no zone of its own: the FoxESS cloud used to set it, and while the
   cloud cannot change anything foxess-local keeps it to this zone,
   correcting drift and daylight-saving changes automatically.

### On an OpenStick (a low-cost dedicated gateway)

A UZ801-class 4G USB stick (Qualcomm MSM8916) flashed with the
OpenStick Debian image makes a cheap, low-power gateway that does
nothing else: plug it into a USB charger within Wi-Fi range.
`provision-openstick.sh` (in every release) prepares it and then runs
the installer. It is idempotent: re-running it changes only what is
missing.

1. Flash the stick with the OpenStick Debian image, join it to your
   Wi-Fi (`sudo nmtui`) and log in over SSH. **Reserve its address on
   your router** (a DHCP reservation): the inverter will use it as its
   gateway, so it must not change.
2. Download `provision-openstick.sh`, `SHA256SUMS` and
   `SHA256SUMS.minisig` from the release, verify them as in step 2
   above, and copy the script to the stick (`scp provision-openstick.sh
   user@<stick>:`).
3. On the stick, see what it would change, then run it with your
   inverter's address:

   ```bash
   sudo bash provision-openstick.sh --check
   sudo bash provision-openstick.sh --inverter-ip 192.168.1.50
   ```

   It pins the package sources to the installed Debian release (the
   image names `stable`, which has since moved on), installs the gate's
   tools, adds compressed swap in RAM (the stick has ~320 MB), caps the
   system log, turns Wi-Fi power saving off and turns on IPv4
   forwarding. Then it downloads this release, checks its signature
   against the release key above and runs the installer.

   **TPROXY:** the stick's kernel has no TPROXY support, which the gate
   needs. The script builds the three modules on the stick, from the
   kernel.org source of the running kernel's version (checksum pinned in
   the script) with the kernel's own configuration, and tests that they
   load. This takes about 5 minutes the first time. UDP transparent
   proxying is left out (the kernel does not export what it needs; the
   gate uses TCP only). If the kernel is ever replaced, run the script
   again: it rebuilds them.
4. Continue with steps 4 (point the inverter at the stick) and 5 above.

The script warns if the image's well-known default password is still
set: change it (`passwd`). Once you log in with an SSH key,
`--ssh-keys-only` turns password logins off. It leaves the stick's modem,
hotspot and USB networking alone. `--hostname NAME` renames the stick;
`--version vX.Y.Z` installs a specific release.

Configuration lives in `/etc/foxess-local/config.toml` — every setting is
optional and documented in the file. The battery capacity, the cloud
forwarding mode and visitor access can also be changed in the UI (System →
Setup, System → FoxESS cloud); a value set there is kept across restarts and wins
over the file.

## Updates

foxess-local checks GitHub for a new release once a week (one HTTPS
request carrying only the version). When one exists, **System →
Updates** offers it. Under **Update choices** an installer can set
how often to check (or never), opt in to **prereleases** (test versions
ahead of a release, which may have bugs), and turn on **installing
updates automatically**: each new update is then applied once, when a
check finds it, and one that fails to start is rolled back and not tried
again. The same three are `[update]` settings in the config file.

Applying an update, by hand or automatically:

- the new binary is downloaded, its **signature** (made offline by the
  release owner) and checksum are verified against the key built into
  your current binary, and it is **self-tested** before anything changes;
- it replaces the old binary atomically (the old one is kept), and the
  app restarts into it — **no root, no SSH**;
- the new version is trusted only once it has the inverter back with
  fresh readings stored (or, if the inverter was offline when you
  upgraded, after 10 minutes of running); if it fails to start, or does
  not get the inverter back within 15 minutes, three times over, the
  previous one is **restored automatically** and the UI says so. A start
  that ends only because the inverter's port was still held by the
  previous session is not counted (at most three times).

From a shell instead: `sudo -u foxess /var/lib/foxess-local/bin/foxess-local upgrade`
(`-check` to only look), then restart the service.

Before downloading, the app checks there is room for the new binary and
still `[retention] min_free_mb` (default 200 MB) free afterwards.

Updates of the gate scripts or units (rare) come with a new
`installer.sh`: re-run it — it keeps your config and token. Use the
files of the release you are on (or newer): the installer refuses to put
back an older version than the one installed (`--allow-downgrade` if you
mean it).

## Container (alternative)

The app can run as a container; the gate stays on the host:

```bash
sudo bash installer.sh install --gate-only --inverter-ip 192.168.1.50
```

(it also seeds `/etc/foxess-local/config.toml`, which the container
reads), then use [`deploy/container/compose.yaml`](deploy/container/compose.yaml)
(host networking; the container keeps only `CAP_NET_ADMIN`, read-only
root filesystem, no new privileges). Set `FOXESS_IMAGE` to the image
pinned by its digest from the release notes, and create the first token
as shown in the file. To update, set the new digest and `docker compose
up -d` — the container does not update itself. (This route has not yet
been exercised end to end.)

## HTTPS (optional, through a reverse proxy)

foxess-local serves plain HTTP and is meant for your home network. If you
want HTTPS — or reach it from outside, which is not recommended — put a
reverse proxy in front; the app has no TLS listener of its own. With
nginx:

```nginx
server {
    listen 443 ssl;
    server_name foxess.example.lan;
    ssl_certificate     /etc/ssl/foxess.crt;
    ssl_certificate_key /etc/ssl/foxess.key;

    location / {
        proxy_pass http://127.0.0.1:18080;
        proxy_set_header Host $host;
        proxy_http_version 1.1;       # the live updates (server-sent events)
        proxy_read_timeout 1h;        # keep the event stream open
    }
}
```

- The app answers only to its own names (a protection against DNS
  rebinding): IP addresses, `localhost`, this machine's host name and
  names ending in `.local`, `.lan`, `.home.arpa` or `.internal` work
  as they are; add any other name the proxy uses to `[net] host_names`
  in `/etc/foxess-local/config.toml`.
- The event stream tells nginx not to buffer it, so no
  `proxy_buffering` setting is needed.
- Then bind the app to the loopback address only (`[server] http_listen
  = "127.0.0.1:18080"`), so the plain-HTTP port is not reachable from
  the network.

**Under a path** (for example `https://home.example.lan/foxess/` beside
other services): strip the prefix in the proxy. Every URL the web UI
uses is relative to the page, so it needs no setting of its own.

```nginx
location = /foxess { return 301 /foxess/; }   # the trailing slash matters
location /foxess/ {
    proxy_pass http://127.0.0.1:18080/;       # the trailing slash strips /foxess
    proxy_set_header Host $host;
    proxy_http_version 1.1;
    proxy_read_timeout 1h;
}
```

The API is then at `/foxess/api/v1`, which is the base URL to give Home
Assistant or other clients.

## Uninstall

```bash
sudo bash installer.sh uninstall          # keeps config and data
sudo bash installer.sh uninstall --purge  # removes everything
```

Then set the inverter's network gateway back to your router: until you
do, it still routes through this machine, and with the gate removed it is
neither filtered nor served.

## Security notes

| What | Runs as | Privilege |
|---|---|---|
| Gate boot-time drop (`foxess-gate-early.service`) | root | blocks everything the inverter would forward from early boot until the gate is up — and keeps blocking if the gate fails (fails closed) |
| Gate apply / remove (`foxess-gate.service`) | root | fixed, root-owned scripts; matches the inverter by address and, when known, the dongle's MAC |
| Gate check (every 5 min) | root | re-applies the gate if rules vanish; alerts in the journal |
| Upgrade guard (before each app start) | `foxess` | root-owned script: the app cannot change the rollback rule |
| The app | `foxess` | `CAP_NET_ADMIN` only (the TPROXY listener); `NoNewPrivileges`, `ProtectSystem=strict`, writes only `/var/lib/foxess-local` |

- The app owns its binary directory so it can update itself; it only
  installs binaries signed by the release key built into it.
- Network connections the app makes: the inverter's cloud channel (to
  the real cloud only in the forwarding modes you choose) and the daily
  release check. Nothing else; no telemetry.
- The inverter's settings live in memory with limited rewrite
  endurance, so writes are rationed (reads cost nothing): every change
  is read first and not written when the inverter already holds the
  value; each setting accepts at most 10 real writes an hour (more is
  refused with HTTP 429 — an automation that flips a setting every
  minute is stopped, not obeyed); and a failing scheduled change backs
  off (30 s doubling to 15 min) and raises an alert instead of retrying
  every 30 s.
- The web UI and API are LAN-only by default and every write needs a
  token with the right role (read-only < standard < expert < installer).
- Without a token, visitors see the dashboard only — no other pages, no
  settings. `[access] public_all_pages = true` lets them view (never
  change) the other pages too; `public_read_only = false` requires a token
  for everything.

## Other inverter models

If your model is not verified, foxess-local identifies it, picks a
community register map (from the foxess_modbus project) by testing it
against physical checks, and shows live values marked *tentative*.
Settings are locked; an installer can enable untested writes for that
exact model and firmware (Inverter → About this inverter), at their own
risk.

To help add full support: **System → Help → Download bundle**. The bundle contains recent messages and register readings with
serial numbers and dates removed — you can inspect it before sharing it
in a GitHub issue. Nothing is uploaded automatically.

## Home Assistant

[docs/home-assistant.md](docs/home-assistant.md) covers sensors (including
the Energy dashboard), commands and notifications from plain Home
Assistant configuration. The API's full description is served by your
install at `http://<this machine>:18080/api/v1/openapi.yaml`.

## AI assistants (MCP)

An MCP client (Claude Code, for one) can connect at
`http://<this machine>:18080/mcp` with a token: it reads the inverter
and, with a standard token, sets the work mode and schedule — always
through the same checks, limits and audit log as the web UI. See
[docs/mcp.md](docs/mcp.md).

## Notifications

**System → Notifications** sends you a message when something
needs attention, without the web UI open:

- the inverter offline for 10 minutes (and when it is back);
- a grid outage — the inverter switching to backup — and the grid's
  return (also when the grid is already down as foxess-local starts,
  e.g. after losing power itself);
- the gate losing its rules (the inverter was briefly unfiltered — the
  5-minute check puts them back);
- an upgrade that rolled back;
- foxess-local itself restarting after an unexpected stop (a power cut);
- the battery at or below its alert level (20 % by default), or forecast
  to get there within two hours (set under System → Notifications →
  Battery alerts; 0 turns these off);
- a failed command or schedule, a write refused by the hourly limit,
  low disk space;
- the cloud-forwarding mode or a maintenance window changing;
- the cloud asking the inverter to update its firmware: which component
  and file, and whether it was blocked (outside a maintenance window) or
  passed. The download address is not in the message; it is under Activity →
  Raw messages (expert), where the request shows as a "Firmware update
  task".

Two targets, either or both:

- **ntfy**: a push to your phone through the [ntfy](https://ntfy.sh)
  app. Enter a topic URL such as `https://ntfy.sh/foxess-<something
  hard to guess>` and subscribe to the same topic in the app.
  Everything arrives at high priority except "back online".
- **Webhook**: a JSON POST per notice, e.g. to a Home Assistant
  automation with a webhook trigger
  (`http://homeassistant.local:8123/api/webhook/<id>`). The body has
  `event`, `severity`, `title`, `message` and `ts`.

Notices wait on disk until they are delivered, so after a power cut
they arrive once the network and the internet are back, each saying when
it happened.

**Send a test** checks each target. The URLs can also go in
`/etc/foxess-local/config.toml` (`[notify]`); a value saved in the UI
wins. Only installers can see or change them.

## Reporting a problem

**System → Help → Report a problem**: describe what went wrong, choose how much
history to include, and download the report — one file with your
description, the app's version, settings and checks, recent errors, and
the raw messages between the inverter and the cloud. Serial numbers are
replaced and dates shifted; you can open the file (a `.tar.gz`) and read
everything before sharing it. The page then offers a GitHub issue,
prefilled with your description and setup, to attach the file to.
Nothing is uploaded automatically.

For a model or firmware that is not verified yet, run **Inverter → About this
inverter → Scan registers** first: it reads (never writes) every register the
community maps for your model name, which shows a developer which map
fits; the result goes into your next report.

## Troubleshooting

Start with **`sudo /var/lib/foxess-local/bin/foxess-local doctor`**. It
checks the config, the services, the gate's rules and the app's own
readiness, and prints a fix for each failing check (it changes
nothing). Add `FOXESS_TOKEN=<a token>` before the command for the
detail behind each of the app's checks; without `sudo` it skips the
firewall rules.

For monitoring: `GET /readyz` answers 503 while something is wrong (no
token needed; with one, each check's detail), and `GET /metrics` serves
Prometheus metrics — readiness, power, SoC, temperatures — read like the
dashboard.

- **No data / "offline"**: the inverter's gateway must be this machine;
  check `journalctl -u foxess-local` for a session, and
  `sudo /usr/local/lib/foxess-local/apply-gate.sh show`.
- **The app does not start**: `journalctl -u foxess-local -n 50`;
  `sudo -u foxess /var/lib/foxess-local/bin/foxess-local config` checks
  the config file.
- **Lost the token**: stop the app and mint a new one —
  `sudo systemctl stop foxess-local && sudo -u foxess /var/lib/foxess-local/bin/foxess-local tokens create -name new -role installer && sudo systemctl start foxess-local`.
