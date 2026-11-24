# OpenStick appliance and setup wizard

**Status: draft, proposed 2026-11-23.** Phase-1 decisions made by the
owner on 2026-11-24 (see Decisions). The remaining open questions are
at the end. Nothing here is implemented.

An OpenStick (a UZ801-class MSM8916 USB stick, `provision-openstick.sh`)
is a cheap gateway that does one thing. Today it still takes SSH,
`nmtui`, a DHCP reservation, the installer and the data logger's web
page to set one up: fine for the owner, out of reach for most users.
This design turns a provisioned stick into an **appliance**. Someone
technical prepares it ahead of time. The end user plugs it in, scans a
QR code, picks their Wi-Fi network, and confirms one step that points
the inverter at it. The same tool helps them take foxess-local out
again.

## Goals

- **Setup without a terminal.** From a phone: join the stick's
  hotspot, enter the home Wi-Fi password, confirm the data logger
  change. No SSH, no IP addresses typed.
- **Ahead-of-time preparation.** All slow, technical work (flashing, the
  TPROXY module build, installing foxess-local) happens before the stick
  reaches the user's house.
- **Self-explaining failure.** When something is wrong, the stick says
  what, in words, with the next step, and shows it on its light.
- **A way out.** Undo the data logger change (back to DHCP or its
  original static address) and take foxess-local out of the path.

Not goals: other gateway hardware (a Raspberry Pi keeps the normal
`INSTALL.md` path); configuring the router; data loggers other than the
FoxESS WLan / Smart WB (other loggers get the manual instructions).

## Facts this design rests on

Checked on a freshly flashed UZ801 v3.0 (OpenStick Debian 12.7, kernel
6.6.0-msm8916, systemd 252), 2026-11-23:

- **One radio, used by one network at a time.** `wcn36xx`; `iw list`:
  "interface combinations are not supported" (modes: managed, AP, IBSS,
  monitor). The stick is in exactly one of three Wi-Fi roles at any
  moment. It is on the home network as a client, runs its own hotspot,
  or is on the data logger's network as a client. Every flow that
  changes role drops the operator's phone.
- **2.4 GHz only** (NetworkManager: `5GHZ: no`).
- **USB networking runs alongside the Wi-Fi.** The image brings up an
  RNDIS gadget, `usb0`, at `192.168.6.1/24` in NetworkManager shared
  mode (it hands the host an address). A laptop plugged into the stick
  reaches it whatever the radio is doing. This is the preparation
  channel and the fallback route to the wizard.
- **Three single-colour LEDs**, on/off only (`max_brightness` 1), with
  the kernel's `timer` and `heartbeat` triggers: `red:power`,
  `green:wlan`, `blue:wan`. They are separate LEDs, so position tells
  them apart as well as colour.
- **A button, not usable by end users:** `gpio-keys` "Restart"
  (`KEY_RESTART`, code 408) is a pinhole next to the SIM slot under the
  back cover (owner, 2026-11-23). Reaching it means unplugging the stick
  and opening it. The wizard does not use it.
- **The IMEI is printed under the back cover**, so reading it also
  needs the stick opened. The modem is the SoC's own (`qcom_q6v5_mss`,
  `/dev/wwan0qmi0`, root 0600). With no SIM, ModemManager reports it as
  `failed: sim-missing` but still reads the IMEI: 15 digits, Luhn valid,
  matching the cover on the test stick. It's available within a minute
  of boot. **Any local user can read it** through ModemManager's D-Bus
  interface (`mmcli -m 0` worked as the unprivileged `user`), and that
  includes `foxess`. ModemManager uses ~15 MB of the 317 MB RAM for a
  modem that has no SIM.
- **The image's own connections:** `wifi-client` (the home network),
  `hotspot` (SSID `Openstick`, `192.168.4.1/24`, shared mode, off),
  `usb`, `lte` (no SIM). `dnsmasq-base` is installed, which shared mode
  needs. RAM: 317 MB.
- **No battery-backed clock.** At boot the clock reads 2024-08-25 until
  NTP sets it, and when the stick is off the home network it may never
  be set. Every timer that must survive a reboot (the power-cycle
  trigger, the sign-in lockout) counts boots and uptime, never wall
  time.
- **Role switches are fast** (spike S2, 2026-11-24, NetworkManager
  1.42.4):
  - home → hotspot: 1.5 s;
  - hotspot → home: 2.7 s, and the router answers at once;
  - a wrong home password is reported in 6.5 s (`nmcli` exit 4,
    "Secrets were required"), distinct from other failures;
  - a client-mode scan takes 1.4 s.
- **No scanning while the hotspot is up.** `iw scan` fails with "not
  supported" in AP mode, and NetworkManager's rescan returns only stale
  entries. The wizard scans before it raises the hotspot and keeps the
  list. A fresh scan means dropping the hotspot for ~5 s (down, scan,
  up). Checking whether the home network is back works the same way,
  which the `home_lost` alternation already does.
- **Captive DNS, scoped to the hotspot** (spike S1). The hotspot
  connection uses manual addressing (`10.42.0.1/24`, not shared mode),
  and the wizard starts its own `dnsmasq` bound to `10.42.0.1` only,
  with `address=/#/10.42.0.1` and a DHCP range. It coexists with the
  image's dnsmasq instances (USB link `192.168.6.1`, NetworkManager's
  `127.0.0.1`). Checked: probe names (`connectivitycheck.gstatic.com`,
  `captive.apple.com`) resolve to `10.42.0.1` on the hotspot, and the
  USB link's resolver doesn't. Shared mode can't be used: it gives
  every shared connection the same `dnsmasq-shared.d`, so an
  `address=/#/` line there would also catch the USB link. `dnsmasq` is
  an OS program the wizard runs, like `nmcli`.
- **The modem's QMI device appears ~9.5 s after boot** with
  ModemManager masked (spike S4), and the first IMEI read succeeds. An
  unsolicited CTL indication (message 0x0027) arrives before the reply,
  so the client must skip unrelated frames.

Further facts:
- **The stick is the inverter's gateway.** Whenever it is not on the
  home network, the inverter has no gateway: no cloud, no NTP, no local
  data. Changing role is therefore never automatic while things work.
- **The data logger** (`INSTALL.md` step 4) runs its own hotspot
  `W-<last 7 digits of its serial>`, password `mtmt2020` from the
  factory, with a web UI at `https://192.168.1.1`. The first login forces
  a new password (8–16 characters, mixed classes), which also becomes
  its hotspot password. It has Wi-Fi settings (SSID, password) and
  network settings (DHCP / static IP, mask, gateway; no DNS). The Status
  page shows "Enter transparent mode" once configured and shows the
  logger's MAC.
- **The data logger is 2.4 GHz too**, like the stick. A home network
  that offers only 5 GHz cannot work, and the wizard must say so instead
  of listing nothing.
- **Home subnets are not always /24.** A `192.168.0.0/16` home network
  contains the image's `192.168.4.0/24` hotspot, the `192.168.6.0/24`
  USB link and the logger's `192.168.1.0/24`. The radio is never on two
  networks at once, so only the USB link coexists with the home
  network, and its more specific route then hides home hosts in
  `192.168.6.0/24` from the stick. The wizard checks for this overlap
  and moves the USB link (to a `10.x` /24 outside the home range) when
  it finds one.
- **foxess-local** already serves `/healthz` (liveness) and `/readyz`
  (readiness: listener, inverter connected and fresh, store). Its
  binary is owned by `foxess` and replaces itself on upgrade (D-023).

## D-031 (proposed): an appliance wizard, separate from foxess-local

### The wizard is its own program, run as root

`foxess-wizard`, a second Go binary from this repository (pure Go,
`CGO_ENABLED=0`, built and signed with each release), runs as a root
systemd unit on OpenSticks only.

- **Why separate.** It must work when foxess-local is missing, broken or
  stopped, and it needs root: NetworkManager, systemd units, the LEDs,
  power-off, running the installer.
- **Why not a subcommand of `foxess-local`.** That binary is owned by
  `foxess` and replaced by the self-upgrade. Running it as root would
  let anything that can write as `foxess` run code as root. The wizard
  is root-owned (`/usr/local/lib/foxess-wizard/`, mode 0755 root), and
  it never executes anything under `/var/lib/foxess-local`. It runs
  `foxess-local` only through systemd, and runs the installer only from
  a release it verified itself.
- **C-006.** "One binary, no sidecar processes" describes foxess-local,
  the system that touches the inverter. The wizard never speaks the
  inverter's protocol, never writes a register and never sits in the
  data path. It is OpenStick appliance tooling, like
  `provision-openstick.sh`. Decided (owner, 2026-11-24): the wizard is
  outside C-006's scope, recorded as a scope note on the constraint;
  C-006 itself is unchanged.
- **Updates.** The wizard checks the same releases and verifies them
  with the same embedded minisign key as D-023, and replaces itself
  (root, signed releases only, previous copy kept). It updates
  separately from foxess-local, so a bad foxess-local release cannot
  take the recovery tool with it.

### States

The wizard keeps a small state machine. Every timer runs on injected
time (C-004: `synctest` tests).

| State | Meaning | Wi-Fi role |
|---|---|---|
| `unconfigured` | no home Wi-Fi credentials | hotspot |
| `joining` | trying the home network | home client |
| `normal` | on the home network, foxess-local live and ready | home client |
| `degraded` | on the home network, something needs attention (see triggers) | home client |
| `home_lost` | credentials known, home network unreachable | home client, retrying |
| `hotspot` | hotspot open for an operator | hotspot |
| `logger_setup` | on the data logger's network, configuring it | logger client |
| `awaiting_inverter` | back home, waiting for the inverter to connect | home client |
| `stopped` | foxess-local stopped by an operator | home client |
| `reverting` | undoing the logger change, then taking foxess-local out | logger client, then home |

### When the hotspot opens (and when it doesn't)

Opening the hotspot takes the inverter's gateway away, so it opens only
when the stick is already off the home network, or when someone at the
stick asks for it:

- **No credentials** (`unconfigured`): at once, and it stays open.
- **Home network lost** (`home_lost`): retry with backoff for 10 min,
  then alternate a 5-minute hotspot window (held open while an operator
  is connected) with a 2-minute retry, indefinitely. The retry is
  needed because a router restart must never leave the stick parked in
  hotspot mode.
- **On request at the stick:** unplug and replug it three times within
  five minutes (owner, 2026-11-24). Without a clock (see Facts), the
  wizard counts *short boots*. On each boot it records its uptime to
  flash at 10, 30, 60 and 120 s (fsync, then rename). A boot whose last
  record is under 120 s is short. Three short boots in a row, whose
  recorded uptimes plus ~30 s of boot time each add up to under five
  minutes, open the hotspot. A boot that reaches 10 minutes of uptime
  clears the count. This is the "I changed my Wi-Fi password" path. The
  button is not an option (see Facts). A laptop on the USB link reaches
  the wizard without opening the hotspot at all.
- **Not** when foxess-local is unhealthy or the inverter is silent but
  the home network works. Those are `degraded`: the wizard reports them
  on its home-network page and the light, restarts foxess-local if it is
  crash-looping (systemd already restarts it; the wizard only reports
  `failed`), and offers the logger step. It does not take the network
  down.

foxess-local keeps running while the hotspot is up. It cannot reach the
inverter, but it holds the gate, the store and its notifications outbox,
and it carries on by itself when the stick rejoins. The wizard stops it
only for an operator action that needs it (changing its configuration,
revert).

### Triggers for `degraded`

From the wizard's own checks every 15 s, with a short hysteresis:

| Check | Source | Shown as |
|---|---|---|
| foxess-local not live for 2 min | `/healthz`, `systemctl is-failed` | "foxess-local has stopped", with its last log lines and a restart button |
| inverter not connected or stale for 10 min | `/readyz` check ids | "No data from the inverter", with the logger step |
| the stick's home address changed | NetworkManager | "This stick's address changed; the inverter is pointed at the old one", with the fix (see Addresses) |
| clock, disk, gate | `/readyz` | foxess-local's own detail and fix text |

### Reaching the wizard

- **On the hotspot:** SSID `foxess-<4 hex>`, WPA2 with a per-stick
  password generated during preparation. A captive portal answers every
  DNS name with the stick's address, so the phone opens the wizard
  page by itself (the wizard's own `dnsmasq` on the hotspot; see
  Facts). The wizard keeps its own NetworkManager connection, with
  manual addressing, and leaves the image's `hotspot` connection
  untouched.
  Subnet `10.42.0.0/24`. Only one network is up at a time, so it need
  not avoid the home range.
- **Over USB:** `http://<usb address>:18081` from a laptop plugged into
  the stick, at any time, including while the radio is on the logger's
  network. Signing in needs the stick's password, as on the home
  network.
- **On the home network:** `http://foxess-<4 hex>.local:18081`, announced
  by mDNS (a small pure-Go responder), and the stick's address. The
  foxess-local UI links to it (a System hub entry, OpenStick only).
- **Authentication.** A phone that joined the hotspot has proved it has
  the stick's password (it is printed on the stick); that session is
  signed in. On the home network the page asks for the same password.
  foxess-local tokens are not used: the wizard must work when
  foxess-local does not.

### The IMEI as proof of physical access (proposed)

The stick's password (on the label) proves the person is at the stick
or has the label. A few operations deserve a second proof, one a
device on the home network or someone who saw the label can't have.
Those operations: revert, changing or showing the stick's password,
resetting the wizard to `unconfigured`, and showing the data logger's
stored password. For these, the page asks for the **last 7 digits of
the IMEI** from under the back cover.

- **Why 7 digits.** An IMEI is an 8-digit model code (TAC: the same on
  every UZ801), a 6-digit serial and a Luhn check digit. The last 7
  digits are therefore the whole serial plus the check, about 10⁶
  possibilities, as strong as the full 15 digits once the model is
  known. The last 4 would be ~10³ (the last digit is the check), too few.
- **Online only, rate-limited.** 10⁶ is fine against guessing through
  the page: 5 attempts, then a lockout that doubles, starting at
  15 minutes and surviving a restart. It is useless offline, so the IMEI
  must never become a Wi-Fi password, a key, or a stored unsalted hash.
  The wizard reads it from the modem when it checks an answer and keeps
  nothing.
- **Never shown, never sent.** The wizard never displays, logs or
  reports it. foxess-local's diagnostic bundle (D-022) and the
  identifier scan treat an IMEI-shaped number like a serial.
- **Only root can read it.** Preparation stops and masks ModemManager
  (no SIM, no LTE; it also frees ~15 MB), so the IMEI is reachable only
  through `/dev/wwan0qmi0`, which is root 0600. The wizard reads it
  there with a QMI DMS "get device serial numbers" request: a few
  framed messages, small enough to implement in pure Go. A compromised
  foxess-local therefore can't read the IMEI and use it to unlock the
  gated operations. Keeping ModemManager and restricting it with a
  D-Bus policy was rejected: more moving parts for a modem that isn't
  used.
- **When the modem can't supply one.** The modem needs no SIM to report
  its IMEI (checked on the test stick). But
  reflashing an MSM8916 stick without restoring its modem calibration
  partitions (`modemst1`/`modemst2`/`fsg`) is known to erase the IMEI.
  The modem then reports zeros, a placeholder, or nothing. If the value
  isn't 15 digits, fails the Luhn check, or is all zeros, the wizard falls back to the label password alone and says
  so on the page. Preparation reads the IMEI once and warns the
  preparer when it's missing, so a stick isn't shipped relying on it.
- **What it doesn't protect.** Anyone who has the stick in hand has
  both proofs. The IMEI guards against the home network and the label,
  not against theft.

### The label

Preparation prints a label to stick on the device: the hotspot SSID and
password, a Wi-Fi QR code (`WIFI:T:WPA;S:<ssid>;P:<password>;;`), and
the `.local` name. Scanning the QR code is the whole first step.

### Flow 1: first setup at the user's house

1. The stick boots into `unconfigured`. The light shows red, blinking.
2. The user scans the QR code. The captive page opens (red, solid: an
   operator is connected). It explains the three steps ahead.
3. **Home Wi-Fi.** The wizard lists the networks it scanned just before
   raising the hotspot, by signal strength, with a note that only
   2.4 GHz networks can appear. "Scan again" drops the hotspot for
   ~5 s; the page says so first, and the phone rejoins by itself (to
   confirm, S3). The user
   picks one and types the password.
4. **The switch.** Before leaving, the page says exactly what will
   happen: "The stick will now leave this hotspot and join *Network*.
   If the light turns solid green, join *Network* again on this phone
   and open `foxess-xxxx.local:18081`. If it turns red and blinks again,
   rejoin this hotspot: the page will show what went wrong." The stick
   joins (green, blinking).
   - **Success:** credentials stored (NetworkManager connection,
     autoconnect), green solid, state `awaiting_inverter` if the logger
     is not yet pointed at the stick.
   - **Failure** (wrong password, no DHCP, no internet when the release
     download needs it): back to `hotspot` within 60 s with the reason
     on the page.
5. **The data logger** (Flow 2), offered on the home-network page.
6. **foxess-local setup.** Once the inverter connects, the page hands
   over to the foxess-local UI's setup page with the first installer
   token (shown once by the wizard, which got it from the installer run
   in Flow 2).

### Flow 2: point the data logger at the stick (explicit confirmation)

The stick leaves the home network, which takes the inverter offline for
a minute or two. It runs only on an operator's explicit confirmation.

1. **Find the logger.** Scan for `W-` + 7 characters. More than one: the
   operator picks (the page shows signal strength; the serial digits
   match the logger's sticker). None: "Is the inverter on? The data
   logger's hotspot is not visible", with how to restart it.
2. **Credentials.** Operator hints first (an optional field: "if you
   changed the data logger's password, enter it"), then the stored
   password from an earlier run, then the factory default. At most
   three login attempts per run (the logger's lockout behaviour is
   unknown). The logger's HTTPS certificate is pinned on first use.
3. **Forced password change.** If the logger demands a new password,
   the wizard generates one that meets its rules. It stores the password
   and shows it on the page, telling the operator to note it down
   ("this is now also the data logger's hotspot password").
4. **Record before changing.** The logger's current Wi-Fi SSID, its
   network type, address, mask and gateway, and its MAC are stored
   (`/var/lib/foxess-wizard/logger.json`, root 0600) *before* any
   change. This record is what revert restores, and the page shows it
   for a manual undo.
5. **Write.** Wi-Fi: the home network's SSID and password, which the
   stick already has. Network: static IP = the inverter address (see
   Addresses), the home network's mask, gateway = the stick's gateway
   address. Submit, then read the settings back.
6. **foxess-local.** If foxess-local was installed without an inverter
   (preparation), the wizard runs the verified installer with
   `--inverter-ip` and `--inverter-mac`; otherwise it updates both and
   restarts it.
7. **Back home and wait** (`awaiting_inverter`): up to 10 min for
   `/readyz` to report the inverter connected. Success: green solid, the
   page goes on to foxess-local setup. Timeout: the page says what was
   written, offers "try again" and "undo", and lists the likely causes
   (weak signal at the inverter; the home network has no 2.4 GHz).

If anything fails between step 4 and step 5's read-back, the wizard
tries once to restore the recorded settings before going home, and
reports both outcomes.

### Addresses

The inverter's static address and the stick's address must both stay
put, and the wizard cannot see the router's DHCP range.

- **The stick's gateway address.** Proposed: a second, fixed address on
  the stick's home interface (an alias), chosen once and used only as
  the inverter's gateway. The stick's own DHCP lease can then change
  without breaking the inverter. Choosing it: from the top of the /24
  that holds the stick's own address, counting down (`.250`, `.249`, …),
  even when the subnet is wider (a /16, say). Skip any
  address that answers an ARP probe, and re-probe at every boot (a
  conflict is a `degraded` check). A DHCP reservation on the router
  remains the recommendation, and the page says so.
- **The inverter's address.** Chosen the same way, next below the
  gateway alias, unless the operator gives one.
- **The risk:** an address above the probe could still be in the
  router's pool and be handed out later to a device that was off
  during the probe. The high end of a /24 is usually outside consumer
  routers' default pools, but not always. That is why the page
  recommends a reservation for both addresses and shows the MACs.

### Flow 3: stop, power off (from foxess-local)

New installer-only actions in foxess-local's System hub, audited as
system events:

- **Stop foxess-local.** The app writes a request to
  `/run/foxess-local/requests/` (its own runtime directory) and exits
  cleanly. The wizard picks up the request, stops the unit so systemd
  does not restart it, and enters `stopped`. Its page then offers
  start, revert and power off.
- **Power off the stick.** Same channel. The wizard runs `systemctl
  poweroff` after foxess-local has stopped (the store closed cleanly).

The wizard reads only a fixed set of verbs from that directory and
ignores anything else. foxess-local gains no privilege: it asks, and
root decides.

### Flow 4: revert (take foxess-local out)

From the wizard page, with a confirmation that says what changes:
"the inverter will talk to the FoxESS cloud directly again; the cloud
can change its settings and its firmware". This is the end of C-001 and
C-002 for this installation, so the page says so in plain words.

1. Join the logger's network (`reverting`), log in, and restore the
   recorded network settings (DHCP, or the original static address and
   gateway). The logger's Wi-Fi settings stay as they are.
2. Back home. Confirm the logger has left the stick: no more packets
   from its MAC to the gateway alias for 5 minutes.
3. Stop and disable foxess-local, then remove the gate (`rollback.sh`).
   The data stays on the stick unless the operator also asks to delete
   it.
4. Report the result. The stick stays usable: the wizard can set
   everything up again later from its stored state.

Without a working stick there is no wizard. `INSTALL.md` and the label
then point to the manual steps: join the logger's hotspot, set Network
Configuration back to DHCP.

### The light

The stick has three separate on/off LEDs: `red:power`, `green:wlan`,
`blue:wan`. Which LED is lit and how it blinks carry the meaning, so
the light can be read without telling colours apart. Blinking uses the
kernel's `timer` trigger (`delay_on` / `delay_off`), so the pattern
keeps running if the wizard stalls. A double blink needs the wizard to
drive the LED itself, and stops if it hangs, which is itself a signal.

| State | LED | Pattern |
|---|---|---|
| `unconfigured`, `hotspot`, nobody connected | red | slow blink (1 s) |
| `hotspot`, operator connected | red | solid |
| `joining`, `home_lost` retrying | green | fast blink (0.25 s) |
| `logger_setup`, `reverting` | blue | fast blink |
| `awaiting_inverter` | green solid + blue slow blink | |
| `degraded`, `stopped` | green solid + red slow blink | |
| `normal` | green | solid |
| boot, before the wizard runs | (the image's own) | |

### Preparation (ahead of time)

A new `provision-openstick.sh --appliance` mode, run once by whoever
prepares the stick (over USB networking or a temporary Wi-Fi
connection):

1. Everything the script does today, including the TPROXY build
   (5 minutes, done here rather than at the user's house).
2. Install foxess-local **without an inverter** (a new installer flag,
   `--no-inverter`: gate rules and the listener wait for an address;
   `/readyz` reports "no inverter configured"). Install the wizard.
3. Generate the stick's hotspot SSID and password, and print the label
   (a PNG or PDF for the preparer, plus the text).
4. **Seal:** remove the preparation Wi-Fi connection; replace the
   image's well-known `user` password with a random one, or lock the
   account; turn SSH password logins off (keys only). That closes the
   open default-password item for every appliance stick. Stop and mask
   ModemManager, then check that the wizard can read a valid IMEI over
   QMI; warn if it can't (see the IMEI section). On the next
   boot the stick is `unconfigured`.

Later (phase 4): a prebuilt, signed TPROXY module package for the
OpenStick image's fixed kernel version, and then a ready-to-flash image.
Either removes the on-stick build.

### Privacy

The logger's SSID holds 7 digits of its serial number, and the stored
record holds its MAC. The wizard's logs name the logger `W-…` with the
digits replaced, and `logger.json` stays root-only. The diagnostic
bundle (D-022) treats both like the other identifiers if it ever
includes wizard state.

### Testing

- The state machine and every timer (backoff, hotspot windows, the
  hysteresis, the inverter wait) are tested with `synctest` (C-004).
- NetworkManager, systemd, the LEDs and ARP probing sit behind
  interfaces with fakes. The NetworkManager side is `nmcli` behind an
  interface, or D-Bus with `godbus` (pure Go). `nmcli` is simpler to
  start with.
- The logger client is tested against recorded pages from a real
  logger, with serials and MACs replaced by placeholders (repository
  privacy rules).
- An end-to-end rehearsal on the test stick for each phase, including
  a failed home password, a moved router, and revert.

## Phases

1. **Wizard core:** root unit, state machine, hotspot + captive portal,
   home Wi-Fi onboarding, home-network status page, mDNS, LEDs,
   `--appliance` preparation with label and seal, `--no-inverter`.
2. **Data logger:** recon of the logger's web UI (a recorded browser
   session), then Flow 2 with the record-before-change, the address
   choice, and the installer hand-off.
3. **Stop / power off / revert:** foxess-local's request channel, Flows
   3 and 4.
4. **Faster preparation:** prebuilt TPROXY modules, then an image.

## Unknowns to check on hardware

Resolved (see Facts): interface combinations (none), 5 GHz (no), the
LEDs, the button, dnsmasq (present), on 2026-11-23. Role-switch
timings and scanning in AP mode (S2), captive DNS scoping (S1) and QMI
after boot (S4), on 2026-11-24. The QMI client used for S4: CTL "get
client ID" for DMS, DMS 0x0025 "get device serial numbers", TLV 0x11,
"release client ID", over raw QMUX framing (`0x01` marker, as with
`cdc-wdm`). It read the IMEI as root in 2–3 ms; as `user`, the open
fails. Still open:

- **S3, phones (needs the owner):** does an Android phone and an
  iPhone open the sign-in page by itself on the hotspot, and which
  probe URLs must answer how? Does a phone rejoin by itself after the
  hotspot drops for a ~5 s rescan?
- **S5, power cycles (needs the owner):** three unplug/replug cycles
  within five minutes; the short-boot records survive and nothing is
  corrupted.
- Whether the USB link's DHCP works with phones over USB-C OTG
  (Android RNDIS host support varies), or only with laptops.
- The logger's web UI: form fields, the forced-change flow, whether its
  hotspot stays visible once it is on the home network (if not, revert
  needs it brought back: a logger restart, or a hardware reset that
  also forgets its Wi-Fi), lockout after failed logins, whether it
  reboots on submit.

## Decisions (owner, 2026-11-24)

- **C-006:** the wizard is appliance tooling outside its scope; a scope
  note is added to C-006, the constraint is not amended.
- **Hotspot on request:** three power-cycles within five minutes. The
  USB link remains an extra way in.
- **foxess-local during the hotspot:** keeps running.
- **Preparer:** only the owner for now. Preparation is a script mode
  with a text and PNG label; no preparer's guide yet.

## Open questions for the owner

1. **Address choice** (phase 2): the gateway alias with ARP-probed high
   addresses (proposed), or require a router reservation and ask for
   the addresses?
2. **Revert scope** (phase 3): keep foxess-local's data by default
   (proposed)?
3. **IMEI gate** (phase 3): which operations need it (proposed: revert,
   stick password, wizard reset, showing the logger's password), and is
   asking someone to open the cover acceptable for them?
