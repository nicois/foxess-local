# foxess-local

Local monitoring and control for FoxESS hybrid inverters (verified: KH10).
One Go 1.27 binary on the inverter's network gateway, behind a default-deny
egress gate (nftables + TPROXY). It terminates the inverter's proprietary
cloud channel (framed binary over TCP/14431), decodes every frame, applies
policy to every frame and keeps the data locally. It serves a self-owned
REST API (Home Assistant via the foxess-control integration's local
backend) and a web UI. Cloud forwarding is opt-in.

Installation-specific notes, when present, are in `CLAUDE.local.md`. That
file is git-ignored and never committed.

## Build / Test / Lint

```bash
go build ./...
go test ./...
prek run --all-files    # the lint hooks — exactly what CI's lint job runs
```

`make all` builds what ships:

- the stamped `foxess-local` binary (git short hash, D-018);
- the standalone `installer.sh`. `deploy/` is the source of truth and
  `deploy/gen-installer.sh` the generator.

Releases (D-023, `RELEASING.md`):

- `make release VERSION=vX.Y.Z REPO=owner/name` builds reproducible static
  amd64/arm64/armv7 binaries, the installer and `SHA256SUMS`.
- `make sign` signs offline with minisign; `make verify-release` checks
  the result.

Users install per `INSTALL.md`. Installed copies self-upgrade without root
(`internal/update`, with the rollback guard `deploy/upgrade-guard.sh`).

For UI work, `go run ./tools/devui` serves the real API and UI over a
seeded demo store. It needs no inverter, gate or root.

The commit hook (prek; `prek install`) runs golangci-lint, shellcheck and
the generated-installer check from the revs pinned in
`.pre-commit-config.yaml`. The lint rules are in `.golangci.yml`. CI runs
the same hooks, so `prek autoupdate` is the one place to update the
linters.

- Never invoke a hand-installed golangci-lint binary.
- No `git commit --no-verify` without naming the failing linter in the
  commit message.

## Key constraints

- **C-001**: no upstream→inverter message flows outside `full` mode and an active maintenance window (both explicit, installer-gated, audited), except that `telemetry_only` passes well-formed Modbus read requests (they cannot change the inverter; D-024); everything else deny-all (logged + dropped). Every answer goes to whoever asked: answers to this app's requests never reach the cloud.
- **C-002**: cloud-initiated inverter modification flows only in `full` mode or an active maintenance window; firmware only in a maintenance window (`full` passes only the frame kinds the cloud is known to send and holds the rest — D-025); in `off`/`telemetry_only` no cloud write reaches the inverter.
- **C-003**: all operator commands pass one validated, range-clamping gateway; the frame service is the sole upstream→inverter path.
- **C-004**: red-first TDD; time-driven logic tested with `synctest` virtual time, never wall-clock sleeps.
- **C-005**: the REST API is self-owned — never a mimicry of the cloud API.
- **C-006**: Go 1.27, single binary, pure-Go dependencies.
- **C-007**: the egress gate (nftables + TPROXY on the gateway host) default-denies the inverter's egress — only DNS→resolver, NTP, and tproxied cloud channels pass; everything else drops + logs.
- **C-008**: writes need write-grade evidence for the identified device — the layout driver must have an encoding (else `write_unsupported`: unmapped / not_supported) and the device must be the exact verified profile, or covered by the installer's audited grant of untested writes bound to its exact model + firmware; failing checks are never overridable. Enforced at the gateway (`internal/device`, `internal/driver`; design `docs/knowledge/04-design/device-compatibility.md`).

## Privacy

The repository is public. Never commit anything that identifies a specific
installation:

- serial numbers (inverter, data logger, battery modules), addresses,
  installer or plant names;
- LAN addresses or hostnames of a real setup;
- packet captures, session logs or installer-portal exports.

Test fixtures and docs use placeholder serials (`60KB10EXAMPLE01`,
`609WEXAMPLE0001`, `60E5MEXAMPLE116`…) and documentation addresses. A
binary fixture taken from a capture must have its serials replaced
(same length) and its frame CRCs recomputed. The diagnostic bundle
(`internal/bundle`) does the same pseudonymisation for users.

## Architecture

One binary on the gateway host, which is the inverter's statically
configured gateway:

- The egress gate (nftables + TPROXY) default-denies the inverter's
  egress. It hands the tproxied cloud-channel flows (TCP/14431) to the
  frame-decoding cloud-channel service: the TLS inverter listener, plus
  REST/UI on :18080.
- A local resolver answers the inverter's DNS (observation only — the
  inverter dials the cloud by raw IP).
- The embedded prox serve mode carries the optional upstream leg to the
  real cloud.
- The frame service is the sole upstream→inverter path; a single gateway
  is the sole operator write path.

## prox package

The frame service is built on the published `github.com/nicois/prox`
package; do not reimplement TPROXY/TLS/relay plumbing.

- `go.mod` requires a pseudo-version (no release tag yet). Upgrade with
  `go get github.com/nicois/prox@<tag|commit>`.
- `internal/proxpin` reads the version from the build info.
- The conformance suite (`test/proxconformance`) pins the adoption
  requirements against that version.
- **Do not modify the prox repo.** Report missing features and design
  issues to its owner; never fix them there and never work around them in
  this design.

prox runs embedded (`cfg.DataRoot == ""`): in-process, with no filesystem
side effects. It uses consumer-owned capture rules (`rules.mode:
consumer`): the gate owns the TPROXY rules.

Known gaps, reported upstream:

- `transparentListen` binds without SO_REUSEADDR. A previous session's
  TIME_WAIT can block a restart for up to ~60 s; the app waits up to 120 s.
- There is no listener-ready signal; the app relies on prox's `listening`
  log line.

## Protocol tooling

`tools/mon150` is the frame monitor/parser for prox captures. Use it
before parsing `c2s.bin`/`s2c.bin` by hand: dual markers (7f7f/7e7e) plus
CRC validation make naive resync silently lossy.

- Live tail: `mon150 [-dur 60s]`.
- Replay: `mon150 -from HH:MM -to HH:MM [-file …]`.
- Value cross-check: `-ha <url>` with `HA_TOKEN`.

It decodes power telemetry, keepalives, registration, the fn=64 wrapper,
Modbus reads and writes with register names, per-push Δ diffs, the 48xxx
time-group tables and the device-info snapshot series. Its register
names come from `frame.RegName` (`internal/frame/modbus.go`): keep that in
sync with `docs/recon/protocol-profile.md` §5 when a register is decoded. The corpus parity test (`internal/frame/corpus_test.go`) builds
it from source.

`tools/probe-registers.sh` reads register blocks through
`POST /api/v1/registers/read` (installer token).

## Project knowledge

- **Wire protocol, known:** [`docs/recon/protocol-profile.md`](docs/recon/protocol-profile.md)
  - §3 bootstrap handshake, §4 telemetry/snapshot/wrapper classes, §5
    command and register map.
  - **Open protocol items:** profile §8.
  - Working interpretations are marked as hypotheses.
- **Constraints, decisions and tests:** the [`docs/knowledge/`](docs/knowledge/)
  tree. CLAUDE.md is a summary, not a source; on divergence the tree and
  the profile win.
- **Defects:** `docs/knowledge/07-deficiencies.md` (DEF-NNN).
- **Users:** `INSTALL.md`. **Overview:** `README.md`.
- **Reviews:** `docs/reviews/` (the 2026-11-17 project audit and install
  review).
- **prox adoption:** `docs/positions/`.
