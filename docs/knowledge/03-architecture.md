---
project: foxesslocal
level: 3
last_verified: 2026-11-16
traces_up: [02-constraints.md]
traces_down: [04-design/]
---
# Architecture

## Overview

One Go binary on **the gateway host**, the inverter's statically configured gateway: every byte the inverter sends transits the **egress gate** (nftables + tproxy, host-level), which default-denies (C-007) and hands the tproxied cloud-channel flows (TCP/14431, real destination preserved) to the **frame-decoding cloud-channel service** (plan 2, D-014): it terminates the inverter's TLS on the gate's handoff listener (T2 anchor), parses the 7e7e/7f7f frame protocol, generates the bootstrap handshake locally (profile §3 — time-derived trio + the cloud's two initial reads, not a replay), and holds the sole upstream→inverter path (C-001). The inverter does not speak MQTT (OQ-1 resolved, 2026-11-04 capture); LAN clients (foxess-control via REST `:18080`, web UI) use the REST API (the plan-1 MQTT broker was removed 2026-11-18). Frame-level hooks capture every frame and (plan 2) route it through the policy engine; a local resolver (dnsmasq) answers the inverter's DNS and logs the queries (observation only — the inverter dials the cloud by raw IP, no name). Cloud forwarding is not a separate client: the embedded prox serve mode carries the upstream leg (relay in `telemetry_only`/`full`, local-only in `off`), subject to the gate's egress rules. Plan 1 delivered the skeleton plus the recon/cert toolchain (landed 2026-11-03); plan 2 adds the frame service, policy, codec, API, UI, scheduler, forward, and stores.

## Module boundaries (planned layout)

### `cmd/foxess-local`
**Responsibility**: CLI entry — `run` (the services), `config`, `tokens`, `upgrade`, `verify-release`, `certgen`, `emudhra-check`.
**Why separate**: one composition root; subcommands share `internal/config`.

### `internal/config`
**Responsibility**: TOML loader (`config.Load`, `config.DefaultPath`) with validation and defaults.
**Why separate**: every component reads the same validated config object; bad config fails at startup, not mid-cutover.

### `internal/emudhra`
**Responsibility**: small-key verifier for the eMudhra compromised intermediates (P-384 `k*G` check; small-k candidates `4, 56, 66, 115, 155, 156, 230, 237`).
**Why separate**: pure crypto logic with no I/O — independently testable against CT-log fixtures; reusable by certgen.

### `internal/certgen`
**Responsibility**: anchor/leaf generation (kinds: `selfsigned`, `small-key`, `extracted-ca`, `own-ca`) and the JSON anchor manifest.
**Why separate**: anchor selection (T1–T5) is a data-driven choice recorded in the manifest; switching anchors must be certgen + restart, not code.

### `internal/framesvc` + `internal/frame` (plan 2)
**Responsibility**: `internal/framesvc` is the inverter-facing cloud-channel service — the hook chain over the embedded prox TPROXY/TLS listener on the gate's handoff (T2 anchor): per-frame policy, capture/audit, the locally generated bootstrap (trio + initial reads, profile §3; pinned by the golden-transaction suite `internal/framesvc/testdata/transactions/`), gateway command injection, and the sole upstream→inverter path (C-001). `internal/frame` is the pure codec: 7e7e/7f7f framing (CRC-16), frame classes, payload decode, and the trio generator.
**Why separate**: framesvc *is* the interception point (D-014) and the structural half of C-001/C-003; the codec is pure bytes-in/bytes-out and corpus-testable.

The plan-1 MQTT surface (`internal/broker`, `internal/certlab`, `internal/profile`) was removed on 2026-11-18 (owner): the inverter does not speak MQTT (OQ-1), the service never started the broker, and Home Assistant integrates through the REST API (SD-2).

### Egress gate (host-level on the gateway host — not a Go module)
**Responsibility**: nftables/iptables default-deny on new flows from the inverter's src IP; tproxy of the cloud channel (TCP/14431) to the app's inverter listener; drop + log everything else.
**Why separate**: the gate is the network-layer half of C-001/C-002/C-007; it lives outside the binary's lifecycle (C-006 — one binary, pure Go), and its ruleset is a deploy artifact (runbook 1a), not code.

### Local resolver (dnsmasq, host-level on the gateway host)
**Responsibility**: the inverter's resolver (its static config points at the gateway host); forwards queries upstream and logs them (recon data + flow attribution). No name override — flow-level capture makes name-scoped interception redundant (the DNS override is the documented fallback for a lost vantage point only).
**Why separate**: observation duty + fallback insurance; replaceable by any forwarding resolver with logging.

### Plan 2 modules (implemented 2026-11-13)
`internal/framesvc` (inverter-facing frame service on the prox pipeline, D-014 — the sole upstream→inverter path), `internal/frame` (frame types + payload codec + reassembly from the observed protocol profile — cross-checked lagged against HA sensor state, see `06-tests.md`), `internal/policy` (mode-level decide: the `off`/`telemetry_only`/`full` ladder + maintenance-window flag, C-001/C-002), `internal/gateway` (single validated operator-command publisher, C-003), `internal/api` (REST `/api/v1`, C-005), `internal/web` (htmx web UI), `internal/forward` (forward-mode state, the maintenance window, and the scheduler — `synctest`-tested per C-004), `internal/store` (SQLite, pure-Go driver per C-006).

## Data flow

```
inverter ──all egress (static config: gateway = the gateway host)──> egress gate (nftables)
  gate: tproxied cloud channel TCP/14431 (real dst preserved) ──> frame service (TLS T2, frame decode, D-014)
  gate: DNS ──> local resolver (dnsmasq: upstream forward + query log; observation only — inverter dials raw IP)
  gate: firmware/unknown ──> DROP + log (C-007; C-002 flow layer)
frame service ──frame hooks──> capture + policy (plan 2)
HA (foxess-control) ──REST :18080──> api ──> gateway ──command frames──> frame service ──> inverter
web UI ──REST :18080──> same api/gateway path
frame service ──prox serve mode (off = local only | telemetry_only | full; gated by egress rules)──> real cloud
```

Key transformation points: policy engine (upstream→inverter: mode ladder + window — deny-all except `full`, an active window, and read requests in `telemetry_only` — D-024), gateway (operator→inverter: validate + clamp), codec (both directions).

## Key abstractions

### Frame pipeline hooks
**What**: per-frame hook points on the inverter-facing service (onFrame, onSession, …) — the interception points (plan 2). 
**Why**: policy, capture, and forwarding all need to see every frame without forking the service.
**Implemented by**: the frame hooks + policy/forward (`internal/framesvc`).

### Anchor manifest (JSON)
**What**: records which anchor kind produced the inverter-facing certificate (issuer, small-k if T2, SANs, validity).
**Why**: cert switching is a data change; the manifest is the audit trail of the TLS trust decision.
**Implemented by**: `certgen.WriteManifest` / `certgen.LoadManifest`.

## External dependencies

| Dependency | Role | Why chosen |
|---|---|---|
| BurntSushi/toml | config parsing | owner's config format; pure Go |
| nftables/iptables + kernel tproxy (the gateway host host) | egress gate | the C-007 enforcement layer; host-level, not a Go dependency (C-006 intact) |
| dnsmasq (the gateway host host) | local resolver | the inverter's DNS duty + query log; replaceable |
| stretchr/testify | test assertions | standard; assertion-style tests read as spec |
| SQLite (pure-Go driver, plan 2) | telemetry retention store | C-006 rules out mattn/go-sqlite3 (cgo) → modernc.org/sqlite |

## Load-bearing vs replaceable

**Load-bearing** (replacing one changes invariants, not just code):
- the egress gate (default-deny + tproxy) as the flow-layer half of C-001/C-002/C-007 — removing it reopens the uncontrolled channel;
- the frame service as the sole upstream→inverter path — the structural half of C-001/C-003 (D-014);
- single gateway as the sole operator write path (C-003);
- the REST API's self-ownership (C-005) — a cloud-mimic API would re-couple the system to cloud behaviour;
- single-binary/pure-Go shape (C-006).

**Replaceable** (genuinely replaceable given the current invariants):
- BurntSushi/toml → any TOML parser (config schema is self-owned);
- web UI framework (plan 2 — the API is the contract, the UI is a consumer);
- certgen internals → any generator producing the same manifest (the manifest, not the generator, is the interface);
- local resolver (dnsmasq) → any forwarding resolver with query logging (its only duties are answering and logging; name override is fallback-only).
