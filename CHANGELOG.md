# Changelog

Notable changes to foxess-local, newest first. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions
follow [semantic versioning](https://semver.org/).

Add entries under **[Unreleased]** as changes land, under one of these
headings:
- **Breaking:** you must act when upgrading.
- **Added**
- **Changed**
- **Deprecated**
- **Removed**
- **Fixed**
- **Security**

`make tag` picks the next version from them. Breaking or Removed bumps
the major version (the minor one before 1.0); Added bumps the minor
version; anything else bumps the patch version.

## [Unreleased]

### Added

- Schedule override for automations (`PUT /api/v1/schedule/override`): a
  force discharge or force charge from now until a given time, with a
  cut-off SoC that the inverter enforces. foxess-local puts the window in
  a free schedule group without moving yours, and clears it again when
  it ends, when it is cancelled, or after a restart. The token that
  started it renews it by asking again with a later end; if the
  automation stops, the override lapses. "Running now" shows who holds
  it and until when, with "End override now".
- Notices from automations (`POST /api/v1/notices`): a sidecar sends a
  notice with its own token, and it reaches your webhook and ntfy like
  foxess-local's own, labelled with the token's name. At most 20 an hour
  per token.
- Activity → Notices lists every notice from the last 30 days, from
  foxess-local and from automations, whether or not a notification
  target is set (`GET /api/v1/notices`). The database schema is now v14.
- `examples/aemo-spike`: an example sidecar that force-discharges while
  the NEM's dispatch price for your region is high, and tells you through notices. It is a starting point for your own
  automations, small enough for an OpenStick.

### Fixed

- Cloud forwarding `full` works again. Since 2026-11-20 08:08 UTC it held
  the cloud's replies to the inverter's registration, so the inverter
  never finished connecting and sent no data until you switched back.
  A byte in those replies is a clock, and it ticked over from `6a` to
  `6b` that morning; foxess-local had taken it for a constant. `off`
  mode had the same fault: its commands went out without the session's
  clock bytes. foxess-local's own replies now carry the clock as the
  cloud's do.
- Activity → Messages and `GET /api/v1/messages` show each frame's
  header (its device bytes and function byte), not only its payload.
- Opening another page in the web UI starts at its top. It kept the
  previous page's scroll position, so after scrolling down Activity, the
  Inverter page opened part-way down.
- "Running now" (Home and the Inverter page) names the schedule's
  default group as the default mode, instead of its 00:00–23:59 window.
- The low-battery forecast no longer warns about a scheduled force
  discharge as if it ran on forever. During one, it takes the measured
  discharge rate until the discharge is expected to end — the window's
  end or its cut-off SoC, whichever comes first — and the house's recent
  usage (its load less solar) from there. It has a forecast within a
  minute of a session starting. After a session, its readings are left
  out of the forecast.

## [v0.3.0] - 2026-11-20

### Added

- The state of charge between the inverter's whole-percent readings,
  estimated from the battery power. The dashboard shows it with two
  decimals; the API has it as
  `battery.soc_estimate_pct` beside `soc_pct` (in `/state` and the live
  events), and `/metrics` as `foxess_battery_soc_estimate_percent`. It
  needs the battery capacity.
- The battery's measured capacity: the energy between whole-percent
  steps, learnt separately for charging and discharging and kept across
  restarts. It corrects the estimate for a pack that has lost health or
  a capacity set wrong, and is in `/state` (`battery.measured_capacity`)
  and `/metrics`.
- The SoC estimate follows the battery's own SoC. The battery reports
  its state of charge to 0.1 % about every 5 minutes, and the estimate
  starts again from each report. Without those reports it follows the
  inverter's whole-percent reading, which trails the battery by up to a
  percent. Measured on a KH10, the estimate was within 0.04 % of the
  battery's figure at the median and 0.14 % at the 95th percentile.
- History: the SoC estimate is stored with each reading. The State of
  charge chart draws it, with the inverter's whole-percent reading
  dashed beside it; `GET /api/v1/history` has it as `soc_est`, and the
  data table and exports as "SoC estimate". Readings stored before this
  version have none.

### Changed

- The web UI's pages are grouped by what you come to do. **Inverter**
  (was Control) holds the inverter's settings, with links to **About
  this inverter** (was More → Device) and its quirks. **Activity** shows
  the change log and, one tap away, the raw messages (was More → Frame
  messages). **System** (was More) is a hub: FoxESS cloud,
  Notifications (now with the battery alerts, which were under Tuning),
  Access (tokens and visitor access), Updates, Site (time zone, clock,
  language), Storage, Help (report a problem, the diagnostic bundle,
  the API description) and Setup. Settings is split across these.
  Old links still open the right page.
- The foxesslocal name in the header goes to Home; on wide screens the
  separate Home tab is gone (phones keep it in the bottom bar). The
  cloud badge opens System → FoxESS cloud, the update badge System →
  Updates.
- Each setting has one home, and Setup walks through the same panels:
  the time zone, inverter clock and battery capacity are under System →
  Site, visitor access under System → Access, cloud forwarding under
  System → FoxESS cloud (the wizard's own "Nothing / Readings only /
  Everything" wording is gone; it uses Off / Telemetry only / Full like
  the rest). Until setup is marked done, System shows "Finish setting
  up" first, with what is still to do.
- Home says what the inverter is running now and why (a schedule group
  and its window, or the base work mode), linked to the schedule.

### Fixed

- Pages load faster, especially through a reverse proxy or over a slow
  link. The page files are sent compressed (the app's script shrinks from
  175 KB to 53 KB), a repeat visit re-downloads only what changed, and
  the two modules the app needs are fetched alongside it rather than
  after it.
- The dashboard no longer jumps as it loads. A scrollbar appearing when
  the history filled in narrowed the page, which shrank the flow diagram
  and moved everything below it.
- Inverter page on a wide screen: the page uses the width. The diagram
  stays on the left; the work mode, the limits (their groups side by side)
  and the links fill the right, so the whole page fits a 1080-pixel-high
  screen.
- Dashboard: the two-column layout (the live view beside the history)
  is used only when the whole live view fits the window's height. On a
  wide but short window the bottom of the live view was cut off; it now
  stays one column there, and switches as the window is resized.
- The web UI rides out a restart. A page loaded while foxess-local was
  restarting (after an update, say) signed you out, even with "keep me
  signed in"; it now keeps the stored token and tries again until the
  service answers. Only a rejected token still signs you out.
- After an update, an open page reloads into the new version by itself
  once the service is back, so it no longer runs the old version's
  pages. With an unsaved edit it waits until that is saved or
  discarded.
- Schedule: a window in groups 4–8 shows its grid import and export
  limits and whether it uses solar. Only groups 1–3 were read, so a
  default (remaining-time) window that the cloud placed in group 4
  showed none of them.
- The "battery running low" forecast works on a slow discharge. It fits
  the estimated state of charge: whole-percent readings at about 1 %/h
  never warned, or warned early from a single step.
- History charts: a legend entry's line sample no longer wraps away from
  its name on narrow screens.
- Settings written from foxess-local reach the inverter. Every write
  (work mode, limits, the schedule, the backup-port mode, the clock) was
  sent with the frame header of a read, and the inverter ignored it
  without an answer, so the change was reported as unconfirmed. Writes
  now carry the header the FoxESS cloud uses.
- Control → Backup (EPS) port: choosing the other mode works. Clicking a
  mode asked to switch to the current one, and confirming did nothing.

## [v0.2.0] - 2026-11-19

### Added
- A site time zone, and foxess-local keeps the inverter's clock. Setup
  asks where the inverter is (`site.time_zone`, or `[site] time_zone`):
  the inverter's clock and schedule follow that zone, never the zone of
  the machine foxess-local runs on, so a gateway on UTC works. The
  FoxESS cloud used to set the inverter's clock; with its writes blocked
  the clock drifted and would have missed daylight saving. foxess-local
  now corrects drift and daylight-saving changes by itself, from an
  NTP-synchronised clock, at most hourly and audited; a larger
  difference waits for you (Setup offers to set the clock,
  `POST /api/v1/clock/set`). A zone the inverter's clock disagrees with
  needs confirming.
- Cloud firmware updates are visible. When the cloud asks the inverter to
  update a firmware component (master, slave, manager or the
  datalogger), Frame messages shows a "Firmware update
  task" with the component, file, size, CRC and download address (expert
  and installer); Activity names the file and component instead of
  "other"; and a notification says what was requested and whether it was
  blocked (outside a maintenance window) or passed. The notification and
  Activity leave out the download address, which is tied to your account.
- OpenStick provisioning (`provision-openstick.sh`, in every release): turns
  a UZ801-class MSM8916 4G USB stick running the OpenStick Debian image
  into the inverter's gateway, then installs foxess-local. Idempotent
  (`--check` reports only). It builds the TPROXY modules the stick's
  kernel lacks from pinned kernel.org source, pins the package sources
  to the installed Debian release, adds zram swap, caps the journal,
  turns Wi-Fi power saving off, and warns about the image's default
  password (`--ssh-keys-only`). INSTALL.md: "On an OpenStick".
- A write delay for fragile storage (SD cards, eMMC: OpenStick-class
  boards). New readings wait in memory and are written in fewer, larger
  batches, every 60 s by default (`[store] write_delay_s`, 0–900;
  also under More → Settings → Tuning, expert).
  - A power cut loses at most what was waiting.
  - Commands and setting changes are written at once, a clean stop
    writes everything, and History and Messages are never behind.
- Battery alerts (`battery-low`, `battery-forecast-low`): a notice
  when the charge is at or below the alert level (20 % by default), and
  when a line fitted to the last 30 minutes says, with 95 % confidence,
  that it will be there within 2 hours. All four knobs are in
  `[alerts]` and under Tuning (expert); a level of 0 turns them off.
- Expert tokens can change the tuning settings; the other settings
  still need an installer.
- Lifetime energy counters in `/state` and in every SSE snapshot
  (`energy`: grid export / import, PV, battery charge / discharge, load,
  inverter output, in kWh). They come from the inverter's own registers,
  read every 30 s, and never decrease; a real reset is announced with a
  `counter-reset` system event. For the foxess-control local backend and
  the Home Assistant Energy dashboard.
- The write budget is visible: `/state.write_budget` per setting
  (`limit`, `remaining`, `window_s`, `next_free_at`), and config events
  carry it. A refused write's 429 names the `setting` and `next_free_at`,
  with a `Retry-After` header. The limits are configurable
  (`[writes] per_hour`, `[writes.per_setting]`).
- `/config.time_zone_name`: the host's IANA time zone.
- Prereleases: `make tag PRE=1` tags `vX.Y.Z-rc.N`, and CI drafts it as
  a GitHub prerelease. Installations are offered prereleases only when
  an installer opts in, and an installation on a prerelease is offered
  the release that follows it.
- Update choices (installer, More → Settings → Updates, and `[update]`
  in the config file): how often to check (default a week; never is a
  choice), prereleases, and installing updates automatically — each new
  update tried once, never again after a rollback.

- A header badge when a newer version is available ("update ↑" on a
  phone), linked to Settings → Updates.
- `/update` carries `reason_code`, so a UI can translate why an update
  cannot be applied here.

- History: the Home chart draws the backup (EPS) port's share beside the
  whole household load (which includes it), when the range has any; the
  history table, CSV/JSON export and `/history` (`eps`) carry it too.
  History buckets keep it from now on (schema v12); older buckets get it
  from their raw samples while those are kept (30 days by default).

- Inverter quirks (More → Inverter quirks, `[quirks]` in the config
  file, `GET /quirks`): switches for reading behaviours seen in the
  field, off until reports show them on other installations. The first,
  `eps_dropouts`, shows the backup port on the dashboard as the median
  of its last 5 readings. Problem reports say which quirks were on.

### Changed
- The schedule's "active now" (`/state.active_mode`) and the app's own
  force-charge windows (`[scheduler]`) follow the inverter's clock (else
  the site time zone) instead of the host's zone; `active_mode` is null
  until either is known. `/config.inverter_clock_offset_s` is measured
  against the site time zone (null until one is chosen), and `/config`
  gains `site_time_zone`. A missing site zone does not make `/readyz`
  fail.
- Release checks run weekly by default (they were daily).
- Frame-level data needs the expert or installer role. The raw frame
  log (`GET /messages`, More → Frame messages) refuses read-only tokens
  and public access. Activity (`GET /audit`) shows them the operator
  actions only, without the cloud's individual messages, and says so
  (`cloud_hidden`).
- On a very wide screen (1400 px and up) the dashboard uses two columns:
  the header, live view and footer on the left, the energy history on
  the right with its own scrolling, so the live view stays in sight.
- The web UI in German, French, Italian, Spanish and Simplified Chinese.
  It follows the browser's language, with a picker under More →
  Settings → Language that is remembered on the device. Dates follow the
  chosen language. The translations are machine-made (one JSON file per
  language; corrections welcome). The API, logs and notifications stay
  English.
- `CHANGELOG.md`, and `make tag`. `make tag` picks the next version
  from the entries under Unreleased, dates them, commits and creates
  the tag. Each draft release's notes are its changelog section.

### Fixed
- Live data no longer stops for five minutes after one lost read. On a
  lossy link (an OpenStick on Wi-Fi) a lost version read during the
  5-minute model check threw away the inverter's identified layout, and
  live polling paused until the next check.
- The first install could leave the egress gate failed, or with every
  rule doubled: the installer starts the gate and its rule-loss check
  together, and the check re-applies a missing gate at the same moment.
  Gate changes now take a lock. Seen on an OpenStick; any slow machine
  could hit it.
- The web UI works behind a reverse proxy under a path prefix
  (`/foxess/`): it loaded its files and called the API at absolute
  paths. INSTALL.md has the nginx block.
- The dashboard's main-board load (household − backup port) never shows
  below 0, and the history's backup line never rises above the household
  line: the difference is measurement error (the grid meter's offset),
  not a load.
- The Update choices ran together on one line; the reason an update
  cannot be applied ("a source build: update by rebuilding") was not
  translated.
- The web UI did not load at all (a syntax error in the translated
  More page, unreleased). A test now checks every UI script as an ES
  module; `node --check` on a `.js` file had silently passed it.
- A write whose frame could not be sent no longer counts against the
  write budget.
- The API description now lists every problem code the API returns
  (a test keeps them in step), the 429 on every write endpoint, and the
  fields `/state.ts`, `/config.forward_mode` and the compatibility
  report's `layout` and `third_party`, which were returned but
  undocumented.
- Two garbled sentences on the Device page (the register scan's
  description).
- The release workflow uploads the release files by name. It used to
  upload everything in `dist/`, including the empty signature
  placeholder, which GitHub refused. That left the v0.1.1 draft with
  only `SHA256SUMS`, and its files had to be added by hand.

## [v0.1.1] - 2026-11-18

### Fixed
- Key IDs read as minisign prints them (`553C34FE26291351`). They were
  printed byte-reversed, in `verify-release` and in the "signed by
  another key" error.

### Changed
- `make sign` uses the key in the signing account (`SIGN_KEY`), and
  `make release` leaves a signature file that account can write.

## [v0.1.0] - 2026-11-18

### Added
- First public release:
  - local monitoring and control of FoxESS hybrid inverters (verified
    on the KH10), behind a default-deny egress gate;
  - a REST API, a web UI and an MCP endpoint;
  - notifications that survive restarts (webhook, ntfy), including a
    lost grid and a restart after a power cut;
  - maintenance windows, which can record their traffic;
  - cloud forwarding off, read-only or full;
  - signed, reproducible releases that upgrade themselves, with an
    automatic rollback.
