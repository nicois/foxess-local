# Position: the interception interface foxesslocal wants from prox

Status: position document (input to the prox refactor and to the plan-2
frame-service design). 2026-11-06; rev 2026-11-10 (R13 added after the
v0.1.0 publish audit); rev 2026-11-11 (R13 satisfied in 8d68f0e).
Scope: what the foxesslocal team requires of prox-as-a-library so that building
the local stack on top of it is the superior choice. Deliberately does not
reference prox's current implementation — it states what we want, and the
refactor can meet it any way it likes.

## Context — the two facts this position stands on

1. **Our end state** (ratified, D-013/C-007): the egress gate (nftables +
   tproxy on the gateway host) default-denies the inverter's egress and hands the
   tproxied cloud-channel flows (TCP/14431) to a frame-decoding service in the
   foxesslocal binary. That service is the sole upstream→inverter path (C-003),
   applies a mode-level decision to upstream-originated messages — deny-all
   except `full` mode or an active maintenance window (C-001/C-002; the
   per-message allowlist and firmware hold were retired 2026-11-13) — and
   forwards to the real cloud per forward mode `off | telemetry_only | full`,
   and — at end-state
   C — serves the inverter with **no upstream at all**.
2. **The channel is adversarial to assumptions**: proprietary framed binary
   (7f7f framing, ~240 B telemetry, Modbus PDUs in `0x10`-family envelopes),
   SNI is a raw IP (no DNS name), the inverter trusts any CA (T2 adopted), and
   the register/field layout is still being pinned down from captures (plan-2
   codec input = the recon profile). The wire format will shift as we learn
   more and as firmware changes.

Until stage-1 cutover, prox remains the live interceptor and capture oracle;
that role is unchanged by this position either way.

## The decision this document feeds

Build the interception layer of foxesslocal **on top of prox as an embedded
library** (proven two-direction interception, hook-registered), or keep the
self-contained TLS listener from the stage-1 runbook. The gate, the codec, the
policy engine, the REST/UI surface, the scheduler, and the SQLite store are
identical under both outcomes — the decision is only *where the interception
service lives*.

## Requirements

Priorities are RFC 2119. "MUST" items are the adoption gate: if the prox
refactor does not converge to all of them, foxesslocal builds its own listener
and prox remains the recon-capture tool.

### R1 — Two-direction raw-byte interception (MUST)

Per connection, a hook surface that receives byte chunks in **both**
directions, tagged by direction, with flow identity available to the hook:
the client/upstream four-tuple and — when TLS is terminated by the layer —
SNI (which in our deployment is an IP address) and handshake result.

*Why*: policy (C-001) and the codec are message-level and owned by
foxesslocal; the frame format is still under reconstruction and will evolve.
Any protocol assumption baked into the hook layer is a defect in our use,
because our codec is the only component allowed to know the protocol.

### R2 — Full peer control, not a tap (MUST)

For each direction, per chunk, the hook chain must be able to: pass
unmodified, transform, **drop**, or **inject** — and injection must be
possible from our code at any time, independent of receiving bytes in the
other direction (operator commands arrive from HA/UI/scheduler at arbitrary
moments, C-003). The client connection must remain healthy while bytes in the
other direction are withheld indefinitely (upstream denied in
`telemetry_only`, absent entirely in `off`), and the withholding
pressure must be observable (see R5).

*Why*: C-001/C-002 are day-one invariants of the end state, not a feature of
any forward mode. A tap that cannot withhold and cannot speak for its peer
cannot host the end state.

### R3 — The upstream is optional (MUST)

A flow must be servable in two peer modes, decided per flow at runtime:
(a) mirrored to a real upstream (forwarding), or (b) **no upstream** — the
local side synthesizes every downstream byte (end-state C). Forward mode
(`off | telemetry_only | full`) must be expressible as a runtime per-flow /
per-frame decision, not as per-process configuration.

*Why*: stage-1 cutover is precisely the moment upstream becomes optional; a
proxy whose downstream direction is wired to an upstream connection cannot
represent `off` or the post-failover state honestly.

### R4 — Ordered, chainable hooks with short-circuit (MUST)

Multiple hook instances per flow, ordered, all seeing every chunk exactly
once, with two composable effects: *transform* (sequential) and
*short-circuit* (drop by any hook, terminating the chain for that
chunk). Hook state must be per-flow, not global — the mode/window decision,
capture state, and forward mode are all per-connection in our design.

*Why*: the policy engine, the telemetry store, the audit capture, and the
forward client are separate plan-2 components that must each observe every
frame without cross-wiring.

### R5 — Deterministic ordering, observable pressure, no silent loss (MUST)

Per-direction byte order preserved; hook decisions synchronous; buffering
bounded but with observable pressure (watermark callback or equivalent).
Backpressure or buffer-exhaustion events MUST surface to the hook chain —
absorbed loss is unacceptable.

*Why*: telemetry fidelity is a priority (P-002; DEF-002). A silently dropped
frame is an invisible fidelity regression; the HA-oracle cross-check would
catch it late and noisily.

### R6 — Capture and audit as a first-class stream (MUST)

Per connection, the layer MUST expose (as a readable stream or API, not as a
filesystem side-effect): (a) raw bytes per direction with capture timestamps;
(b) the **post-decision view** — for each upstream-originated chunk:
forwarded / dropped, with the dropping hook's reason; (c) every
injected chunk marked as injected with its origin; (d) connection metadata:
four-tuple, TLS handshake result, start/end times, close cause.

*Why*: C-001 and C-002 are auditable invariants — "which upstream→inverter
messages passed (and by which open state — `full` or the active window)" and
"which were dropped, and why" must be provable from the record alone, after
the fact, by the owner or a reviewer.
The plan-2 SQLite store and the replay tooling (mon150) both consume this;
today's interim state is scraping per-connection files from a shared
directory, which this requirement is meant to outgrow. This is also the
property that makes a third-party interception layer *trustable* on the
C-001/C-002 critical path.

### R7 — TLS as a pluggable service (MUST)

TLS termination enabled per flow by us; a certificate-selection callback
receiving the client's offer (SNI included — IP-SAN selection must work,
since the inverter dials raw IPs including the Aliyun failover set); the CA
and certificate store remain ours. Non-TLS flows must reach the same hook
surface.

*Why*: T2 anchor (inverter trusts any CA; we mint and rotate certs via
certgen). The cert selector is ours because the anchor is ours.

### R8 — Single-binary embedding, clean lifecycle, stable API (MUST)

Consumable as a Go library from our single binary (C-006: single binary, pure
Go); ctx-based start/shutdown with graceful drain; no per-connection
goroutine or state leak across months of unattended uptime; programmatic
configuration; and a **versioned, stable hook API** (semver) — foxesslocal is
a permanent tenant of the surface, not a one-off consumer.

*Why*: the gateway host runs unattended; the policy engine compiles once and lives. An
unversioned hook API would make every prox change a foxesslocal risk event.

### R9 — In-memory transport test harness (SHOULD)

A loopback or in-memory client+upstream pair driving the same hook surface, so
policy and codec tests can run red-first (C-004) without a network, and time
drives cleanly with `synctest` for hold/timeout logic.

*Why*: today every interception-behavior test needs a real TCP pair — fine for
integration, too slow and too flaky for unit-level policy tests.

### R10 — Typed lifecycle and failure events (SHOULD)

Upstream connect failure, upstream close, client close, TLS alert, and
hold-depth watermark delivered as typed events with a cause value, so plan-2
alerting (DEF-001) reacts to events instead of inferring state from silence.

*Why*: the post-cutover failure mode is a blackholed uplink; alerting quality
is bounded by the event quality of the layer underneath.

### R11 — Receive-timestamped capture chunks (MAY)

Per-chunk receive timestamps on the capture stream. Replay tooling currently
reconstructs command times from inverter-response correlation; byte-granular
timestamps would make replay attribution exact.

### R12 — Per-flow labels (MAY)

A way to attach typed labels to a flow at establishment (e.g.
`inverter-cloud-channel` vs `other`) that all subsequent hooks and capture
records inherit.

### R13 — Consumer-owned capture rules (MUST)

In embedded use the library must not install or remove firewall rules
itself: the consumer owns the egress gate (C-007 — nftables,
default-deny on the inverter source IP), and the tproxy diversion is one
rule inside that gate, not a second ruleset the library applies and
removes on its own lifecycle.

*Why*: the published v0.1.0 violates this — `Prox.Run()` unconditionally
applies and removes iptables TPROXY rules through a hardcoded exec runner
(`pkg/prox/prox.go` Run→`rules.Apply`/`rules.Remove`;
`pkg/rules.BuildRules` hardcodes `Bin: "iptables"`): no runner
injection, no embedded-mode skip. The consumer's nftables gate and a
library-managed iptables ruleset cannot both own PREROUTING for the same
port/mark. Reported to the owner 2026-11-10; tracked-fail in the conformance
suite (`test/proxconformance`, R13). Per owner policy the issue is
reported, not worked around. **Satisfied in `8d68f0e` (2026-11-11):**
`rules.mode: "consumer"` makes `Prox.Run()` install and remove nothing;
`SetRulesRunner` injects the runner in managed mode — verified at the
facade by the R13 check.

## What we do not want from prox

- **No protocol parsing** in the hook layer (no MQTT, no frame format) — the
  codec is ours and evolves with the recon.
- **No storage engine** — SQLite is ours; prox exposes streams, we persist.
- **No policy engine or allowlist** — default-deny is foxesslocal's engine.
- **No DNS in the critical path** — the inverter dials raw IPs; the local
  resolver is observation-only. Opt-in if offered.
- **No mandatory sidecar process** (C-006).
- **No opinionated retention or pruning** — the store's job.

## The surface we would want (requirement sketch)

Illustrative Go-flavoured sketch of R1–R8 — names are placeholders; the
constraint is the shape, not the identifiers:

```go
// One hook instance per flow, ordered in a chain; every chunk in both
// directions passes through the chain exactly once.
type Hook interface {
    // Decide how the flow is served: terminate TLS and mirror to upstream,
    // serve locally with no upstream (R3), or reject. May set flow labels (R12).
    OnEstablished(flow *Flow) EstablishDecision
    // Per chunk, both directions (R1, R2, R4). Result: pass / transform /
    // hold / drop — hold and drop MUST carry a reason that lands in the
    // post-decision capture view (R6).
    Handle(flow *Flow, dir Direction, data []byte) ChunkDecision
    OnClosed(flow *Flow, cause Cause) // R10
}

type Flow struct {
    Tuple     FourTuple          // client + upstream, 0-side if no upstream (R3)
    TLS       *TLSInfo           // SNI (an IP, in our deployment), version, result
    Labels    map[string]string
    WriteToClient(ctx, data) error // injection at any time (R2)
    HoldDepth() int64                  // observable (R2, R5)
    Events() <-chan Event              // R10
}

type Registry interface {
    Use(hooks ...Hook)       // ordered chain (R4)
    Capture() CaptureReader  // R6: raw + post-decision + metadata streams
    Listen(ctx) error
    Shutdown(ctx) error      // graceful drain, no leaks (R8)
}
```

The two properties that are hardest to retrofit and worth checking first in
the refactor: **R3 (upstream-optional flows)** and **R6 (post-decision
capture view)**. Everything else is a conventional proxy-hook surface.

## Evaluation and fallback

- **Adopt** when the prox refactor satisfies R1–R8 (and ideally R9–R10).
  Then: the frame service becomes a hook chain over a prox registry embedded
  in the foxesslocal binary; the gate (C-007) and the stage-1 runbook are
  unchanged except that "our TLS listener" is a prox instance; mon150 and the
  SQLite store consume the capture API (R6) instead of files.
- **Build in-house** if any MUST is not met — in that case the original
  stage-1 design stands, and this position is revisited against the next prox
  API release.
- Note on trust: adopting prox places its code on the C-001/C-002 critical
  path. R6 is what makes that acceptable — an interception layer that cannot
  prove what it dropped, injected, and forwarded cannot host a
  default-deny invariant, regardless of how convenient the rest of its API is.

## Open questions for the prox refactor

1. Is the hook surface byte-stream based, or does it impose a framing? (We
   need byte-stream; framing is ours.)
2. Can a flow be served with no upstream, and does the client-side connection
   stay healthy under indefinite withholding (upstream denied or absent)?
3. Are hook instances per-flow (not global), multiple, orderable, with
   short-circuit?
4. What is the capture contract — API or files — and does it record the
   post-decision state (forwarded / dropped+reason / injected)?
5. Is TLS termination pluggable with a cert-selection callback, and does it
   handle IP-SNI?
6. Is there a semver policy for the hook surface?
7. Is an in-memory transport for tests present or planned?
8. What are the backpressure semantics — is buffer pressure observable, and
   what happens under sustained withholding in one direction while the other
   direction is quiet (our channel is uplink-heavy: 5 s telemetry, ~30 s
   keepalive, sparse downstream)?
