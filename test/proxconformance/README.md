# prox conformance suite

Runnable R1–R13 adoption gate: tests that drive the **real prox relay** and
assert each requirement of [`docs/positions/prox-interface.md`](../../docs/positions/prox-interface.md)
against a **pinned prox version**. "prox will comply" is a version pin with
evidence, not an assumption — a later upgrade re-runs this same gate.

## Pinned prox

- module: the published `github.com/nicois/prox`, required in `go.mod` as
  `v0.0.0-20261113075658-359e3da207e2` (commit `359e3da`, the public
  repository's "initial commit"; no release tag yet). Its tree is
  identical to the local v0.1.0 publish commit `a93e083` the suite was
  run against before (tree `d3b90d1`, compared 2026-11-17), the squashed
  publish of `8d68f0e`, whose range since the previous pin implements
  the [gap
  clarification](../../docs/positions/2026-11-10-prox-v0.1.0-gap-clarification.md)
  items: consumer-owned rules mode (R13), opt-in capture payload (R6a),
  hook-drop chunk length, s2c cap losses on the capture stream. Phase 1
  + Phase 2 (serve mode, hold, leaf selector, typed events).
- `internal/proxpin` reads the version from the build info, so the app's
  version stamp (`…-prox-359e3da`), this suite's skip reasons and
  `go.mod` cannot disagree.

## Results (against the pinned commit)

| Req | Result | Test |
|---|---|---|
| R1 two-direction interception | PASS | `TestR1_TwoDirectionInterception` |
| R2a transform | PASS | `TestR2a_Transform` |
| R2b drop | PASS | `TestR2b_Drop` |
| R2c inject at arbitrary moment | PASS | `TestR2c_InjectAtArbitraryMoment` |
| R2d starvation, client stays healthy | PASS | `TestR2d_Starvation` (synctest, 300 ms virtual hold) |
| R3 upstream-optional serve mode | **SKIP-tracked** (check body pending; Phase 2 in pin) | `TestR3_ServeMode` |
| R4 ordered chain, short-circuit, per-flow state | PASS | `TestR4_OrderedChainBothSeeEveryChunk`, `TestR4_ShortCircuit` |
| R5 per-direction ordering, bounded no-silent-loss | PASS | `TestR5_OrderPreservedPerDirection`, `TestR5_CaptureBoundedNoSilentLoss` |
| R6 capture post-decision view | PASS (findings below) | `TestR6_CapturePostDecisionView` |
| R7a IP-SNI minting | PASS | `TestR7a_IPSNIMinting` |
| R7b cert-selection callback | **SKIP-tracked** (check body pending; Phase 2 in pin) | `TestR7b_CertSelectionCallback` |
| R8 embedded mode, clean drain, no leak | PASS | `TestR8_EmbeddedModeNoFiles`, `TestR8_DrainNoGoroutineLeak` |
| R9 in-memory transport, synctest-safe | PASS | `TestR9_InMemoryTransportSynctest` |
| R13 consumer-owned capture rules | PASS (facade level: consumer mode makes zero runner calls; managed mode routes install+removal through the injected runner) | `TestR13_ConsumerOwnedCaptureRules` |

Zero failures; the only skips are R3/R7b (check bodies pending — their
Phase-2 features are in the pin, so the documented bodies must land on
the next gate pass), each with a `tracked: …` skip reason naming the
pinned short SHA.

## Drive level and harness notes

Every check runs the relay at its **ladder seam** — `proxy.Ladder.Serve` on
the client leg, `Ladder.Dial` for the scripted upstream, `pkg/chanpipe`
in-memory transport (created inside synctest bubbles where used). This is
the same seam the TPROXY facade (`pkg/prox`) drives per connection; the
facade's `Run`/`Shutdown` itself needs CAP_NET_ADMIN (nftables + a
transparent listener) and is not drivable in an unprivileged test.

- **R8 drain level**: ladder seam. In-flight flows are drained by closing
  every leg (the pinned version exposes no facade-level drain without
  privileges); the check asserts `OnClose` fires for every flow with a
  clean cause and `runtime.NumGoroutine` returns to the pre-test baseline
  after settling on the virtual clock.
- **Client four-tuple**: the ladder derives `hooks.Conn.Src/SrcPort` from
  `conn.RemoteAddr()` exactly as on a real TPROXY socket; chanpipe's
  `net.Conn` carries no address, so the suite presents the tuple through a
  small `addrShim` wrapper (bytes flow through the untouched chanpipe).
- **SNI**: Go's stdlib client **suppresses the SNI extension for
  IP-literal ServerNames** (`crypto/tls` `hostnameInSNI` returns `""` for
  parseable IPs), so an end-to-end IP-SNI flow cannot be driven from a Go
  client. SNI-carrying flows use a hostname (sniff's SNI parse is
  byte-generic — no hostname assumption); the IP-literal shape is covered
  in `TestR7a`: (1) a raw-IP dial completes the MITM and the forged leaf
  carries the dst IP as an iPAddress SAN (the client verified it), and (2)
  the leaf minter given an IP-literal SNI (the inverter's OpenSSL dial
  shape, failover-set IP included) mints an iPAddress SAN.
- **R2d starvation** exercises the Phase-1 **drop** verdict
  (`Result{Data: []byte{}}`); the `Held` verdict is reserved in the pinned
  version (no Held records are emitted — asserted in the R6 check).

## R3 check body (tracked; Phase 2 is in the pin — implement on the next gate pass)

When the pin moves past the Phase-2 merge, replace the skip with:

1. one flow per serve mode on the same ladder — the decision hook picks
   `ServeMirror` for one flow and `ServeLocal` for another **at runtime**;
2. the `ServeLocal` flow has **no upstream connection at all**; every
   downstream byte is synthesized by the hook's consumer-provided leg
   (assert the relay never dials for that flow);
3. the `ServeLocal` client stays healthy indefinitely: N virtual seconds
   (synctest) of c2s traffic answered only by injected bytes, no upstream
   leg, connection still open at the end.

## Findings — tracked prox gaps

- **R6(a) — no payload bytes in the capture record: resolved in
  `8d68f0e`.** `Registry.EnableBytes()` turns on payload capture for
  streams opened after the call (default off; the record path pays only a
  bool check); `capture.Chunk.Payload` is a copy of the chunk bytes at
  the record site. foxesslocal will opt its capture stream in so the
  post-decision index can stand on its own bytes; the frame-level store
  (plan 2, Task 4) remains the protocol-level audit.
- **Dropped chunks record `Len == 0`: resolved in `8d68f0e`.**
  `hooks.Pipe.Data` records a drop with `Len` = the chunk as the dropping
  hook received it, consistent with every other record path. The R6 check
  asserts the length.
- **Hook-write order vs in-flight relay bytes** (open confirmation —
  clarification doc Q1): `HookConn` writes are serialized with the relay's
  writes to the same direction, but their order relative to already-in-
  flight relay bytes is not contractually fixed (observed in R6: the
  injection can land before a just-forwarded chunk). Plan-2 consumers must
  not assume total order with relay traffic; per-chunk identity comes from
  the capture record (origin/decision), not from stream position.
- **R13 — consumer-owned capture rules (MUST): resolved in `8d68f0e`.**
  `rules.mode: "consumer"` makes `Prox.Run()` install and remove nothing
  — no runner calls at all, including on exit; `SetRulesRunner` injects
  the runner in managed mode for both install and removal. Verified at
  the facade by the R13 check.

## Upgrade procedure

1. `go get github.com/nicois/prox@<tag or commit>` and `go mod tidy`
   (the stamp and the skip reasons follow by themselves).
2. Update the module line above.
3. Re-run: `go test ./test/proxconformance/ -v -count=1`.
4. If a check now fails: decide whether the pin or the check is stale —
   a Phase-2 behavior change (e.g. `Held` records appearing) updates the
   check; a regression files a prox issue and holds the pin.
5. When a tracked skip's feature lands in the pin, replace the skip with
   the documented body (R3 above; R7b's body is in its test comment).
