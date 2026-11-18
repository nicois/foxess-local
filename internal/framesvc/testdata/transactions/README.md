# Golden-transaction cases

A registered set of (input bytes → response bytes) exchanges extracted
from the prox corpus, each verified by `TestGoldenTransactions`
(`transactions_test.go`) to reproduce **byte-exact** under a named
policy through the framesvc rig. This is the ratchet: new supported
behavior lands when a new transaction gets registered here. The corpus
stays the *source* only — there is no discovery, no full-session
replay, and no count parity against the corpus as a test base.

## Case layout

```
<name>/
  case.json    the case: session, policy, epoch_ms, inputs, wants, audit
  in*.bin      input frames (one file each)
  want*.bin    expected client-leg frames (one file each)
```

`case.json` carries corpus *data* only (bytes, the session's `conn_open`
timestamp as `epoch_ms`, drive offsets). No expected windows, offsets,
or stamps are authored here:

- **Client wants** are read from the rig's client leg right after the
  input named in `after_input` and compared byte-for-byte.
- **The cloud leg** is identity by construction in the test: it must
  carry exactly the c2s inputs in drive order (off never dials: nothing
  arrives).
- **Audit rows** (decision/dir/class/origin/reason-prefix) are checked
  in order; their timing is computed in the test from the service's own
  emission constants (`trioDelay + n·trioRespGap + …` within a named
  jitter, everything else within a coarse window of its driving input).

## Pinned epoch

The rig's service clock is pinned to the session's `conn_open` instant
(`epoch_ms`), running on the bubble's virtual time (C-004 — no wall
clock). Time-derived bytes therefore reproduce the baseline's values:
the trio's `(S, fn)` stamp is `u = 256·S + fn = unix_seconds %
(256·65536)` (frame/trio.go) — for the bootstrap case the extractor
asserts the baseline's stamp equals what the service stamps at
`2a + trioDelay`, so the 45 trio bytes are byte-identical without any
masking. If a future case's baseline was stamped in a different second
than `epoch + trioDelay` would produce, either pick the epoch so the
residue aligns, or mask-compare (corpus-identity except `[2:5]` + CRC
against `TrioParamsAt(emitTime)` — precedent:
`bootstrap_test.go` `wantTrio` recompute) and record the deviation in
the case's `note`.

## Registering a new case

1. Pick a transaction (c2s/s2c exchange) from a prox session in
   `/var/lib/prox/conns` — mon150 (`-file`/`-s2c`/`-from`/`-to`) is the
   analysis tool; never re-derive bytes by hand.
2. Extract it **programmatically with asserts** (frame scan: marker,
   plen, terminator, CRC16 init `0xFFFF` poly `0xA001`; select frames by
   structural properties, never by hand-computed offsets; copy
   timestamps verbatim from `meta.jsonl` — its `ts` is **milliseconds**).
   Record the exact extraction command + selectors + session in the
   case's `extract` field. Never hand-paste payload hex into source.
3. Add the case directory here and make the test pass: the inputs drive
   at `at_us` (c2s meta has no per-frame ts — burst spacings are
   nominal; s2c timestamps are exact), the wants assert the response
   bytes, the audit entries assert the rows + timing class.
4. A denied-behavior case asserts silence (`silence_ms`) + the dropped
   row with its verdict `reason_prefix`.
