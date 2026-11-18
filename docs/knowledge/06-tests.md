---
project: foxesslocal
level: 6
last_verified: 2026-11-14
traces_up: [02-constraints.md, 04-design/]
---
# Test inventory

State: plans 1 + 2 landed on main (plan 1 commits `34ef944`…`208fccc`, including the final-review fixes I-1/M-1…M-5; plan 2 `c1e20f5`…`cfcb2d4`; SDD ledger `.superpowers/sdd/2026-11-02-foxess-local-recon-certlab/`). All 24 plan-1 tests below are live and shipped red-first per C-004, as are the plan-2 domain suites (below). Node IDs use `<package>::TestName` form. **OQ-1 resolved 2026-11-04**: the inverter-facing test surface moved to the plan-2 frame service (below).

## Config (plan 1 task 1)

**Constraints**: C-006 (config-driven single binary — loader is its base)
**Source**: `internal/config/config_test.go`

| Test | Verifies | Constraint |
|---|---|---|
| `internal/config::TestLoadValid` | full config parses to expected values | C-006 |
| `internal/config::TestLoadDefaultsRetention` | omitted fields take documented defaults | C-006 |
| `internal/config::TestLoadChunkBytesRetention` | chunk-byte knob: parses, defaults to 0 (off), rejects negative (TestLoadChunkBytesDefaultOff / TestLoadRejectsNegativeChunkBytes) | C-006 |
| `internal/config::TestLoadMissingFile` | missing file is an error, not a panic/zero-value | C-006 |
| `internal/config::TestLoadRejectsBadIP` | bad listener IP fails validation at load time | C-006 |

## eMudhra verification (plan 1 task 2)

**Design**: D-006 (P-001 mechanism — anchor identification)
**Source**: `internal/emudhra/*_test.go`

| Test | Verifies | Design |
|---|---|---|
| `internal/emudhra::TestSmallKeyForFindsK4` | a P-384 key with small-k 4 is identified as compromised | D-006 |
| `internal/emudhra::TestSmallKeyForEachCandidate` | every small-k candidate value round-trips (key from k → k found) | D-006 |
| `internal/emudhra::TestSmallKeyForRejectsRandomKey` | a random strong key is not misidentified | D-006 |
| `internal/emudhra::TestSmallKeyForRejectsNonP384` | non-P-384 keys are rejected, not misparsed | D-006 |
| `internal/emudhra::TestSmallKeysLiteralSet` | the eight small-k candidate literals are pinned (no drift of the known set) | D-006 |
| `internal/emudhra::TestG3CCert` | the embedded G3C public cert parses and is the known small-key intermediate (k=4) — the lazy bootstrap's basis | D-019 |

## Certificate generation (plan 1 task 3)

**Design**: D-007 (manifest), D-008 (validity/SANs)
**Source**: `internal/certgen/*_test.go`

| Test | Verifies | Design |
|---|---|---|
| `internal/certgen::TestGenerateSelfSigned` | selfsigned anchor kind produces a valid leaf+manifest | D-007, D-008 |
| `internal/certgen::TestGenerateSmallKeyAnchor` | small-key anchor kind mints under a compromised intermediate; manifest records small-k | D-006, D-007 |
| `internal/certgen::TestGenerateSmallKeyAnchorRejectsStrongIssuer` | small-key kind refuses an issuer whose key is not small | D-006 |
| `internal/certgen::TestManifestRoundTrip` | manifest write→load is lossless | D-007 |

The plan-1 broker ACL, certlab and MQTT capture suites were removed with those packages on 2026-11-18 (owner): they guarded only the optional MQTT surface, which the service never started. C-001/C-003 are covered by the policy, frame-service and gateway suites below.

## Plan 2 domains (implemented 2026-11-13)

Every domain is written red-first; the time-driven suites run under `synctest` virtual time.

- **Frame service + codec** — D-014, C-001 (structural half): `internal/frame::TestCorpusParity` + the codec suite (corpus parity, fixture decode, snap61 energy capacity, bootstrap-trio byte-exact, split/CRC/terminator errors, Modbus PDU parses); `internal/framesvc::TestTelemetryOnlyRelaysReads` (D-024: the cloud's reads reach the inverter and their answers the cloud; its write is denied; an interleaved operator command is acked by its own answer, which never reaches the cloud) + `TestRoute*` (answers routed to whoever asked) + the pipeline suite (reassembly order + resync, bootstrap trio before the first forwarded byte, telemetry cadence zero-loss, firmware-sized forwarding in `full`, writes denied / reads relayed in `telemetry_only`, S2C starvation over two virtual hours, filter matches the cloud port only, off-mode local serving — `TestOffModeNeverDialsTheCloud` (zero cloud bytes; the leg is truly local), `TestOffModeLocalCommand` (bootstrap trio + an operator command over the local session, stamped with the inverter's own session tail pinned from its c2s session-pattern frame), coalesced schedule echoes — `TestSchedulePushCoalescedEchoes` (13 read responses in one chunk, every echo acked in stream order), s2c rebuild tiling — `TestS2CRebuildTilesEmbeddedFrame` (a wrapper's embedded frame is audited but never re-emitted). **Reconciliation oracle (owner-supplied)**: decoded telemetry values are cross-checked, *lagged*, against the inverter's live HA sensor state (foxess-control integration, cloud-fed — the owner provides an HA auth token; we query HA, no websocket code on our side), until the local backend replaces the cloud feed. This validates the snapshot field-level decode (profile §4/§8).
- **Policy mode ladder** — C-001/C-002: `internal/policy::TestFullPassesEveryKind` + the ladder suite (`TestTelemetryOnlyDeniesEveryKind`, `TestOffGuard`, `TestWindowOverridesEveryMode`, the deny reason names the frame).
- **Maintenance window** — D-016: `internal/forward::TestExpiryFiresEventAndRestores` + the window suite (default duration, re-arm, per-call cap, close, synctest expiry incl. re-arm honouring); `internal/api::TestMaintenanceOpenRearm` + the handler set (inactive / open-rearm / duration-errors / close).
- **Forward-mode state** — D-009: `internal/forward::TestNewDefaultsToTelemetryOnly` + the mode-state suite (seed defaults to the closed mode, unknown mode refused, per-mode `Decide`, leg round-trip — the off leg is local-only: no dial, zero cloud bytes, while a `full` flow mirrors (the `r3` ladder test), `Set` changes the new-connection decision, audit rows).
- **Gateway validation/clamp** — C-003: `internal/gateway::TestFrameWorkModeByteExact` + the gateway suite (byte-exact command frames — work-mode / meter-compensation / limits, schedule read-set + Δ signature + no-op + r28 latch, the full `Validate` surface, in-flight rejection, flow-loss fails the in-flight command, observer acked/failed/expired once).
- **REST API** — C-005: `internal/api::TestAuthRoleMatrix` + the api suite (the auth role matrix — the spec's x-required-role table, token lifecycle, state, history buckets + gap nulls, SSE gap / Last-Event-ID / query-token, maintenance, forward mode, config view, public access, command lifecycle — ack read-back, in-flight 409, offline 503, a failed read-back → 502 with the gateway's detail + `applied=no` (`TestPutCommandFailed`)).
- **Scheduler** — C-004: `internal/forward::TestSchedulerOpenClose` + the scheduler suite (open/close, in-flight retry) — virtual time.
- **Retention / store** — C-006: `internal/store::TestMaintainRawRollupThenPrune` + the store suite (close-based reap — long-lived sessions survive the raw window, finished conns reaped whole by close, watermark guard, crash orphans synthesized at open then reaped (TestRestartOrphanSynthesizedAtOpen / TestRestartOrphanSessionReaped), chunk byte horizon + off (TestMaintainLongLivedSessionSurvives / TestMaintainReapsFinishedByClose / TestMaintainChunkBytes), chunk payload persistence (TestRecordChunkPayload), release stamping (TestRecordConnStampsAppVersion / TestMigrationAddsVersionColumn — connections rows carry the opening binary's version; a pre-v3 DB upgrades in place and old rows read 'unknown'), rollup — idempotent, backfill, self-heals last bucket, audit round-trip, batch writer).
- **Smoke + UI** — C-002 (positive proof), C-003, D-014: `cmd/foxess-local::TestStage1bSmoke` (the end-to-end `synctest` run: scripted inverter ↔ frame service ↔ scripted cloud — forwarding in `full` incl. the byte-exact firmware chunk, a work-mode command with read-back, audit rows), token-mint tests, UI mount tests; `internal/web::TestIndex` + the static-asset set.
- **CA bootstrap** — D-019: `cmd/foxess-local::TestCaMaterialBootstrapsT2Anchor` (no certgen output → the first `caMaterial` call self-mints the T2 anchor from the embedded G3C public cert: manifest `small-key` k=4, chain = leaf + G3C intermediate, the configured iPAddress SAN, and the second call reuses the cached mint byte-identical; a crash between the two material writes self-heals — `TestCaMaterialHealsPartialT2Material`, a missing issuer-key triggers the rebuild); `internal/emudhra::TestG3CCert`.

## Unmapped tests

None (plan-1 + plan-2 tests trace to a C-NNN or D-NNN; the coverage script reconciles the matrices).
