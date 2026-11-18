---
project: foxesslocal
level: 7
last_verified: 2026-11-16
traces_up: [01-vision.md, 02-constraints.md, 03-architecture.md]
traces_down: []
---
# Deficiencies

Tree-level gaps that are not yet invariant-shaped. Graduation targets: C-NNN, new P-NNN, or OQ-*.

## Open

### DEF-001: No gateway self-health or alerting
**Category**: missing-feature-area
**Statement** (reclassified 2026-11-13, owner): the owned failure mode is **no inverter data** at the frame service — "if no data has been received from the inverter in the last 15 s, something is wrong." That one symptom subsumes: gate rules failed to apply (no tproxy — the app receives no packets, the inverter is unfiltered to the real cloud), the inverter no longer routing via the gateway host at all (config drift — invisible to the gate's own detector), inverter-down, and a zombie session (established, silent). App/the gateway host fully down stays the coarse HA-staleness case (systemd `Restart=always` re-lands the app in seconds; in all of these the inverter's isolation from the cloud is the safe state — the plant runs locally). The owner-facing signal is the app's own freshness: `State.online` is server-computed data freshness (data within 15 s), not session state (contract change 2026-11-13 in `api/openapi.yaml`); the web UI (final stage) must reflect it as a persistent stale indicator; HA sees it via the `connection` event and `inverter_offline` 503s. The gate trio (DEF-003) is the operational self-repair layer + journal record, not the owner alarm.
**Why it matters**: P-004 (nothing to babysit) — staleness must be unambiguous in the product the owner already watches, not in a journal they would have to go read; and data freshness is the only signal that catches the inverter drifting off the gateway host.
**Graduation path**: contract landed 2026-11-13 (`State.online` redefined server-computed at the 15 s threshold; `connection` fires on liveness transitions; `inverter_offline` = no data within 15 s). Implementation = plan 2 frame-service stage (freshness from the 5 s telemetry, never the ~293 s bursts; `synctest` per C-004) + web-UI stage. Promote to C-NNN ("no inverter data for >15 s ⇒ the API and web UI indicate staleness unambiguously") on owner ratification. The N-minute push-channel option is retired: the product surface (API + UI + HA via SSE) is the notification.
**Progress (2026-11-14)**: implemented end-to-end — `State.online` is server-computed from the 15 s telemetry freshness (internal/api/handlers_state.go, never the ~293 s bursts), SSE `connection` fires on liveness transitions, and the web-UI badge reflects it (internal/web). What remains is the C-NNN promotion above.
**Regression (2026-11-16, cutover log §3.8/§3.9)**: the implementation keys freshness on the 150 B telemetry (`TeleTS`, internal/api/handlers_state.go), and that stream is **absent under the current cloud handshake** (profile §4 note — none since 11-11, none in any foxess-local session). Live, `online` is permanently false while the session is healthy (30 s keepalives, ~5 min bursts), so the signal is currently useless. Fix options: base freshness on the newest inverter frame of any kind with a window matched to the 30 s keepalive, and/or restore a live telemetry source (the post-1b goal: local Modbus polling). Not "implemented end-to-end" until one lands.
**Fix (2026-11-16, live-verified 15:05 AEDT — cutover log §3.10)**: the live-value poller (framesvc, KH_PRE133 registers every 5 s — profile §5) records ordinary telemetry rows, so the unchanged `TeleTS` freshness rule has a live source again in every uplink form; a dead session stops the polls and `online` goes false within 15 s as designed.
**Status**: open (fixed and live-verified; awaiting C-NNN ratification)
**Blocked on**: owner ratification of the C-NNN promotion
**Discovered**: 2026-11-02, init

### DEF-002: Telemetry fidelity is not measurable
**Category**: mission-success
**Statement**: P-002 ("the local view is at least as complete as the cloud's") has no measurement — nothing defines when the field set is "at least as complete", so a codec regression that silently drops a field would not be caught by the tree.
**Why it matters**: P-002 — an unmeasurable priority decays silently; control decisions act on the reduced state without anyone noticing.
**Graduation path**: recon landed 2026-11-04 (profile filled): the channel is the TCP/14431 binary, not MQTT — the `foxess/inv/#` wording is obsolete. Define fidelity as "every field of the profile's telemetry classes (keepalive, 6-chunk snapshot series, RSSI heartbeat, Modbus read responses) appears in the local store"; promote to a C-NNN with a codec round-trip test against `docs/recon/captures/` plus the HA-oracle cross-check (see `06-tests.md`). 2026-11-05: the field set is now concrete (150 B telemetry table + snap61 TLV map + §5 command map; residual open items in profile §8) — graduation is unblocked once the plan-2 codec lands.
**Status**: open
**Blocked on**: nothing — the codec (plan 2) and the profile's field set are both landed (2026-11-14); the next step is defining fidelity against the profile field set and graduating to the C-NNN
**Note (2026-11-16)**: the field set must be defined per uplink form — the 150 B telemetry class is absent under the current cloud handshake (profile §4 note), so a fidelity gate over it would fail vacuously or forever; the live classes today are keepalive + the snapshot series (+ Modbus read responses once the app polls).
**Discovered**: 2026-11-02, init

### DEF-003: Gate rule persistence and monitoring
**Category**: missing-feature-area
**Statement**: the gate's default-deny rules are host-level state on the gateway host. If the gateway host reboots (or the rules are otherwise lost) and the rule set is not re-applied, the inverter's egress silently reverts to **default-allow** — it reaches the real cloud unfiltered: a C-007 violation with no visible signal to the owner from the local side.
**Why it matters**: P-001 — the airtightness of "no uncontrolled channel" (C-007) depends on the rule set being live at all times; rule loss silently reopens exactly the channel the gate exists to close.
**Graduation path**: plan 2's resilience/ops design — a persisted rule set (nftables file + boot/restart re-apply, or container-managed rules) plus a detector for "inverter flow to a non-local destination". Promote to a C-007 extension or runbook gate when the owner commits to it.
**Status**: open
**Progress (2026-11-14)**: stage 1a is live on the gateway host — the `deploy/` set (`gate.nft` + `apply-gate.sh` + `check-gate.sh`, verified end-to-end in a disposable namespace before deploy) is up: default-deny + tproxy 14431 scoped to the inverter src IP, with `foxess-gate.service` (boot re-apply) and the `foxess-gate-check` timer + `RULE-LOSS` journal detector armed; role clarified by the DEF-001 reclassification — operational self-repair, not the owner-facing alarm (that is data freshness). Remaining: the reboot drill proving the boot re-apply (runbook 1a.2); the 1b data-path swap is done (2026-11-15/16, foxess-local on 14431).
**Blocked on**: the reboot drill; the rule set is live
**Discovered**: 2026-11-04, update workflow

### DEF-008: Delayed bootstrap trio cannot be quiesced before store close
**Category**: other
**Statement** (discovered 2026-11-15 during DEF-007): the delayed bootstrap trio is a producer that cannot be quiesced before store close — if a 2a registration lands within trioDelay (~4 s) of shutdown, `emitTrio` fires after `st.Close()`: the write fails (conn force-closed) → `RecordAudit after Close` → panic on stop (same class as DEF-007, not covered by `WaitStreams` — it is not a persistStream).
**Why it matters**: P-004/P-005 — a panic at stop corrupts the graceful-shutdown path (systemd restart cycle) in exactly the race window (registration within ~4 s of stop) that a restart-during-registration creates.
**Graduation path**: cancel the trio timer's ctx in `handle.Shutdown` before `st.Close()`, with a deterministic red test (registration armed within trioDelay of shutdown). DEF-007's test deliberately never arms a trio (telemetry frames, no 2a registration) because of this race. Deferred — do not fix mid-cutover (2026-11-15 owner decision).
**Narrowed (2026-11-16)**: the cancellable injection waits (`waitGap` selects on `fl.hc.Done()`) now exit emitters on flow close; the residual is the waitGap-true → `writeS2C`-fail → `RecordAudit after Close` race (the emitter is an untracked goroutine — `WaitStreams` does not cover it). The ctx-cancel in `handle.Shutdown` remains the complete fix.
**Fixed (2026-11-17, audit R1)**: the service tracks its own goroutines (the bootstrap emitters and the flow activation); `Service.Quiesce` closes every flow's quit signal — the emitters' waits and pending writes end at once — and waits for them; `handle.Shutdown` calls it before `st.Close()`. The quit signal is per flow (made in `OnOpen`): under synctest a channel made outside the bubble never counts as durably blocked. Red test: `TestQuiesceStopsEmitters`.
**Status**: fixed
**Discovered**: 2026-11-15, during DEF-007 fix

### DEF-005: full-mode cloud Modbus echoes are indistinguishable from local-command echoes
**Category**: accepted-residual-risk
**Statement**: in `full` mode (and an active installer-gated maintenance window) the cloud's own Modbus write pushes (profile §5: the cloud's read-modify-write) reach the inverter, and the inverter's echoes of those pushes arrive c2s as ordinary `mbecho` frames — same session, same shape as the echoes of locally injected commands. `ackInFlight` (internal/framesvc/service.go) accepts any `KindMbecho`/`KindCfg91` while a local command is in flight, and the gateway's write-ack match is addr+cnt only for fn10 (the echo carries no value; internal/gateway/ack.go). A cloud push overlapping a local command's in-flight window can therefore consume the ACK meant for the command (false-ack: the API reports success on a register the cloud just rewrote) or mismatch the local write (false-fail). The maintenance window is a transparent pass-through (internal/forward/maintenance.go), not an isolation mechanism.
**Why it matters**: C-003 (the gateway is the sole, validated write path) — the ack bookkeeping is what makes "the command applied" true. The hazard is bounded: the overlap window is the in-flight duration (seconds), cloud pushes are rare in the corpus, and the single in-flight slot already prevents local-local collisions — the ambiguity is local-vs-cloud only. The read phase is exact (length-correlated ×80 blocks after the F4 coalescing fix); the exposure is the write phase: a same-address fn10 cloud write false-acks the local command, and a cloud Modbus push to any register in the window false-fails it (the mismatched echo fails the in-flight match).
**Graduation path**: the code-level mitigation (ignore c2s echoes that do not match the in-flight command's write plan) was deliberately not implemented at the 2026-11-14 pre-cutover audit: the gateway's strict matching (fn06 addr+value, fn10 addr+cnt) already rejects most collisions: a **same-address** cloud write inside the in-flight window is the false-ack direction, a cloud push to **any** register in that window is the false-fail direction — both bounded by the seconds-long window and the rarity of cloud pushes. Accepted residual risk of `full` mode; if the audit log ever shows a false-ack (an `injected` mbwrite row whose register is rewritten by a cloud-originated write within the window), implement plan matching in `ackInFlight`.
**Status**: open (accepted residual risk)
**Blocked on**: none — observation item
**Discovered**: 2026-11-14, pre-cutover codebase audit

### DEF-006: Rootless upgrades — socket-activation endgame (interim CAP_NET_ADMIN)
**Category**: missing-feature-area
**Statement**: the app runs as a *system* unit (`User=foxess`) with `AmbientCapabilities=CAP_NET_ADMIN` — required for the TPROXY listener (the kernel's TPROXY delivery only sees sockets bound with `IP_TRANSPARENT`; prox's `transparentListen` sets it unconditionally). Consequence: user systemd + linger is out (the user manager cannot grant ambient capabilities; a file-caps workaround is void — a binary replacement wipes the caps), and the unit's restart is root-gated. The 2026-11-14 installer rework removed the per-upgrade root *staging*: `make all` builds the stamped binary + the standalone idempotent `installer.sh` (two files are the entire install surface — the gateway never needs a repo checkout), and the binary lives in foxess-owned `/var/lib/foxess-local/bin/` — most upgrades are a drop-in copy (no root) + `systemctl restart`. Root remains for the one-shot installer (the gate is inherently root) and for the restart until the capability is gone.
**Why it matters**: the frequently-upgraded part (the app) should not need root in its upgrade path — an upgrade-path bug is a root-context failure. Not a P-001/C-007 gap: the inverter's isolation and the gate itself are unaffected.
**Graduation path**: D-015 (owner-ratified 2026-11-13) — systemd socket activation: a `ListenStream=14431; Transparent=true` unit creates the TPROXY socket (systemd, as root, at socket creation); the app inherits it and runs with **zero capabilities** (the TPROXY gate mechanism is unchanged). The blocker is prox's listener-injection seam: prox creates its own socket in `transparentListen` and must instead accept the inherited fd / an external `net.Listener` (owner request to the prox repo — reported, not fixed there, per the repo rule). After it lands: drop `AmbientCapabilities` from the unit and add the `.socket` unit to the installer's embedded set. Sequenced post-1b.
**Status**: open
**Blocked on**: prox listener-injection seam (owner request)
**Discovered**: 2026-11-14, owner question (upgrade/reinstall without root)

## Graduated

### DEF-009 [GRADUATED]: Inverter-listener bind failure leaves the service UP but degraded
**Category**: missing-feature-area
**Statement** (observed live 2026-11-15 22:24 during the stage-1b cutover): an inverter-listener bind failure (EADDRINUSE — the tproxied inverter session's kernel socket, still live in FIN_WAIT 59 ms after the local FIN, blocked the `0.0.0.0:14431` bind; prox's `transparentListen` binds without SO_REUSEADDR, so FIN_WAIT and TIME_WAIT both block it, up to ~60 s on the gateway host (re-corrected 2026-11-16)) leaves the service UP but degraded: HTTP/REST alive, inverter leg silently dead, the journal is the only trace.
**Why it matters**: P-004 (nothing to babysit) — a degraded-but-UP state is invisible to the owner and to systemd; the data-freshness signal (DEF-001) would catch it only if the freshness watch runs, and the owner would see "no data" without knowing the listener never bound.
**Graduation path**: startup should fail loud (non-zero exit → systemd retries) or the healthz/state surface must report the dead leg. Interim: runbook 1b.2 carries the drain wait (~60–90 s, tcp_fin_timeout bound) + `ss` verification before restart.
**Status**: graduated (2026-11-16, owner chose fail-loud with a wait)
**Blocked on**: none
**Discovered**: 2026-11-15, live observation during the stage-1b cutover
**Graduated**: 2026-11-16 — `superviseListener` (cmd/foxess-local/listener.go): EADDRINUSE retried every 5 s for up to 120 s, then the process exits non-zero (systemd `Restart=always` retries); any other listener error, or the listener stopping while the service runs, is fatal at once. Tests: the `TestSuperviseListener*` synctest suite + `TestRunCmdExitsWhenListenerFails` (e2e). Residual (reported to prox, not worked around): no SO_REUSEADDR on the TPROXY bind, and no listener-ready signal (the "up" line covers HTTP only; prox's `listening` log line is the readiness proof).

### DEF-007 [GRADUATED]: Shutdown panic — RecordChunk after Close
**Category**: other
**Statement**: stopping the service while capture streams were still persisting raced `handle.Shutdown`'s `st.Close()` against the persisters: `store: RecordChunk after Close` → panic at stop.
**Why it matters**: P-004/P-005 — a panic at stop turns a clean shutdown into a crash-restart cycle (systemd) and risks a partially-persisted tail in the store.
**Graduation path**: quiesce-before-Close in `handle.Shutdown` (`st.WaitStreams()`, error-gated) + a deterministic red test. Bug, not an invariant: the fix closed the entry.
**Status**: graduated (c20f943, 2026-11-15)
**Blocked on**: none
**Discovered**: 2026-11-15, during the stage-1b cutover stop
**Graduated**: 2026-11-15 — quiesce-before-Close landed in c20f943 with `TestShutdownQuiescesCaptureStreams` (deterministic red); the repo-root binary was re-staged at c20f943.

### DEF-004 [GRADUATED]: mon150 `-file` "newest" default never resolves
**Category**: tooling-defect
**Statement**: `docs/recon/tools/mon150/main.go` — the `-file` flag is documented as "c2s capture (default: newest under /var/lib/prox/conns)" and the tooling docs (CLAUDE.md, profile §9) describe live mode as following the newest capture with auto-switch on reconnect/rollover, but `newestCapture()` globs `/var/lib/prox/conns/*/c2s.bin` — one level shallower than the actual layout `/var/lib/prox/conns/<date>/<session>/c2s.bin`. The glob never matches, so the function always returns "" and: (a) a run without `-file` silently reads the hardcoded fallback `/var/lib/prox/conns/20261104/1793779365405-2/c2s.bin` (a stale 11-04 session), and (b) the live-mode rotation auto-switch (the `newestCapture()` check in the tailer loop) never fires. No warning or error is printed in either case.
**Why it matters**: P-002/P-005 — a session following the docs believes it is watching the live inverter but is replaying a stale 11-04 capture; decoded "live" values are wrong and the HA-oracle cross-check compares stale frames against live HA data (mismatch or false confidence). The 2026-11-05 session avoided this only by passing `-file` explicitly.
**Graduation path**: fix — glob `conns/*/*/c2s.bin` (or walk the tree) and fail loud instead of falling back to the hardcoded path when no capture exists. Bug, not an invariant: the fix closes this entry; no C-NNN promotion.
**Status**: graduated (e90b04b, 2026-11-06)
**Blocked on**: none (self-contained fix)
**Discovered**: 2026-11-05, owner report
**Graduated**: 2026-11-06 — fix landed in e90b04b (`newestCapture()` globs `conns/<date>/<session>/c2s.bin`, fails loud when no capture); verified in live-tail mode.

## Retired

(None yet.)
