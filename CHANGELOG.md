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

### Changed
- Release checks run weekly by default (they were daily).
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
