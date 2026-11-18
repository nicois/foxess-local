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
- `CHANGELOG.md`, and `make tag`. `make tag` picks the next version
  from the entries under Unreleased, dates them, commits and creates
  the tag. Each draft release's notes are its changelog section.

### Fixed
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
