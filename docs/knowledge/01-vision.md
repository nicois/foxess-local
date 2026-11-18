---
project: foxesslocal
level: 1
last_verified: 2026-11-04
traces_up: []
traces_down: [02-constraints.md]
---
# Vision

## Purpose

The owner runs a Foxess KH10 hybrid inverter on his home LAN. Today the inverter's state and control are mediated solely by Foxess Cloud (a proprietary framed-binary uplink over TCP/14431 — confirmed by the 2026-11-04 capture).

**Mission:** the owner has direct, permanent local authority over his own inverter — Foxess Cloud is never again the sole path to inverter state or commands, and remains an opt-in upstream (forwarding), not a dependency.

## Product shape

A single Go 1.27 binary (`foxess-local`) on **the gateway host** (the inverter's statically configured gateway; all inverter egress transits it) that:

- sits behind an **egress gate** (nftables + tproxy, host-level): default-deny on the inverter's egress, cloud-channel flows tproxied to the app, everything else dropped + logged (C-007),
- impersonates the cloud's TCP/14431 binary service to the inverter (terminates the uplink TLS on the gate's handoff listener; frame decode + bootstrap handshake),
- applies policy to every message, both directions (the forward-mode ladder + maintenance window decide the upstream direction — deny-all except `full` and an active window; the command gateway validates operator writes — C-001/C-002/C-003),
- serves a self-owned REST API (`/api/v1`) consumed by the owner's foxess-control HACS integration in Home Assistant, plus a basic web UI,
- optionally forwards to the real cloud (modes: `off` / `telemetry_only` / `full`).

## Users

- **The owner** — single technical operator. Maintains the HA instance and the foxess-control integration; runs the binary on one of his always-on Linux servers.
- **Home Assistant (via foxess-control)** — primary control and telemetry surface; dashboards and automations target integration entities, so backend swaps are transparent.
- **Foxess Cloud** — opt-in upstream, not a user.

## Success criteria

1. **Cloud cut.** The cloud is cut completely; HA remains fully functional (telemetry + control); switching back is one config change to this system. Proven by actually cutting the cloud once during rollout.
2. **Cloud write protection.** Cloud-initiated inverter modification (firmware included) flows only in `full` mode or during an active maintenance window — both explicit installer-gated audited opt-ins; deny-all otherwise (C-001/C-002; the 2026-11-13 redesign replaced the per-message firmware hold).

## Strategic direction

### SD-1: embedded broker [RETIRED]
Prefer embedding MochiMQTT in the single Go binary. Escape: if the inverter shows exotic broker requirements MochiMQTT cannot satisfy, fall back to mosquitto + a thin sidecar — a config change, not a redesign. The escape defers to P-001 (local control must survive any broker swap).
**[RETIRED 2026-11-04]**: the 2026-11-04 capture (OQ-1) showed the inverter does not speak MQTT — the uplink is a proprietary framed binary over TCP/14431. The broker-facing question this SD answered no longer exists; the MochiMQTT listener is demoted to an optional LAN surface (D-014).

### SD-2: HA surface
Prefer HA control via the owner's foxess-control HACS integration against this system's REST API, over MQTT auto-discovery. Escape: if foxess-control stops supporting a local backend, fall back to MQTT-discovered entities. The escape defers to P-004.

## Non-goals

- Not a general-purpose MQTT broker product; not a multi-inverter platform (one KH10).
- Not a mimicry of the Foxess Cloud API (the REST API is self-owned; the cloud API is a moving target with quirks).
- No extra inverter hardware in v1 (the RS485/Modbus interface is a known fallback, not a plan).
- No Home Assistant add-on packaging (the owner maintains his own HACS integration).
- MQTT auto-discovery in v1 (post-v1, per SD-2).

## Priorities

Strictly ordered; lower ID wins when goals conflict.

### P-001: Local control retained
**Statement**: the inverter is fully operable — telemetry visible, commands accepted — while the cloud is unreachable, and every path that can write to the inverter is owned by this system.
**Rationale**: this is the mission; everything else is convenience. It beats fidelity, cloud compatibility, simplicity, and velocity when they conflict.
**Enforced by**: C-001, C-002, C-003, C-007
**Served by**: D-002, D-005, D-006, D-009, D-011, D-012, D-013, D-014, D-015, D-016

### P-002: Inverter telemetry fidelity
**Statement**: the local telemetry view is at least as complete as what the cloud sees — no inverter-reported field is silently dropped.
**Rationale**: control decisions (SoC floors, schedules, maintenance overrides) are only as good as the state they act on.
**Enforced by**: (none yet — see DEF-002)
**Served by**: D-012, D-014, D-017

### P-003: Cloud compatibility / forwarding
**Statement**: with forwarding enabled, the real cloud stays in a working state (`telemetry_only` / `full`), so switching to or from the cloud loses nothing the cloud provided.
**Rationale**: the cloud is a feature, not a dependency; the owner keeps vendor support and cloud dashboards as options.
**Enforced by**: (none — UNCONSTRAINED; loud-failure, acceptable)
**Served by**: D-009, D-016
**Trade note**: D-016 (maintenance window) deliberately opens P-003 for a bounded period — the window restores full cloud compatibility (firmware pushes included) for a time-boxed, installer-gated, audited stretch; the trade is the inverse of the retired D-010 (see the trade-off audit in `05-coverage.md`).

### P-004: Operational simplicity
**Statement**: one binary, config-driven, nothing to babysit — deployment, cert switching, and posture changes are config changes plus restart.
**Rationale**: a local system that needs constant attention loses to the cloud on the one axis the owner does not care about.
**Enforced by**: C-005, C-006
**Served by**: D-001, D-003, D-004, D-007, D-008, D-018, D-019

### P-005: Velocity
**Statement**: each rollout stage is short and reversible, so cutover happens and the next stage starts within days, not weeks.
**Rationale**: the value is realized only after cutover; stage gates (D-011) pace velocity without sacrificing P-001.
**Enforced by**: C-004
**Served by**: (workflow discipline — red-first TDD, virtual-time tests)

## Trade-off rules

- Lower ID wins. Every D-NNN cites the priority it serves and, if it sacrifices anything, a strictly lower-priority goal.
- P-001 is never traded for P-003…P-005: e.g. upstream deny-all by default (C-001/C-002) beats cloud compatibility; the only deliberate openings are `full` mode and the maintenance window — both installer-gated, audited, and time-boxed where they can be (D-016).
- P-002 beats P-003: local state completeness beats forwarding convenience.
- P-004/P-005 are pacing priorities: they lose to P-001…P-003, and decide between themselves (e.g. 5-year cert validity [P-004] over a 90-day cert [P-005]).
