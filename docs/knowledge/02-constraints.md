---
project: foxesslocal
level: 2
last_verified: 2026-11-16
traces_up: [01-vision.md]
traces_down: [03-architecture.md, 04-design/]
---
# Constraints

Invariants that must hold across all implementations. C-001…C-006 were ratified by the owner at the init interview (2026-11-02); C-007 was ratified with the gate-primary architecture revision (2026-11-04); C-008 was ratified with the device-compatibility design (2026-11-16); C-001/C-002 were reworded to the mode-level form by the 2026-11-13 policy redesign (owner ratified); C-001 was amended 2026-11-17 to pass read-only requests in `telemetry_only` (owner, D-024).

## Safety constraints

### C-001: Upstream deny-all except explicit open modes and read-only requests
**Kind**: state
**Statement**: no upstream (cloud→inverter) message flows outside `full` forward mode and an active maintenance window (both explicit, installer-gated, audited), with one exception: in `telemetry_only`, a recognised request that cannot change the inverter — a well-formed Modbus read (unit 1, fn03/fn04, 1–125 registers, exactly six bytes) — passes, and its answer goes back to the cloud (amended 2026-11-17, owner: "allow the cloud to continue to receive responses to its requests as long as they do not modify inverter state"; D-024). Everything else is deny-all (logged + dropped); in `off` there is no cloud leg. Answers to this app's own requests never go to the cloud.
**Priority enforced**: P-001
**Rationale**: the inverter's uplink is a trust boundary; once it sits between owner and inverter, the cloud is an untrusted third party. A mode-level deny-all with two explicit audited opt-ins replaces per-message allowlisting (2026-11-13 redesign): the operator's decision is one knob, and every pass-through state is one the installer deliberately opened that is logged and audited.
**Violation consequence**: an unvetted cloud command (work-mode change, reboot, firmware update) reaches the inverter — local authority silently bypassed.
**Traces**: D-014 (inverter-facing structural half: the frame service is the sole upstream→inverter path), D-016 (the maintenance-window opt-in), D-013 (flow-layer complement: the gate default-denies the inverter's egress, C-007), D-002 (the six live `internal/broker` ACL tests guard the optional LAN surface only), `internal/policy` (mode-ladder decide tests, red-first).
**Red-proof**: plan 2 policy tests (`internal/policy/policy_test.go`) were written red-first: the `telemetry_only`/`off` deny and `full` pass assertions failed before the `Decide` mode ladder landed (C-004; SDD ledger 2026-11-13).

### C-002: Cloud-initiated inverter modification only in full or maintenance
**Kind**: state
**Statement**: cloud-initiated inverter modification flows only in `full` mode or during an active maintenance window, and firmware only during a maintenance window: `full` passes only the frame kinds the cloud is known to send (session control, Modbus reads and writes, the serial query) and holds every other kind — a firmware push among them — until a window is open (owner, 2026-11-18; D-025). In `off`/`telemetry_only` no cloud write ever reaches the inverter.
**Priority enforced**: P-001
**Rationale**: a firmware update is the highest-impact cloud action and the most likely to arrive unexpectedly. The 2026-11-13 redesign made "blocked by default" structural: `full` mode and the maintenance window are both deliberate, installer-gated, audited states (the window is time-boxed and re-armable), and every other state deny-all. Since the gate-primary revision (2026-11-04) this is two-layered: the gate (D-013) drops out-of-band firmware/OTA flows at the network layer; the mode ladder covers the in-band cloud-push case over the permitted cloud channel (TCP/14431 binary frames).
**Violation consequence**: the inverter reboots into new firmware mid-rollout; the local system is now validated against a version it did not choose.
**Traces**: D-016 (the window opt-in), D-010 (retired — the hold it enforced), D-013 (flow layer: firmware/OTA flows drop + log at the gate), `internal/policy` + `internal/forward` (red-first: the deny assertions and the synctest window tests).
**Red-proof**: plan 2 policy/forward tests (2026-11-13, red-first per C-004): `TestTelemetryOnlyDeniesEveryKind`, `TestFullPassesEveryKind`, the `TestOpen*`/`TestExpiry*` window suite; the flow-layer half is verified at stage 1 by runbook 1a/1b (1a: dropped flows appear in the gate log; 1b: the 14431 flow terminates on our listener).

### C-003: Single command gateway
**Kind**: state
**Statement**: every operator-originated command (HA, web UI, scheduler) passes one gateway that validates payload shape and clamps ranges before the command is sent to the inverter; the service architecture makes every other upstream→inverter write path structurally impossible (the frame service is the sole upstream path; the optional LAN MQTT surface is deny-all and does not reach the inverter).
**Priority enforced**: P-001
**Rationale**: a command is a write to physical hardware. Scattered validation rots; one chokepoint plus a structural deny keeps the invariant verifiable at one point, not by code review alone.
**Violation consequence**: a bad or unvalidated command reaches the inverter — out-of-range values misconfigure the hardware; unknown shapes produce undefined inverter behaviour.
**Traces**: D-002 (plan 1 ACL structure — optional LAN surface), D-014 (sole upstream path), plan 2 gateway (validation/clamp tests, red-first); the ack bookkeeping's full-mode residual risk is DEF-005.

### C-007: Default-deny inverter egress (network layer)
**Kind**: state
**Statement**: no new flow from the inverter's source IP may reach a destination outside the LAN unless it matches the egress gate's allowlist (DNS → local resolver, NTP, tproxied cloud-channel ports); firmware/OTA and all other flows are dropped and logged.
**Priority enforced**: P-001
**Rationale**: while the inverter has any uncontrolled channel to the cloud, C-001/C-002 are only half-enforced — a message-level policy cannot cover flows that never reach the app. The gate removes the channel itself, which is what makes "blocked by default" airtight rather than policy-dependent.
**Violation consequence**: inverter telemetry or a firmware/OTA pull reaches the real cloud without policy; the local system is nominally in the path and actually bypassed — and (if the rules are lost, DEF-003) this can happen silently.
**Traces**: D-013; runbook 1a (gate deploy: default-deny + tproxy scoped to the inverter src IP) / 1b (swap + verification: the inverter's 14431 flow terminates on our frame service's listener, other flows appear as logged drops); plan 2 (rule persistence + non-local-dest flow detection, DEF-003 — runbook 1a.2 from day one); DEF-006 (rootless upgrades: socket activation per D-015 — the prox listener-injection seam is the blocker).
**Red-proof**: host-level enforcement — no unit-test surface; the falsifying check is runbook 1a/1b verification (n/a before then).

### C-008: Writes need write-grade evidence for the identified device
**Kind**: state
**Statement**: no operator, scheduler or simulator write reaches an inverter register unless the identified device's layout driver has an encoding for that feature and the device's confidence for the feature allows it: *exact* (a verified profile: model and every firmware version match) with a verified encoding, or — **amended 2026-11-16 (owner)** — an installer grant of untested writes. The grant is a persisted, audited setting bound to the exact model and firmware it was granted for; any change of either stops it covering the device. It never enables an impossible write (no encoding: *unmapped*; the device lacks the feature: *not supported*) and never overrides a failing plausibility check. Each refusal is a distinct outcome (`write_unsupported` + reason, `write_unverified`, `device_not_identified`, `checks_failing`). Reads are not restricted by C-008.
**Priority enforced**: P-001
**Rationale**: every register meaning was proven on one device (KH10 fw 1.73/1.03/1.71). A plausible-looking third-party map was wrong for a write on that same device: foxess_modbus's "max SoC" at 41010 is max SoC from grid. A range-clamped write to the wrong register still misconfigures the hardware, so C-003's range checks are necessary but not sufficient.
**Violation consequence**: a setting written to a register that means something else on this model or firmware — the wrong limit, SoC bound or mode changed on someone's inverter, silently.
**Traces**: D-020 (confidence levels, identification, checks, the no-op write), D-021 (foxess_modbus mappings are tentative: never write-grade), D-022 (guided-session bundles as write-grade evidence), C-003 (the gateway is where the gate is enforced), `internal/identity` (identification + the verified-profile table, 622f38c).
**Red-proof**: `internal/device::TestAuthorize` (every outcome), `internal/gateway::TestAuthorizerGatesWrites` (writes gated before anything is queued or sent; reads never), `internal/device::TestGatewayAuthorizer` and `internal/api::TestWriteGateOutcomes` / `TestWriteOverride` (store-backed gate, grant bound to the exact firmware), written red-first (C-004); the gate is wired in production (`cmd/foxess-local/run.go`).

## Workflow constraints

### C-004: Red-first TDD, virtual time
**Kind**: state
**Statement**: every behaviour ships with a test that failed before the implementation landed (red-first); all time-driven logic (scheduler windows, retention expiry, forward-queue validity windows) is tested with the stdlib `synctest` virtual time, never wall-clock sleeps (real-time waits permitted only for connection-readiness polling).
**Priority enforced**: P-005
**Rationale**: post-hoc tests lock in implementation accidents; wall-clock tests are slow and flaky. Red-first plus virtual time is what keeps the stage cadence sustainable across many small stages.
**Violation consequence**: timing bugs (scheduler windows, retention expiry) surface in production; stage gates slow down because tests are trusted less.
**Traces**: plan 1 tasks 1–5 (each step: failing test written and run before implementation); plan 2 `synctest` suites.

### C-005: Self-owned REST API
**Kind**: state
**Statement**: the REST API under `/api/v1` is generic and self-designed; it does not copy the Foxess Cloud API's shapes, names, or quirks.
**Priority enforced**: P-004
**Rationale**: the cloud API is a moving target with quirks. Mimicking it couples every local change to cloud behaviour and imports its accidents; the owner maintains the HA-side integration, so the mapping is ours to own end-to-end.
**Violation consequence**: cloud API changes break the local API surface (and the foxess-control backend mapping) for no local benefit.
**Traces**: D-004 (plan 2 API design + tests).

### C-006: Go 1.27, single binary, pure-Go dependencies
**Kind**: state
**Statement**: the system is one Go 1.27 binary with a pure-Go dependency set (no cgo, no sidecar processes).
**Priority enforced**: P-004
**Rationale**: owner directive. One statically linked binary on an always-on Linux box is the zero-babysit deployment; pure Go keeps builds reproducible and the Docker image distroless-small.
**Violation consequence**: build/deploy friction returns (CGO cross-compilation, native libraries, extra processes to monitor) — operational simplicity degrades exactly where it matters.
**Traces**: D-001; `go.mod` (`go 1.27` directive); `deploy/release.sh` (static `CGO_ENABLED=0` builds) and `deploy/container/Containerfile` (the root plan-1 Dockerfile was removed 2026-11-17, audit M5).
**Scope** (owner, 2026-11-24; D-031): the constraint covers foxess-local, the system that touches the inverter. The OpenStick setup wizard (`cmd/foxess-wizard`) is appliance tooling, like `provision-openstick.sh`, and lies outside it. It is a second, root-owned binary that never speaks the inverter's protocol, never writes a register and never sits in the data path. It is built pure Go and static, like foxess-local, but may run OS programs (`nmcli`, `dnsmasq`, `systemctl`).

## Proposed

(C-008 ratified 2026-11-16 — see Safety constraints.)

(None pending — C-001…C-007 is the full ratified invariant set (C-001/C-002 reworded 2026-11-13). Graduation candidates in `07-deficiencies.md`: DEF-001 (data freshness — blocked on a live telemetry source under the current uplink form, 2026-11-16) and DEF-002 (telemetry fidelity — the field set must be defined per uplink form); DEF-003 is live and awaits only the reboot drill.)
