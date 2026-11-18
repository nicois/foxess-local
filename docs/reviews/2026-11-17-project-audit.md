# foxesslocal — Project Audit & Roadmap

Date: 2026-11-17 · Branch `dashboard-redesign` @ `f2a045d`

## Summary

foxesslocal is a carefully engineered core with thin edges: the command path, policy engine and tests are strong, while the supply chain, the gate's failure modes and operator-facing alerting are where the risk sits. The audit found 5 High, 19 Medium and 14 Low findings across 38 items, and none of them is a flaw in the write path to the inverter.

The five that matter most:

- **The project builds on one machine only (M1).** A local-path `replace` in `go.mod` means CI cannot pass, so lint, race and vulnerability checks depend on one workstation.
- **Dependencies are unscanned (S1).** Two networking and crypto modules are two years old and the scanner cannot currently run.
- **The egress gate fails open (S2, S3).** It covers IPv4 by source address only, and the inverter is unfiltered at boot until the gate unit runs.
- **A known shutdown panic is still open (R1).**
- **The owner is not told when something breaks (U1, U2).** Home Assistant is frozen since cutover, and alerts exist only inside an open browser tab.

The roadmap proposes twelve features. The first four (HA local backend, notifications, health and metrics, backup) are small, and together they turn a working system into one that can be left alone.

## Scope and method

This is a read-only audit of the `dashboard-redesign` branch at commit `f2a045d`: source, deploy scripts, CI workflows and docs. Nothing was changed in the repository and nothing was run against the live host (the gateway host) or the inverter.

| Check | Result |
| --- | --- |
| `go build ./...` | passes |
| `go vet ./...` | passes |
| `go test ./...` | passes, 30 packages (results were cached) |
| `golangci-lint run ./...` | 0 issues |
| `govulncheck ./...` | did not run: the installed binary is built with Go 1.26 and cannot load Go 1.27 sources |

Not checked: the live nftables ruleset, the installed systemd units and config on the gateway host, the prox package internals (out of bounds per project rules), and the foxess-control HACS integration.

Findings carry a severity: **High** = fix before wider use, **Medium** = fix this quarter, **Low** = tidy when nearby.

## Security

The write path is well defended: one gateway, role-checked routes, range validation, a per-setting write budget and an audit trail. The weaker points are the transport around it, the gate's blind spots, and an unverified dependency set.

| ID | Severity | Finding | Evidence | Recommendation |
| --- | --- | --- | --- | --- |
| S1 | High | Dependency vulnerabilities are unchecked. `golang.org/x/net v0.33.0` and `golang.org/x/crypto v0.31.0` date from December 2024, and no scanner runs locally or in CI. From memory, both have published advisories since; reachability is unknown. | `go.mod`; `govulncheck` fails on Go 1.27 sources; `ci.yml` runs `go test` only | Rebuild `govulncheck` with Go 1.27, run it, bump both modules, add it to CI. |
| S2 | Medium | The gate filters IPv4 only and identifies the inverter by source IP alone. An IPv6 address or a changed IPv4 address leaves the inverter's egress unfiltered. | `deploy/gate.nft` (`table ip`, `ip saddr`) | Match on the dongle's MAC (`ether saddr`) and add an `inet`/`ip6` drop for it. Confirm on the gateway host that the dongle has no IPv6 address. |
| S3 | Medium | The gate fails open. Before `foxess-gate.service` runs at boot, and whenever `apply-gate.sh` fails, the inverter reaches the cloud directly. The detector runs every 5 minutes. | `foxess-gate.service` (after `network-online`), DEF-003 | Load a static drop rule for the inverter early (`nftables.service` or `Before=network-pre.target`), then let the gate add the tproxy path. Finish the reboot drill. |
| S4 | Medium | Bearer tokens travel over plain HTTP, and `?token=` is accepted on every endpoint, not just the event stream. Query-string tokens end up in browser history, proxy logs and shell history. | `internal/api/server.go` `authenticate`; no TLS listener in `run.go` | Accept `?token=` on `GET /events` only (the UI already sends the header). Offer an optional TLS listener, or document a reverse proxy. |
| S5 | Medium | A compromised app can persist. The service user owns the binary directory, the previous binary and the upgrade marker, and holds `CAP_NET_ADMIN`. The upgrade guard also runs as that user. | `foxess-local.service`, `upgrade-guard.sh`, D-023, DEF-006 | Known trade-off for rootless upgrades. Add `CapabilityBoundingSet=CAP_NET_ADMIN` now; DEF-006 (socket activation) removes the capability entirely. |
| S6 | Medium | Unauthenticated read access is on by default and the event stream has no subscriber cap. Any LAN device can read live energy data and open unlimited SSE connections. There is no `Host` header check, so a hostile web page could read the public endpoints through DNS rebinding. | `publicAdmits`, `Bus.Connect`, no `r.Host` check in `internal/api` or `internal/guard` | Cap concurrent SSE subscribers. Reject requests whose `Host` is not an IP literal or a configured name. Consider defaulting `public_read_only` to off for new installs. |
| S7 | Medium | The simulator is a second write surface with weaker authentication: one static API key from the config file, compared with `!=`, and a predictable default web token (`sim-token-<stationID>`). It is off by default. | `internal/sim/sim.go` lines 199, 234 | Keep it off in production configs. Require an explicit web token, use constant-time comparison, and log a startup warning when enabled. |
| S8 | Low | The systemd unit uses a small part of the available sandboxing. | `foxess-local.service` has `ProtectSystem`, `ProtectHome`, `PrivateTmp`, `NoNewPrivileges` only | Add `ProtectKernelTunables`, `ProtectKernelModules`, `ProtectControlGroups`, `RestrictAddressFamilies`, `RestrictNamespaces`, `LockPersonality`, `SystemCallFilter=@system-service`, `PrivateDevices`. |
| S9 | Low | GitHub Actions are pinned to tags, not commit SHAs. The image workflow has `packages: write`. | `.github/workflows/*.yml` (each file notes this itself) | Pin to SHAs before the first public release. |
| S10 | Low | Gate drop rules log without a rate limit, and NTP (UDP 123) is allowed to any destination. | `gate.nft` | Add `limit rate 10/minute` to the log rules. Restrict NTP to named servers once the inverter's NTP use is observed. |
| S11 | Low | Internal error text is returned to API clients, tokens never expire, `last_used` is lost on restart, and failed logins are not logged. | `Server.internal`, `tokens.go` | Log the error and return a generic detail. Add optional expiry, persist `last_used` periodically, and log 401s with the client address. |

Things that are done well and should stay: signed releases verified against a key built into the binary, with a self-test and automatic rollback; a strict CSP with no inline script; tokens stored as hashes with 256 bits of entropy; the token kept in `sessionStorage`; unknown config keys failing startup; `pprof` restricted to loopback.

## Reliability and correctness

The core is careful about ordering and time: shutdown is sequenced, time-driven logic is tested under virtual time, and writes are read back. The gaps are around detecting a sick process and protecting the data it holds.

| ID | Severity | Finding | Evidence | Recommendation |
| --- | --- | --- | --- | --- |
| R1 | High | A shutdown panic is still open. If the inverter registers within about 4 seconds of a stop, the bootstrap emitter writes an audit row after the store closes and the process panics. | DEF-008; `store.go` line 218 panics on any `Record*` after `Close` | Cancel the emitter's context in `handle.Shutdown` before `st.Close()`, with the red test DEF-008 describes. Consider making late writes a logged drop instead of a panic. |
| R2 | Medium | `/healthz` always answers 200. It does not reflect the inverter listener, the store's last write error, or data freshness, so an external monitor cannot see a degraded service. | `handleHealth` returns `{"status":"ok"}` unconditionally | Keep `/healthz` as liveness and add a readiness view (listener bound, store healthy, last frame age, gate state). |
| R3 | Medium | A hung process is not detected. The unit has no watchdog, and there is no readiness signal for the TPROXY listener. A broken config restarts every 5 seconds forever with no alert beyond the journal. | `foxess-local.service` (`Restart=always`, `StartLimitIntervalSec=0`); pending prox report on listener-ready | Add `Type=notify` with `WatchdogSec` once the listener-ready signal exists. Until then, have the gate-check timer also alert on a restart loop. |
| R4 | Medium | The store uses one SQLite connection for both the write flusher and API queries. A slow history or message query delays flushing, and a full write queue blocks the frame hook. | `internal/store/store.go` header comment, `maxConns = 1`, `batchCap = 4096` | Open a second read-only connection for API queries (WAL already allows it), or put a timeout on query handlers. |
| R5 | Medium | There is no backup or export of the database, tokens or certificates. A dead SD card loses a year of history and every token. | No backup command among the nine CLI subcommands; nothing in README or INSTALL | Add `foxess-local backup <dir>` using `VACUUM INTO`, plus a documented restore. |
| R6 | Medium | In `full` mode a cloud write can be mistaken for the acknowledgement of a local command, or make a good local write report failure. | DEF-005, accepted residual risk | Leave accepted while `full` mode is rare. Implement write-plan matching before recommending `full` mode to other users. |
| R7 | Low | The HTTP server sets only a header-read timeout. Idle and slow-body connections are held open indefinitely. | `run.go` `http.Server{ReadHeaderTimeout: ...}` | Add `IdleTimeout` and `ReadTimeout`. Leave `WriteTimeout` unset for the event stream. |
| R8 | Low | Scheduler windows are evaluated in the host's local time zone. A host with the wrong zone, or a daylight-saving change, shifts force-charge windows relative to the inverter's own clock. | `internal/forward/scheduler.go` line 194 uses `now.Location()` | Show the effective zone in the UI and compare host time with the inverter RTC (register 0x9C40) on each session. |
| R9 | Low | Token file writes are atomic but not synced, and certificate material is written as two separate files. A power cut can leave either incomplete. The code already repairs the certificate case. | `tokens.go` `persistLocked`, `run.go` `smallKeyMaterial` | `fsync` the temp file and directory before rename. |

## Maintainability

Test discipline is the project's strongest asset: 124 test files against 100 source files, 80 to 100 % statement coverage in every internal package, and zero lint findings. The main risk is that the project builds on exactly one machine.

| ID | Severity | Finding | Evidence | Recommendation |
| --- | --- | --- | --- | --- |
| M1 | High | The build depends on an absolute local path. No other machine and no CI runner can build or test the project, so the three workflows have never been able to pass. | `go.mod`: `replace github.com/nicois/prox => <prox checkout>` | Publish prox v0.1.0 and drop the `replace`, as already planned. If that is weeks away, vendor the pinned tree so CI can run in the meantime. |
| M2 | Medium | Three different prox revisions are named as the pin, and nothing ties them together. The version string every stored row carries uses the oldest. | `run.go` `proxPin = "971d648"`; conformance suite `8d68f0e`; CLAUDE.md and the worktree HEAD `a93e083` | Derive the stamp from the build (`debug.ReadBuildInfo`) once prox is a real module version. Until then, one constant with a test that compares it to the worktree. |
| M3 | Medium | CI runs `go test` only. Lint, vet, the race detector and vulnerability scanning run on the owner's machine or not at all. | `ci.yml` | Add `golangci-lint`, `go test -race`, and `govulncheck` jobs. |
| M4 | Medium | The web UI is one 2,388-line JavaScript file with no tests, no linter and no type checking. It is the fastest-changing part of the product: four of the last five commits are UI work. | `internal/web/static/js/app.js` (102 KB); Go tests cover only serving and headers | Split by page into ES modules (no bundler needed). Add a small headless smoke test against `tools/devui`. |
| M5 | Medium | Plan-1 leftovers are still tracked and would mislead a new reader. The root `Dockerfile` cannot build (it runs `mkdir` in a shell-less distroless image and cannot see the prox path); `docker-compose.yml` publishes MQTT ports the app no longer serves. | `Dockerfile`, `docker-compose.yml`, `configs/`; the real container files are in `deploy/container/` | Delete the root container files and `configs/dnsmasq.conf`. Decide whether `internal/broker` and `certlab` stay; they are the only reason two MQTT libraries ship in the binary. |
| M6 | Medium | Defaults specific to the owner's installation are compiled in. Every other user gets a 41.93 kWh battery capacity unless they notice the setting. | `config.DefaultBatteryCapacityWh = 41930`; `defaultLeafIP` in `run.go` | Derive capacity from the snapshot's module count, or leave it unset and have the UI ask. |
| M7 | Low | `cmd/foxess-local` is the least-tested package (55 %) and `run.go` (981 lines) mixes process wiring, certificate bootstrapping and event mapping. | Coverage run; `run.go` | Move certificate bootstrap into `internal/certgen` and the bus adapters into their own file. |
| M8 | Low | Live gate scripts still describe themselves as a pre-draft pending an inventory that was completed. The `[broker] cert_manifest` key names a component that is no longer wired. | `gate.nft`, `apply-gate.sh`, `check-gate.sh` headers; `config.go` | Update the headers. Rename the key to `[certs] manifest` with the old name as a retired alias. |
| M9 | Low | Status is recorded in several places that already disagree in small ways. CLAUDE.md alone carries a dated status, the task list and the established facts. There are 199 `nolint` directives. | CLAUDE.md vs `docs/knowledge/`; `grep nolint` | Keep CLAUDE.md to stable instructions and link to the tree for status. Review `nolint` uses by linter once. |
| M10 | Low | The protocol decoder used for all recon lives under `docs/` as a 1,339-line `main` package, with register names duplicated against the profile. | `docs/recon/tools/mon150/main.go` | Move it to `tools/mon150` and have it import the register names from `internal/frame`. |

## Usability and operability

Installation is in good shape: two files, a signed release, an empty config that works, and an installer that prints the first token. What the product lacks is a way to tell its owner something is wrong when no browser tab is open.

| ID | Severity | Finding | Evidence | Recommendation |
| --- | --- | --- | --- | --- |
| U1 | High | Home Assistant shows frozen values on the live system. HA is still fed by the cloud, and the cloud has been cut off since 15 November. | CLAUDE.md status; the local backend for foxess-control is the stated next goal | Ship the HA local backend next. It is the only item that restores something the owner had before cutover. |
| U2 | High | Alerts reach nobody unless the web UI is open. Offline transitions, failed commands, disk pressure and scheduler faults are published only as server-sent events; gate rule loss goes only to the journal. | `api.SystemAlert` publishers in `run.go`; `check-gate.sh` uses `logger` | Add outbound notification (see roadmap F2). This is also what DEF-001 needs to be more than a badge. |
| U3 | Medium | Two unrelated things are called a schedule. The device's eight time groups are edited through the API and UI; force-charge windows live only in the config file and need a restart. | `PUT /api/v1/schedule` vs `[[scheduler.windows]]` | Move force-charge windows into the store and the UI, under a distinct name. |
| U4 | Medium | Diagnosing a problem means reading a 332-line runbook and running `nft`, `ss`, `conntrack` and `journalctl` by hand. | `docs/recon/runbook.md`; `installer.sh check` covers prerequisites only | Add `foxess-local doctor`: gate rules present, listener bound, inverter session age, last frame, store size, clock skew, release status. |
| U5 | Medium | The battery capacity default is wrong for anyone but the owner, and nothing prompts for it. Usable-energy figures are silently off. | See M6 | First-run prompt in the UI, or derive it from the module table. |
| U6 | Low | A 67-character token must be pasted again in every new browser session, which is awkward on a phone. | Token kept in `sessionStorage` | Offer an opt-in "remember on this device" for read-only and standard roles, or a QR code on the token reveal dialog. |
| U7 | Low | Operator documentation and research notes are interleaved. The README is 459 lines; the cutover log, protocol profile and runbook sit beside it as peers. | `README.md`, `docs/recon/` | Keep README to what the product is and how to run it. Promote the stable parts of the runbook to an operations guide. |
| U8 | Low | Only one device and firmware is verified for writes. Other models are read-only on community register maps. | INSTALL.md; C-008 | Correct as a safety stance. It becomes a growth limit; see roadmap F8. |

## Roadmap: features worth adding

Twelve features, ordered by value to the owner first and to other users second. Effort is a rough size: S is days, M is one to two weeks, L is longer or blocked on something external.

| ID | Feature | What it gives | Builds on | Effort |
| --- | --- | --- | --- | --- |
| F1 | Home Assistant local backend | HA works again without the cloud: live sensors over SSE, controls through the gateway. An MQTT discovery publisher is a second route that needs no custom integration. | `/api/v1/state`, `/events`; the dormant `internal/broker` | M |
| F2 | Outbound notifications | Webhook, ntfy or HA event when the inverter goes offline, the gate loses rules, a command fails, the write budget is exhausted, disk is low, or an upgrade rolls back. | The existing `system` event bus; `check-gate.sh` | S |
| F3 | Readiness, metrics and `doctor` | A Prometheus `/metrics` endpoint, a real readiness check, and a one-command diagnosis. Closes R2, R3 and U4 together. | `internal/liveness`, store counters | S |
| F4 | Backup and restore | One command to snapshot history, tokens and certificates; a scheduled option; a documented restore. Closes R5. | SQLite `VACUUM INTO` | S |
| F5 | Tariff-aware charging | Force-charge and export windows chosen from time-of-use rates or a spot-price feed, with a solar forecast, instead of fixed clock times. This is where local control starts saving money. | `forward.Scheduler`, the gateway's write budget | L |
| F6 | Energy and cost accounting | Daily and monthly kWh for import, export, solar, load and battery; self-consumption and autarky ratios; cost at a configured tariff; CSV export. | 15-minute rollups (`internal/store/rollup.go`) | M |
| F7 | Battery health tracking | Per-module trends from the snapshot series, and measured capacity over time from full charge cycles. Early warning of a weak module on a nine-module pack. | `snap61` module table, SoC and stored-charge counter | M |
| F8 | Device verification pipeline | A guided capture-and-submit flow so other owners can contribute traces, and a path from a tentative map to a verified write profile. This is how the supported-device list grows. | `GET /api/v1/bundle`, opt-in chunk capture, `internal/compat` | L |
| F9 | Firmware and cloud-push control | Show what the cloud tried to send, hold firmware pushes for explicit approval, and record firmware version changes. Makes `telemetry_only` and maintenance windows safe to use routinely. | Policy engine, audit log, C-002 | M |
| F10 | Zero-capability service | Socket activation removes `CAP_NET_ADMIN` and makes restarts rootless. Closes S5 and DEF-006. | Blocked on the prox listener-injection seam | M |
| F11 | Dynamic export limiting | Automatic export curtailment on negative prices or a network operator signal, within the write budget. | Limits registers 41021/41023; F5's price feed | M |
| F12 | Setup wizard and optional TLS | First-run page for battery capacity, time zone, forward mode and access posture; a built-in TLS listener with a self-signed or supplied certificate. Closes U5 and part of S4. | `internal/certgen`, config | M |

Validating `telemetry_only` on the live system belongs beside F1. It is the cheapest way to keep the vendor app and warranty telemetry working, and it is still an untested experiment (runbook 1b.3).

Not recommended yet: multi-inverter support. The gate, the gateway's single in-flight slot and the state model all assume one device, and nothing in the current evidence asks for more.

## Prioritised actions

**First, alongside the stage-1b close-out (days):**

1. Rebuild `govulncheck` for Go 1.27, run it, and bump `x/net` and `x/crypto` (S1).
2. Fix the DEF-008 shutdown panic (R1).
3. Restrict `?token=` to the event stream and cap SSE subscribers (S4, S6).
4. Harden the gate: MAC match, IPv6 drop, early static drop rule, log rate limit; then run the reboot drill (S2, S3, S10).
5. Delete the plan-1 container files and correct the gate script headers (M5, M8).

**Next (this quarter):**

1. Home Assistant local backend, and the `telemetry_only` experiment (U1, F1).
2. Notifications, readiness, metrics and `doctor` (U2, F2, F3).
3. Publish prox, drop the `replace`, and make CI run lint, race and vulnerability checks (M1, M2, M3).
4. Backup and restore (R5, F4).
5. A read-only store connection for API queries (R4).
6. Split `app.js` and add a UI smoke test (M4).

**Later:**

1. Energy accounting, then tariff-aware charging and export limiting (F6, F5, F11).
2. Battery health tracking (F7).
3. Zero-capability service once prox exposes the listener seam (F10, S5).
4. Setup wizard, optional TLS, and removal of owner-specific defaults (F12, M6, U5).
5. Device verification pipeline and firmware-push control, before promoting the project to other owners (F8, F9, R6).
