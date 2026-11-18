---
project: foxesslocal
level: 4
feature: posture-and-forwarding
last_verified: 2026-11-16
traces_up: [02-constraints.md, 03-architecture.md]
traces_down: [05-coverage.md, 06-tests.md]
---
# Design: posture model and cloud forwarding

## Design decisions

### D-009: 2×2 posture model; local is the default; forward mode is config
**Decision**: the system's posture is a 2×2 — inverter uplink (local via this system | direct to cloud) × integration surface (local REST | cloud) — with local uplink the default. Forwarding to the real cloud is a config setting: `off` | `telemetry_only` | `full`. Switching the inverter back to the direct cloud is a one-step revert — revert the inverter's static config to the mesh gateway, or REMOVE the gate rules (the inverter re-dials the real cloud on its next reconnect) — with no HA-side edits. The 2026-11-13 redesign adds the time-boxed maintenance window (D-016) — an override, not a fourth mode: while it is active the channel is an unfettered pass-through, and the configured mode is what is back in force on expiry or close.
**Context**: the end state the owner wants: local by default, cloud opt-in (spec "end-state C"). Posture B (cloud live through you) must be reachable as a config change, not a redesign.
**Rationale**: making posture a first-class, config-shaped axis is what keeps every switch (local↔cloud, forward on/off) cheap and reversible — the owner maintains foxess-control, so the HA surface must not move when the backend does.
**Priority served**: P-001 (local is the floor), P-003 (cloud stays a working option)
**Trades against**: none (both served priorities are protected, not sacrificed)
**Classification**: pacing
**Alternatives considered**:
- cloud-primary with local as a fallback proxy: rejected — it inverts the mission (P-001): the cloud would remain the default path and local control the exception.
**Traces**: P-001, P-003; plan 2 (prox serve mode as the forward path — the separate forward client was retired; `internal/forward` holds the mode state, maintenance window and scheduler).

### D-010: Firmware updates held from cutover, in every forward mode [RETIRED]
**[RETIRED 2026-11-13]**: the redesign replaces the per-message hold with the mode ladder + maintenance window (D-016): cloud-initiated modification (firmware included) flows only in `full` mode or an active maintenance window — both installer-gated and audited. The hold buffer, the `/held` API, and the release/discard machinery were removed; C-002 was reworded accordingly.
**Decision**: from the moment of stage-1 cutover, firmware-update messages from the cloud are held (buffered) in **all** forward modes — including `full`. Release or discard is an explicit owner action.
**Context**: the default-allow window (before the policy allowlist is filled from recon) is exactly when an unexpected cloud push is most dangerous; a held update survives the window.
**Rationale**: firmware is the highest-impact cloud action; "blocked by default from day one" (owner-approved advisory, 2026-11-02) avoids a first-month window where the cloud could update the inverter under the local system's nose.
**Priority served**: P-001
**Trades against**: P-003 (`full` mode users expecting the cloud to update the inverter as normal) — 1 > 3 ✓
**Classification**: safety (enforces C-002)
**Alternatives considered**:
- hold only in `off`/`telemetry_only`, allow updates in `full`: rejected — the window is unbounded (who knows when the cloud pushes) and the owner chose explicit-release over convenience.
- apply-and-notify: rejected — a firmware change is hard to reverse; notify-after is the wrong order.
**Traces**: C-002; plan 2 policy engine (firmware-update rule, red-first).

### D-016: Maintenance window — time-boxed unfettered pass-through (installer-gated)
**Decision**: `POST /api/v1/maintenance` (installer) opens a time-boxed window during which the cloud channel is an unfettered bidirectional pass-through over whatever forward mode is configured — the cloud may do anything upstream, firmware pushes included; `DELETE` closes it immediately. 30 min default; a per-call cap of 4 h; a `POST` while active re-arms the deadline. The window is process-local — it does not survive a restart. Every transition is audited (kind `maintenance`) and emitted as a `system` event (`maintenance-opened` / `maintenance-extended` / `maintenance-expired` / `maintenance-closed`).
**Context**: the owner needs a bounded, deliberate way to let the cloud (or vendor tooling) talk to the inverter — e.g. an installer-requested firmware push — without standing `full` mode up; and the retired hold (D-010) left the question "the cloud should do X right now" without an answer.
**Rationale**: a time-boxed, installer-gated, audited override is the minimum-trust form of "let the cloud through": it self-closes, names who opened it, and leaves the persistent mode untouched. It subsumes the hold's release/discard question — there is nothing to release; the window is the release, bounded in time.
**Priority served**: P-001 (local stays the floor — the window is explicit, bounded, and audited), with a deliberate bounded P-003 opening
**Trades against**: P-003 in the *opposite* direction — the window restores full cloud compatibility for its duration; 1 > 3 ✓ by explicitness, time-boxing, and audit
**Classification**: safety (enforces C-001/C-002 as the audited opt-in)
**Alternatives considered**:
- keep the hold + release machinery and add a release API: rejected — two state machines for one question (may the cloud write?), and an indefinitely-buffered message is a liability the owner no longer accepts.
- a fourth persistent forward mode (e.g. `open`): rejected — "unfettered for N minutes" is not a posture; making it config-shaped would invite leaving it on.
**Traces**: C-001, C-002; `internal/forward/maintenance.go` (red-first; `synctest` expiry), `internal/api` handlers + tests, `api/openapi.yaml` (`/maintenance` ops, `MaintenanceState` schema).

### D-011: Staged rollout 0–3 with go/no-go gates
**Decision**: rollout is staged — 0 recon (egress + DNS capture at the gateway host, TLS chain, payloads; fill the protocol profile; anchor triage T1→T5), 1 cutover (gate live — default-deny + tproxy — and local frame service live; firmware/OTA flows drop + log at the gate), 2 local control (policy engine live, REST + HA live), 3 optional forwarding (`full` where wanted — forward mode has egress semantics at the gate: `off` = no cloud leg at all; `telemetry_only` = channel allowed, inverter→cloud relayed, upstream denied except read requests (D-024); `full` = both directions, the cloud's known frame kinds only — a firmware push is held until a maintenance window (D-025, 2026-11-18); the maintenance window overrides per D-016). Each gate has explicit go/no-go criteria and a documented rollback (gate-rule removal or inverter config revert → the real cloud).
**Context**: the cutover is the highest-risk event in the whole project; the rollback must always be one step (remove the gate rules).
**Rationale**: each stage's gate makes the next stage's preconditions *checked* rather than *assumed*; rollback stays trivial because the inverter's network config is the only thing that changes.
**Priority served**: P-001
**Trades against**: P-005 (stages add ceremony) — 1 > 5 ✓
**Classification**: pacing
**Alternatives considered**:
- big-bang cutover (broker + policy + API at once): rejected — failure isolation is lost; a codec bug would look like a broker bug.
**Traces**: P-001; `docs/recon/runbook.md` with go/no-go gates + rollback (rewritten 2026-11-08: gate deploy is 1a, data-path swap is 1b); plan 2 gates; DEF-001 (post-cut alerting is the open gap this rollout paces toward).

## Key behaviours

- Forwarding `telemetry_only` (frame layer): inverter→cloud passes through (minus the answers to this app's own requests); cloud→inverter frames do not, except well-formed Modbus reads (D-024, C-001). The gate layer (D-013) decides which cloud flows exist at all — `off` = none; `telemetry_only` = the channel, inverter→cloud only; `full` = both directions, the known cloud frame kinds only (D-025; the maintenance window overrides the ladder per D-016).
- The inverter's uplink reaches the **frame service** via the gate's tproxy handoff (real destination preserved, no inverter-side re-pointing ever — 2026-11-04 revision).
- The inverter's identities are observed in the clear in the **bootstrap registration exchange** the frame service terminates (profile §3: module/inverter/gateway serials) — plan 2 uses the observed identities; they are never guessed.
- **OQ-1 resolved (2026-11-04 capture)**: the proprietary-protocol case is the actual case — the frame-decoding service sits behind the *same* gate (D-014); the rest of the architecture (policy, API, UI, forward) is unchanged, exactly as the gate's decoupling promised.

## Edge cases

- Maintenance window across a restart: the window is process-local — a restart ends it (the configured mode is in force, and nothing is logged as an event since the machine is gone); a `PUT /forward-mode` during the window writes the persistent setting, effective on expiry. The UI documents both.
- Maintenance window over `off`: the serve mode is decided per connection and only `off` serves without a cloud leg, so a window transition in `off` ends the live session at once (2026-11-18): opening reconnects the inverter with a cloud leg, closing or expiry reconnects it without one (the same mechanism as a forward-mode change). In `telemetry_only` / `full` every session is mirrored already and the window acts per frame, so no reconnect; a re-arm never reconnects.
- Inverter on Wi-Fi vs Ethernet: the KH10 has both; the static config points whichever interface it uses at the gateway host (0.1 capture confirms; runbook covers both).
- Gateway host restart: the app comes back via its systemd unit (`Restart=always`); the gate re-applies via `foxess-gate.service` at boot and the `foxess-gate-check` timer detects + repairs rule loss (DEF-003) — without them the inverter's egress would be default-allow again. The reboot drill (runbook 1a.2) is still pending.

### D-024: `telemetry_only` relays the cloud's read requests; every answer goes to whoever asked

**Context**: in `telemetry_only` every cloud→inverter frame was dropped, so the cloud's polls (the 48000/48010 scheduler pair every ~30 min, the 0x9C40 clock read and the 10110 ×28 read at connect — profile §3/§5) went unanswered, and the inverter's answers to this app's own polls (every 5 s) were mirrored to a cloud that never asked. Owner (2026-11-17): "the intention is to allow the cloud to continue to receive responses to its requests as long as they do not modify inverter state. We should support recognised interactions with the cloud in telemetry mode where possible."

**Decision**:
- `policy.Decide` passes, in `telemetry_only`, exactly the well-formed Modbus read request: classified `mbread`, no response bit, a six-byte PDU for unit 1, fn03 or fn04, 1–125 registers. Every write (the cloud's RTC sync fn10 @0x9C40 included), the serial query (`7f7f 17`, purpose not established — profile §5) and anything not recognised stays denied. C-001 amended accordingly; C-002 unchanged in substance.
- The frame service routes every answer to whoever asked (`internal/framesvc/route.go`): each request reaching the inverter is recorded in order with its device tail and asker; the inverter echoes the tail in its answer (11-05 write echoes, 11-15 read answers — every frame) within ~50 ms. An answer takes the oldest request with its tail and answer shape (a read of n registers → 3 + 2n bytes; a write → its six-byte echo), else the oldest of that shape, else it is unsolicited. Only answers to this app's requests ack the gateway (before, any echo acked the in-flight command); in every mirrored mode they are cut from the cloud-bound stream.
- The live-value poller polls mirrored flows too (it skipped them while answers could not be told apart; the skip left `telemetry_only` without live data — found live 2026-11-18, fixed with this decision's routing).
- The cloud-bound (c2s) direction of a mirrored flow is rebuilt from the exact stream bytes: everything up to the first byte that could start a still-incomplete frame passes verbatim (junk included, as before); only a possible partial frame is held; the only bytes removed are answers to this app's requests (audited `forwarded` with the reason "answer to this app's request: kept from the cloud").

**Not yet relayed** (open): the serial query (whether it can change anything is not established), the cloud's bootstrap trio (the local trio is generated instead; the cloud's own initial reads now pass alongside the local trigger reads — both are harmless reads).

**Verification**: `internal/policy::TestTelemetryOnlyPassesReads`, `internal/framesvc::TestTelemetryOnlyRelaysReads` (the cloud's reads reach the inverter, its write does not; its answers reach the cloud; our command, interleaved, is acked by its own answer and never reaches the cloud), `TestRoute*`, `TestReasmSettled`. Live verification pending a `telemetry_only` session.

### D-025: Firmware needs a maintenance window, even in `full`
**Decision** (owner, 2026-11-18): `full` no longer passes everything. It passes the frame kinds the cloud is known to send — `bootstrapresp` (session control), `mbread`, `mbwrite` (configuration, schedules, the clock) and `serialquery` — and holds every other kind, logged as dropped with a reason that names the maintenance window. A firmware push can therefore reach the inverter only during a maintenance window.
**Context**: D-010's firmware hold was retired on 2026-11-13 because firmware frames were never identified on this inverter (the prior implementation's 0xA2 / 0x99 transfer envelopes were never observed). A positive firmware match is still impossible, so the rule is the reverse: an allowlist of what the cloud is known to send.
**Evidence**: every cloud-to-inverter frame in the capture corpus (2,925 streams, 2026-11-04…10) and in the owner's 2026-11-18 full-mode session is one of those four kinds (2157 bootstrapresp, 749 mbread, 178 mbwrite, 807 serialquery in the corpus).
**Trade-off**: a legitimate cloud frame of a kind not seen yet is also held in `full`. It shows in Activity / Frame messages as blocked with the hold reason; a maintenance window lets it through, and the allowlist (`internal/policy` fullKinds) can grow from that evidence.
**Traces**: C-002; `internal/policy::TestFullHoldsUnrecognised`, `internal/framesvc::TestFullModeHoldsFirmwareSized`.
