---
project: foxesslocal
level: 4
feature: architecture
last_verified: 2026-11-13
traces_up: [02-constraints.md, 03-architecture.md]
traces_down: [05-coverage.md, 06-tests.md]
---
# Design: architecture

Decisions ratified in the design spec (`docs/superpowers/specs/2026-11-02-foxess-kh10-local-control-design.md`, approved 2026-11-02) and the plan-1 header.

## Design decisions

### D-001: Single Go binary embedding MochiMQTT
**[MQTT PART RETIRED 2026-11-18]**: the embedded MochiMQTT broker was removed (owner): the inverter does not speak MQTT (OQ-1) and the service never started it. The single-binary decision stands.

**Decision**: one binary (`foxess-local`) embeds MochiMQTT v2 as a library; no separate broker process.
**Context**: interception requires seeing/authenticating every MQTT message; the owner wants one deployable with no extra services.
**Rationale**: hooks are exactly the interception points; one process means one config, one health surface, one upgrade.
**Priority served**: P-004
**Trades against**: P-005 (glue code around MochiMQTT vs. configuring a battle-tested standalone broker) — 4 > 5 ✓
**Classification**: safety (enforces C-006)
**Alternatives considered**:
- mosquitto + sidecar (external auth/plugin): rejected as the default because the sidecar is an extra process to run and the interception points are weaker (plugins, not hooks); retained as the SD-1 escape.
- proxy in front of a real broker: rejected — two TLS stacks, double the failure surface.
**Traces**: C-006; plan 1 task 4.
**[REFRAMED 2026-11-04]**: the single-binary decision stands (C-006); the 'embeds MochiMQTT' half no longer applies inverter-facing — OQ-1 resolved to a proprietary framed binary (D-014); the broker is an optional LAN surface.

### D-002: Deny-all broker ACL + single command gateway
**[BROKER HALF RETIRED 2026-11-18]**: the broker ACL went with the broker. The single command gateway (C-003) stands; the inverter-facing structural deny is D-014.

**Decision**: MochiMQTT runs deny-all by default; the only allow rules are the inverter's topic grants and (plan 2) the gateway's publishing identity. Operator commands publish through one gateway that validates payload shape and clamps ranges before publish.
**Context**: structural default-deny for C-001/C-003 — policy as access control, not as scattered checks.
**Rationale**: the broker enforces the negative case for us; a misconfigured policy engine degrades to "no commands get through", never "anything gets through".
**Priority served**: P-001
**Trades against**: none (pure enforcement)
**Classification**: safety (enforces C-001, C-003)
**Alternatives considered**:
- allow-all broker + policy checks in one hook: rejected — the broker would structurally permit direct publishes (e.g. a stolen `lan` credential) that the hook layer must then catch; the negative case lives in code, not structure.
**Traces**: C-001, C-003; plan 1 task 4 — `internal/broker::TestBrokerRejectsUnknownUser`, `internal/broker::TestBrokerAllowsInverterPublishAndLanRead`.
**[REFRAMED 2026-11-04]**: the deny-all broker ACL remains the structural deny for the *optional LAN MQTT surface*; the inverter-facing structural deny moves to D-014 (the frame service is the sole upstream→inverter path). C-001/C-003 invariants unchanged.

### D-003: HA control via foxess-control → REST
**Decision**: the Home Assistant surface is the owner's foxess-control HACS integration pointed at this system's REST API; MQTT auto-discovery is post-v1.
**Context**: the owner already maintains foxess-control; dashboards and automations target integration entities.
**Rationale**: dashboards/automations never see a backend swap — switching inverter uplink (local vs direct cloud) is a config flip in this system, not an edit of every HA object.
**Priority served**: P-004
**Trades against**: P-005 (foxess-control needs a local-backend adapter; MQTT discovery would be quicker to stand up) — 4 > 5 ✓
**Classification**: pacing
**Alternatives considered**:
- MQTT auto-discovery entities: rejected for v1 — dashboards/automations would bind to raw MQTT entities, so a backend swap touches every HA object; also adds a second, parallel control surface (SD-2).
**Traces**: P-004; plan 2 (REST API + foxess-control local-backend notes).

### D-004: Self-owned REST API (`/api/v1`)
**Decision**: the REST API is designed from local requirements (inverter state, commands, scheduler, forward status) — it does not mimic the Foxess Cloud API's endpoints, names, or quirks.
**Context**: the cloud API is a moving target; a mimic couples the local system to cloud behaviour changes.
**Rationale**: the API is the contract between this system and foxess-control/web UI; owning it end-to-end keeps it stable and boring (C-005).
**Priority served**: P-004
**Trades against**: none (P-003 is unaffected — forwarding to the real cloud is orthogonal to the local API's shape; the hypothetical loss of third-party cloud-API tools is not a P-003 concern because the owner runs no such tools).
**Classification**: safety (enforces C-005)
**Alternatives considered**:
- cloud-API mimic for drop-in compatibility: rejected per C-005 — a moving target with quirks not worth replicating (owner decision, 2026-11-02).
**Traces**: C-005; plan 2 API design.

### D-012: Protocol profile from observation before codec
**Decision**: plan 1 ships a JSONL capture hook and a `protocol-profile.md` template; the plan-2 codec and allowlist are built from the filled profile, not from guesswork or forum lore.
**Context**: the KH10 wire protocol (OQ-1: MQTT vs proprietary TCP; OQ-4: payload schemas) is not documented; the plan was split 1/2 for exactly this reason. A third-party prior now exists: open-source `foxess_local_gateway` (same vendor's M1/Q1 microinverter family) documents the frame format (7e/7f framing, CRC16-Modbus-LE), a ~238-byte telemetry layout, a fault-code table, cloud hostnames (`foxesscloud.com`, `*.maitian-yun.com`), and self-signed-cloud-cert acceptance for that family; it is not verified on the KH10, so capture remains the source of truth (plan task 5's profile template carries these as §0 prior expectations).
**Rationale**: a codec written against observed payloads is verifiable (round-trip against captured frames); a guessed codec encodes forum claims as invariants.
**Priority served**: P-002 (complete field set observed), P-001 (allowlist built on facts)
**Trades against**: P-005 (recon stage delays codec work) — 2 > 5, 1 > 5 ✓
**Classification**: pacing
**Alternatives considered**:
- codec first from forum register maps (e.g. RS485 Modbus registers as a proxy): rejected — forum lore is unverified for the KH10's MQTT payloads and would be re-worked at recon; the register map remains a cross-check input to the profile.
**Traces**: P-002, P-001; plan 1 tasks 5–6 (`internal/profile::TestCaptureRecordsSessionLifecycle`, `docs/recon/protocol-profile.md`).

### D-013: Gate-primary egress (default-deny + tproxy + local resolver)
**Decision**: the gateway host (always-on Linux box; Docker: `foxess-local` + `dnsmasq`) is the inverter's statically configured gateway — all inverter egress transits it. The egress gate (nftables + tproxy), scoped to the inverter's source IP, default-denies new flows; exceptions are DNS → local resolver, NTP, and tproxied cloud-channel ports (real destination preserved) handed to the frame service's listener (D-014). The resolver answers names (no override — name-scoped interception is redundant once flow-level capture exists); the DNS override is the documented fallback for a lost vantage point only.
**Context**: owner confirmed (2026-11-04) that the inverter is statically network-configured with the gateway host as its gateway (DHCP off). The gate makes C-001/C-002 airtight at the network layer (no uncontrolled channel — C-007) and decouples interception from the protocol choice (OQ-1).
**Rationale**: interception scoped by *device* (src IP) rather than by *name* (DNS) — immune to hard-coded IP dialing, DNS bypass, and new regional endpoints; the inverter needs zero configuration changes; rollback is one rule removal (runbook).
**Priority served**: P-001
**Trades against**: P-005 (vantage-point dependence — if the inverter ever leaves the gateway host, interception dies; the DNS-override fallback covers that case) — 1 > 5 ✓
**Classification**: safety (enforces C-007; reinforces C-001/C-002 at the flow layer)
**Alternatives considered**:
- DNS override as the primary mechanism (the pre-2026-11-04 design): rejected — name-scoped; a hard-coded IP dial or DNS bypass walks around it, it couples to mesh-side config (eero custom-upstream + rebinding exception), and the override may not be reachable for the inverter's dial style at all.
- Broker/frame-proxy only, no gate (intercept where our listener sits): rejected — the uncontrolled channel persists (inverter → real cloud directly), so C-002 could not be airtight from cutover.
- decoding the cloud channel itself: deferred to plan 2 (the D-014 frame service); the gate deploys at cutover (stage 1), by which point the frame service is in place — interim capture ran through the gateway host's prox, not the gate.
**Traces**: C-007, C-001, C-002 (flow layer); runbook 1a/1b (deploy + verification); spec §4/§6 (2026-11-04 revision).

### D-014: Inverter-facing service is a frame-decoding cloud-channel proxy
**Decision**: the service behind the gate's tproxy handoff is a frame-decoding proxy for the KH10's proprietary cloud protocol — TCP+TLS (T2 anchor), 7e7e/7f7f framing with CRC-16, three-exchange bootstrap handshake reproduction, and per-frame hooks for capture, policy, and forwarding. It is the sole upstream→inverter path. The plan-1 MochiMQTT broker is demoted to an optional LAN surface (keep/delete: plan 2).
**Context**: OQ-1 resolved by the 2026-11-04 prox capture (`docs/recon/protocol-profile.md`): the KH10 uplink is a proprietary framed binary over TCP/14431 (SNI=IP, no DNS name; 427+12 frames, 0 bytes residual), not MQTT. The spec's OQ-1 demotion path activated as written; prox is the working reference implementation. The protocol is fully characterised in the profile (bootstrap, telemetry classes, command envelope, firmware mechanism).
**Rationale**: interception must happen on the channel the inverter actually uses; a broker cannot speak this protocol. The frame proxy reproduces observed bytes verbatim (the inverter does not validate the prior's func-offset arithmetic — byte fidelity is the contract), and per-frame hooks give policy/capture/forward the same interception points the MQTT hooks would have.
**Priority served**: P-001 (enables C-001/C-002 on the real channel), P-002 (every frame is observable → field-set completeness is measurable, DEF-002)
**Trades against**: P-005 (frame service + codec is net-new build work vs the shipped broker) — 1 > 5, 2 > 5 ✓
**Classification**: safety (enforces C-001 at the message level; C-002's in-band half)
**Alternatives considered**:
- keep speaking MQTT to the inverter via translation: rejected — the inverter does not speak MQTT; there is nothing to translate.
- run prox (the interim MITM) as the permanent service: rejected — C-006 (one binary, pure Go, owner-operated) and policy/forward/API integration require the in-house service; prox remains the reference + interim.
**Traces**: C-001, C-002 (message level), C-003 (the frame service is the sole upstream→inverter path), P-002; `docs/recon/protocol-profile.md` (the byte-level spec); plan 2 (`internal/frame`, `internal/codec`); supersedes the inverter-facing halves of D-001/D-002 (both [REFRAMED]).

### D-015: Gateway process model — colocated, socket-activated
**Decision**: the `foxess-local` process runs on the gateway host (colocated with the gate) and receives the tproxy handoff port via **systemd socket activation** — a `ListenStream=<port>;Transparent=true` unit creates the TPROXY socket; the app inherits it and runs with **zero capabilities** (interim: `AmbientCapabilities=CAP_NET_ADMIN` until the prox listener-injection seam lands). The TPROXY redirection component stays a separate, root-owned surface: the gate ruleset plus the socket unit; it "only needs to know the address and port to redirect to."
**Context**: owner ratified 2026-11-13 after walking the two candidate forms. The **remote plain-relay form** (a root-owned plain relay on the gateway host forwarding bytes to an app plain-listening on any LAN host) was rejected: a plain relay stream breaks the TPROXY tuple contract — prox recovers the upstream destination via `getsockname` on the transparent child (`isTransparent` is getsockopt-only), so the relayed flow looks like "relay-IP:ephemeral-port" and the ladder would dial *that* as the cloud. Fixing it needs PROXY-protocol parsing in prox (a larger seam than listener injection) or an endpoint override (silently wrong when the inverter dials a failover IP). Note: this is not an upstream-fidelity argument — the ladder dials upstream from a **fresh socket** (default `Ladder.Dial`, `net.DialTimeout`; no production custom Dial), so the cloud already sees the gateway host's src IP today; a remote app host would only move that src IP. The plain-relay form's remaining costs stand on their own: +1 hop, +1 root unit, client identity in the store becomes the relay IP, and app-host availability joins the uplink failure domain (app host down = blackholed uplink). The flexibility gained ("the process lives on the dev box") is not needed: the app's user surface (web UI, REST, journal) is LAN-reachable from any client — deployment location ≠ operator location — and the single-inverter/fixed-gateway deployment has no second site. Colocation of the Go service with the TPROXY surface is acceptable given the app's small footprint (owner, 2026-11-13).
**Rationale**: socket activation achieves both stated goals — no root/caps for foxess-local, and a clear separation where redirection is its own root surface — with the smallest prox change (listener injection: `Prox` accepts an external `net.Listener` instead of creating one in `transparentListen`), and no new correctness surface: the activated socket carries `IP_TRANSPARENT` (systemd), accepted children inherit it, and every existing prox path (original-destination recovery, leaf selection, hooks) is unchanged.
**Priority served**: P-001 (the app holds no capability that touches the firewall — C-007's "one rule" in the README)
**Trades against**: P-005 (the app cannot run off-gateway; if that need appears, the remote plain-relay form with PROXY protocol is the recorded shape to revisit) — 1 > 5 ✓
**Classification**: safety (enforces C-007 at the process layer; removes the app's CAP_NET_ADMIN)
**Alternatives considered**:
- remote plain-relay / app-anywhere: rejected (context above).
- status quo (app binds the TPROXY socket itself, interim `CAP_NET_ADMIN`): retained only as the interim until the prox seam lands; the capability is a standing C-007 soft-spot (the app could rewrite the gate — the 5-minute detector is the compensating control).
**Traces**: C-007, C-006; README install steps (interim unit, socket-activation target), open items (the prox listener-injection seam — report to the prox repo, never worked around here); supersedes nothing (D-013/D-014 unchanged — same gate, same frame service, new socket ownership).

### D-017: Retention measured from stream close; chunk byte capture is an opt-in development knob
**Decision**: all connection-scoped retention (telemetry, frames, chunks, the connections row) is measured from **when the stream stopped being written to** (connection close) — never from the row's `ts`. A keep-alive session can run for days; its live rows survive the raw window no matter how old. The periodic maintenance pass rolls up buckets, clears captured chunk payload bytes at their own horizon, then reaps finished connections whole. At store open, rows left by a crashed session — whose connections row never came, since it is written only at stream end — are synthesized as finished connections with close = the restart moment, so the reaper reaches them instead of leaking the session's rows. The `chunks.payload` column (nullable) carries bytes only when `[retention] chunk_bytes_hours > 0` — the knob defaults to `0` (off), sets `EnableBytes()` before the first stream opens (the registry decides per stream, G2), and is a development / trace-contribution surface (raw traces for other inverter models and firmware versions). Byte lifetime is `min(chunk_bytes_hours, raw_hours)` — the row dies with the connection reap; the metadata row lives out the full raw window.
**Context**: owner ratification 2026-11-13, two coupled asks: (1) the trace-contribution knob — capture raw chunk bytes, default off, retention period configurable, zero = off; (2) the retention fix — the first implementation pruned connection rows by row `ts`, which deletes the 10-day-old rows of a 10-day-old **open** session the moment a maintenance pass runs; retention must run from when the file stopped being written to.
**Rationale**: close is the true end-of-write; row `ts` is a meaningless age for long-lived sessions. One reaper with one cutoff (raw window, clamped by the rollup watermark — never delete what the rollup has not fully covered, never delete while no buckets exist) keeps the failure mode symmetric across all connection-scoped tables and preserves the "raw is never deleted before the rollup covers it" posture. The byte horizon runs from close for the same reason: a row-ts horizon would null trace bytes mid-session.
**Priority served**: P-002 (live session data is never destroyed — telemetry fidelity over the session's full lifetime)
**Trades against**: P-005 (one knob, one nullable BLOB column, one query) — small, and the off-by-default keeps production byte volume at zero
**Classification**: pacing
**Alternatives considered**:
- per-table close-based pruning with independent cutoffs: rejected — one reaper, one cutoff keeps the watermark gate uniform and the pass report explainable.
- row-ts horizon for the bytes (simpler SQL): rejected — it nulls mid-session on long streams, the same flaw this decision fixes.
- a separate trace-capture subsystem (own storage, own retention): rejected — chunks are already per-connection records; a nullable column plus a horizon keeps one data path (C-006: one binary, boring design).
**Traces**: C-006 (one data path: the nullable column in the existing store, no parallel capture subsystem); config `[retention] chunk_bytes_hours` (0 = off); `internal/store` (`Maintain` reaper, `chunks.payload`, `ChunkPayloadCount`, the restart-orphan synthesis at open); conformance R6a (the EnableBytes-before-open gate); supersedes the row-ts prune semantics (the `Retain` API removed — no production callers).

### D-018: every connections row is stamped with the version of the binary that persisted it
**Decision**: `connections.version` (schema v3: `ALTER TABLE … ADD COLUMN version TEXT NOT NULL DEFAULT 'unknown'`) holds the **composed version stamp of the process that opened the store** — `appVersion() = "<build tag>-prox-<conformance pin>"` (the pin: `internal/proxpin.Short`, tied to go.mod's replace target by a test — audit M2), e.g. `0cc02f3-prox-a93e083`. The same string appears in the `/api/v1` server info and the startup log, so store rows, API, and journal agree. The build tag is set at build time via `-ldflags "-X main.version=$(git rev-parse --short HEAD)"`; unstamped builds read `dev-prox-…`. Rows written before the upgrade read `unknown`; restart-synthesized connections carry the restarting binary's version (it is the process that wrote the row).
**Context**: owner's question at the cutover checkpoint (2026-11-13): is there sufficient versioning so logs and data can be associated with a release/commit? Answer: logs yes (startup line + API info), stored data **no** — no row carried a version, and the default build tag was `dev`.
**Rationale**: a connection is written by exactly one process run (TPROXY flows die with the process), and every frame/chunk/telemetry row hangs off `conn_id` — so one stamp on the connections row attributes a whole session's stored data to a build. Post-cutover bug triage: `SELECT version FROM connections WHERE id = …` answers "which binary recorded this session?".
**Priority served**: P-004 (operational simplicity — stored data is attributable to a build; cutover debugging is tractable)
**Trades against**: P-005 (one column, one ALTER, one parameter through `Open`/`openStore`)
**Classification**: pacing
**Alternatives considered**:
- a schema meta table carrying the stamp: rejected — the stamp belongs on the *data* (the session), not the database; rows and their stamp are written by the same process anyway.
- per-table version columns: rejected — every row hangs off `conn_id`; the one column on `connections` covers all of them.
- deriving the version from file mtimes or journal correlation: rejected — indirect, and it breaks if the store file is copied or restored.
**Traces**: README build step (the ldflags stamp — the shipped binary must be stamped); `internal/store` (schemaV3, `migrate`, `applyConn`, the restart synthesis), `cmd/foxess-local` (`appVersion`, the startup log, the API `ServerInfo`); supersedes nothing.

### D-019: the app self-bootstraps its T2 anchor on first start — no cert files in the install flow
**Decision**: no certificate material is staged or generated at install time. The eMudhra G3C intermediate's **public** certificate is embedded in the binary (`//go:embed` in `internal/emudhra`); on first `run`, when the `[broker] cert_manifest` path is missing, the app scans the embedded cert against the small-key set, derives the issuer key with `KeyFromSmallK(k)` (k=4 for G3C), mints the leaf (SANs from `[identity]`) via `certgen.Generate`, and caches `cert.pem` / `key.pem` / `cert-manifest.json` in the standard location (the `cert_manifest` directory, 0700/0600) before continuing. Later starts reuse the cache. The `certgen` CLI remains for explicit re-mints (delete the manifest; the app re-bootstraps on the next start) and the other anchors (T1 selfsigned, T3/T4 own CA).
**Context**: owner's live-cutover instruction (2026-11-13): "no certificates should be needed at all to be generated by the install step. anything required by the app can be generated lazily on demand, and if necessary cached in the standard location." The prior flow needed the G3C issuer PEM staged as a file plus a manual `certgen` run before the app would start (`caMaterial` refused with "run certgen first"). The same work fixed a latent bug the bootstrap exposed: `buildSmallKeyMaterial` passed the issuer block to `x509.ParseCertificate` as PEM instead of DER (the T2 startup path was never test-exercised).
**Rationale**: the small-key design already stores no CA key material — the key is a small scalar in a known eight-element set, and the cert is a public CA file. Once the cert is embedded, nothing the first `certgen` run did is left that the app cannot do itself at first start. The install flow shrinks to stage binary + config, mint the token, enable. The cache in the standard location keeps the app inspectable, re-mintable via the CLI, and offline-operational.
**Priority served**: P-004 (operational simplicity — zero cert files in the deploy; first start needs only the config)
**Trades against**: P-005 (a 1.7 KB public cert embedded in the binary; one bootstrap code path)
**Classification**: pacing
**Alternatives considered**:
- stage the G3C PEM as a file + manual `certgen` before enabling: rejected by the owner (the instruction above); it also left an "app refuses to start" state between install and mint that failed live mid-cutover.
- acquire/derive the issuer from the inverter's traffic: rejected — the cert is public and stable; embedding is simpler than runtime acquisition.
- a config knob for the bootstrap anchor: rejected — T2 is the deployed default; other anchors stay an explicit CLI operation.
**Traces**: install flow (standalone `installer.sh`, generated by `make installer` from `deploy/` — no cert staging; config seeded from the embedded `config.example.toml`; 0700 data tree), `internal/emudhra` (embedded cert + `G3CCert`), `cmd/foxess-local` (`bootstrapDefaultCA`, the `caMaterial` not-exist branch, the DER-parse fix), README component 2 step 4 (rewritten: automatic), `config.example.toml` (`cert_manifest` comment); supersedes the README's manual certgen step and the `testdata/emudhra/em-ov-tls-ca-g3c.pem` staging reference.

### D-028: The SoC between whole-percent readings, from the battery power
**Context**: the inverter reports the SoC in whole percent (31024). 1 % of the 41.93 kWh pack is about 420 Wh, so a night's 500 W discharge holds one value for close to an hour. The battery forecast (`internal/socalert`) fitted a line to 30 minutes of those readings: flat (no warning), or a single step read as a steep fall (2–3 %/h for a real 1 %/h). On 2026-11-20 the SoC fell from 48 % to 40 % overnight and only battery-low was sent (owner).
**Decision** (owner, 2026-11-20): as the foxess-control integration does, `internal/socest` integrates the battery power (trapezoid, `battery.capacity_wh`) between readings and holds the result within the reported tick's rounding bucket [raw − 0.5, raw + 0.494], so the reported value corrects the integration's drift (losses, a power offset) at every tick. The top is the largest two-decimal value that rounds back to the tick (foxess-control's 0.44 is the same bound for its one-decimal display). Readings over a minute apart are not integrated across. Without the capacity there is no estimate.
- **Learnt scale** (owner, 2026-11-20): two consecutive single-step ticks in one direction are exactly 1 % apart, whether the inverter rounds or truncates; the energy between them is the energy per percent. It is learnt per direction (a running mean, then exponential with weight 0.2), from clean intervals only: no gap, both steps in the energy's direction, at most 10 % of the energy against it, and a size implying a factor within 0.5–2 (else a BMS recalibration). From 3 intervals it scales the integration (nominal ÷ learnt, held within 0.7–1.3). It is stored in Wh per percent (a capacity change does not void it) under the store key `socest.learned`, saved off the telemetry hook. It corrects a pack that has lost health, a wrong `capacity_wh` and the direction's losses. Adjusting the factor at each clamp was rejected: a clamp depends on the rounding rule (unknown — profile §5) and on where in the bucket the estimate started.
- `battery.measured_capacity` (`/state`) and `foxess_battery_measured_capacity_wh{direction}` expose it (× 100; null per direction until learnt). It includes the losses between the measurement and the cells, so it is a trend, not a state-of-health figure.
- The API carries both: `battery.soc_pct` (reported) and `battery.soc_estimate_pct` (two decimals; null without one) in `/state` and the snapshot event; `/metrics` exports `foxess_battery_soc_estimate_percent`. `stored_energy_wh` follows the estimate.
- The battery forecast fits the estimate. battery-low and the re-arming stay on the reported SoC (what the inverter shows).
- The dashboard shows the estimate with two decimals.
**Rejected**: the stored-energy Wh as the forecast's input — it was SoC × capacity, no finer; the snap61 `1903` (0.25 %) and `1907` counters — about every 5 minutes, irregular, `1907`'s unit undetermined (profile §5).
**Traces**: `internal/socest::TestIntegratesBetweenTicks`, `TestTrapezoid`, `TestClampsToTick`, `TestGapAndClock`, `TestNoCapacity`, `TestLatest`, `TestLearnsScale`, `TestLearnsPerDirection`, `TestLearnRejects`, `TestRestoreAndBounds`; `cmd/foxess-local::TestSocLearnedPersists`; `internal/socalert::TestForecastSlowDischarge`; `internal/api::TestStateSocEstimate`; `cmd/foxess-local::TestSocWatchEstimate`.

## Key behaviours

- The plan-1 broker users (`inverter`, `lan`) are **placeholders for the optional LAN surface only** — the inverter never authenticates to MQTT (it does not speak MQTT; D-014). The inverter's identities for the frame service come from the bootstrap registration exchange (profile §3: gateway/Wi-Fi module `609WEXAMPLE0001`, inverter `K1210460KB10EXAMPLE01`, module `60KB10EXAMPLE01`).
- The **frame service's** listener binds to the port the gate's tproxy hands off to (TLS, T2 anchor per the manifest); the REST API is `:18080`; plain `:1883` (optional broker) is for LAN users.
- **OQ-3 resolved (2026-11-04, owner-verified)**: the inverter statically network-configures — the gateway host is its gateway, DHCP off. **OQ-1 resolved (2026-11-04 capture)**: proprietary framed binary over TCP/14431 — the decoupled demotion path activated (D-014); the frame-decoding service sits behind the same gate (prox is the working reference) — interception, policy, API, and rollout were unchanged, exactly as designed. **OQ-2 resolved**: T2 accepted (see `tls-trust.md`).

## Edge cases

- Inverter reconnect behaviour after a the gateway host restart: unobserved yet (the profile has one continuous session) — the frame service must reproduce the full bootstrap handshake on every new session (plan 2). Gate rules must be re-applied after a the gateway host restart (DEF-003) — otherwise the egress default flips to allow.
- `certlab` early-drop detection: the hold check distinguishes "session died before the hold window" from "hold completed" (plan 1 task 4).
