---
project: foxesslocal
level: 4
feature: device-compatibility
last_verified: 2026-11-16
traces_up: [01-vision.md, 02-constraints.md, 03-architecture.md]
traces_down: [05-coverage.md, 06-tests.md]
---
# Design: device identification, compatibility confidence, and diagnostic bundles

Status: **owner-approved (2026-11-16); C-008 ratified.** Step 1 (identity)
is live: on the reference inverter, 30000 ×15 reads "KH10" one character
per register, space-padded, and /state matches `kh10-fw1.73`. The
implementation order is at the end.

## Problem

Everything the app knows about register meanings was proven on one
inverter: a KH10 with firmware master 1.73 / slave 1.03 / manager 1.71
(protocol profile §5). Another model, or another firmware generation,
can move or redefine registers. A wrong guess on a read shows a wrong
number. A wrong guess on a write changes the wrong setting on someone's
inverter. Live example (2026-11-16): the open-source foxess_modbus map
calls 41010 "max SoC", but the cloud writes *max SoC from grid* there;
max SoC is 41012.

The aim is to protect inverters without refusing to work over trivial
differences, such as a firmware patch that changes nothing we use.

## Design decisions

### D-020: Evidence-graded compatibility, per feature, re-checked continuously

**Decision**: the app identifies the device without side effects, then
grades its confidence **per feature**, not per device. Each feature
declares the registers it uses and the checks that must pass:

- live values
- settings read
- work mode
- power limits
- current limits
- schedule
- meter compensation

**Confidence levels:**

| Level | Condition | Allowed |
|---|---|---|
| **exact** | model and every firmware version match a verified profile, and the feature's checks pass | everything verified for that profile |
| **compatible** | same family and register generation, other firmware, and the feature's checks pass | reads on; writes only after write-grade evidence (below) |
| **tentative** | the mapping comes only from a third-party source (D-021) and the read checks pass | reads (monitoring) only |
| **unknown** | no mapping, or any check fails | identity + monitoring of what passes checks; writes off. An installer override is audited and time-boxed. |

**Identification** uses only side-effect-free sources, in this order:

1. **Passive (no request sent):**
   - the device-info message (`snap01`) the inverter pushes every
     ~5 min: serial number, model string, four firmware versions;
   - the battery module list (`snap60`).
2. **Read-only probes**, sent only once the passive model string names
   a known family, and only from a fixed allowlist:
   - holding 30000 ×15: model string (the foxess_modbus method);
   - 30016–30018: firmware versions;
   - the generation pattern: which other families' version registers
     (10016, 36001, …) answer "illegal address".

   A read changes nothing on the inverter, and an exception reply is
   harmless.

**Checks** are chosen so a wrong map fails them but a patch-level
difference passes:

- **Physical consistency:**
  - PV voltage × current ≈ PV power, per string;
  - grid voltage and frequency are plausible;
  - SoC is within 0–100;
  - the power balance closes within a tolerance;
  - the polled SoC agrees with the inverter's own snapshot SoC.
- **Settings plausibility:** every value read lies in the installer's
  range (profile §5 "Installer ranges"). Examples:
  - current limits 2–100 A;
  - the high word of a U32 power limit is 0;
  - meter compensation within ±500 W;
  - schedule groups decode to valid HH:MM, a valid work-mode enum, and
    `+9 = 2`.
- **Schema agreement**, when the owner supplies the cloud's parameter
  schema (as `dump-2026-11-16.json`): the same protocol ID, and the same
  property order and types for the groups a feature uses.

**Write-grade evidence**, any one of:

- a verified profile;
- a captured cloud write of that setting on a device of that
  family/generation (a guided cloud session, D-022);
- schema agreement for the setting's group, **plus** a no-op write.

A **no-op write** writes the current value back, then checks the echo
and the read-back. It proves the address and the encoding while
changing nothing. It is allowed only on settings registers, never on
action registers (system on/off and the like).

**Continuous checking**: every settings read and every poll re-runs the
checks. A device whose firmware changes mid-life (a cloud push in
`full`) drops to the level its new evidence supports without anyone
acting. The level, its reason, and the evidence source of every
register are visible in `/state` and the UI.

**Context**: one test device; other owners have other models. The
gateway's range checks (75bb595) stop absurd values but cannot tell
whether a register means what we think.

**Rationale**:

- Per-feature grading stops one unknown block from disabling everything.
- Physical and range checks catch the dangerous case (a shifted or
  foreign map) cheaply, from data we already read.
- The no-op write turns "probably right" into "proven" with zero effect
  on the device.

**Priority served**: P-001 (local control works on more devices), P-002
(fidelity: values carry their provenance).
**Trades against**: P-005 (more machinery before supporting a second
model). Accepted: the alternative is unsafe writes.
**Alternatives considered**:
- Whole-device allowlist (exact match or nothing): rejected. It blocks
  monitoring over a trivial firmware bump.
- Trust the model string alone: rejected. The 41010 discrepancy shows a
  family name does not pin register meanings.

### D-021: foxess_modbus as the tentative-mapping source, copied with attribution

**Decision**: import the register data of nathanmarlor/foxess_modbus
(MIT, © 2021 nathanmarlor) from a pinned commit: model-string patterns,
the generation rule (e.g. KH below 1.33 → `KH_PRE133`), register
definitions with scale and declared ranges. A generator script
(`tools/`) emits a Go data table into the repo. It records the commit
and carries the MIT notice in the generated file and in
`THIRD_PARTY_NOTICES`. Refreshing to a newer commit is a reviewed diff.

**Evidence precedence**, highest first; a disagreement is flagged, never
silently resolved:

1. our verified profiles (captured cloud writes and read-backs);
2. the cloud's own parameter schema for the device;
3. foxess_modbus;
4. the generic FoxESS Modbus spec (V1.05).

A foxess_modbus-only mapping yields **tentative**: monitoring once the
read checks pass, never writes. The cloud channel reaches the same
register server as RS485: our KH10 answered the foxess_modbus KH RS485
live-value map at 31000, and 41000/41007/41008/41009/41011 agree with
the captured cloud writes.

**Known divergences on the reference KH10** (to report upstream with
evidence, owner's call):

- 41010 is max SoC from grid, not max SoC.
- 41012 is max SoC: it exists, but foxess_modbus marks it invalid.
- The power limits are 41021–41024, high word first. foxess_modbus maps
  46616/46617, low word first, on 1.33+ only.
- 41015, 41016, 41027, 41028 and 41032 are unmapped there.
- A possible generation-selection fallthrough to `KH_133` for versions
  below 1.33 (unverified).

**Test use**: run the confidence checks against foxess_modbus's maps for
the *wrong* generations on recorded data. A wrong map that passes means
the checks are too weak.

**Priority served**: P-001, P-005 (broad coverage without owning a
fleet).
**Trades against**: none: it adds read-only coverage; writes stay gated
by D-020.
**Alternatives considered**: hand-copying selected registers. Rejected:
silent drift, no provenance.

### D-022: De-identified diagnostic bundles, owner-reviewed, folded into releases

**Decision**: `foxess-local bundle` (and a UI button) exports a
self-contained, **de-identified** archive that an owner can review and
send. Nothing is uploaded automatically. Releases incorporate submitted
bundles as a compatibility corpus.

**Contents** (all plain JSON / CSV / binary frames, with a manifest):

- identity: model, firmware versions, generation pattern, and the
  confidence levels and check results per feature;
- the settings and live-value register reads, and the frames table for
  a chosen time range, with direction, class, function, decision and
  verbatim payload;
- optionally:
  - the cloud parameter schema the owner exported;
  - a guided-session run sheet: the owner's annotated cloud operations
    (setting, from → to, time), as in
    `docs/recon/cloud-ops-plan-2026-11-16.md`, but captured from the
    UI.

**De-identification:**

- **Field-aware redaction through the codec.** The decoders know where
  identifiers live:
  - inverter SN: device-info and status snapshots, keepalive, the
    40-byte SN frame, `serialquery`/`cfg40`;
  - datalogger SN: the registration frame;
  - battery module SNs: `snap60`.

  Each is replaced by a same-length pseudonym, and the frame CRC is
  recomputed so the bundle stays parseable by mon150 and the test
  corpus.
- **Fail closed.** After redaction, scan every byte of the bundle for
  each identifier collected from the device (ASCII and BCD forms). Any
  hit aborts the export.
- **Dropped:** IP addresses and connection IDs (renumbered), installer,
  account or plant names (from the schema export), and RTC register
  values (40000 ×6; kept as offsets).
- **Time:** dates are shifted to a fixed epoch while time of day is
  kept, because solar and charge patterns matter for analysis.
- **Reviewable before sending.** The manifest lists every file and what
  was redacted, and the UI shows a summary.

**Release integration**:

- Bundles land in `compat/` (one directory per device-firmware).
- A test re-runs identification and the D-020 checks on every bundle. A
  release must classify every submitted device at the same or a higher
  level than before, unless a regression is acknowledged.
- A bundle with a guided cloud session supplies **write-grade evidence**
  for that model and firmware, so it can promote a profile to *exact*.

**Context**: owners can safely run a guided cloud session today (the
2026-11-16 session settled 13 registers in about an hour). The
bottleneck is getting that evidence to us without exposing their
identity or home.

**Priority served**: P-001, P-005.
**Trades against**: P-004 (an extra export path to maintain). Accepted.
**Alternatives considered**:
- Raw prox captures: rejected. They carry every identifier and TLS
  material.
- Regex scrubbing: rejected. A binary-protocol identifier that the regex
  misses leaks silently. The codec-plus-scan approach fails closed.

## Constraint (ratified 2026-11-16)

**C-008 (ratified, `02-constraints.md`): writes need write-grade evidence
for the identified device.** No operator, scheduler or simulator write reaches a register
unless the identified device's confidence for that feature is *exact*,
or *compatible* with write-grade evidence (D-020). An installer override
is time-boxed and audited. Reads are not restricted beyond the
identification allowlist.

## Implementation order

1. **Identity**:
   - move the `snap01`/`snap60` decode out of `internal/sim` into a
     shared package;
   - add the 30000 ×15 model read and the 30016–30018 version read to
     the settings poll;
   - expose `identity` in `/state` and the UI;
   - record the KH10 1.73/1.03/1.71 verified profile.
2. **Checks** (done, `internal/compat`): per-feature levels in
   `/state.identity.compatibility` and the full report at
   `GET /api/v1/compatibility` (no gating yet). Checks: live values —
   PV V×A = W per active string (±max(50 W, 10 %)), grid 180–270 V and
   45–65 Hz, SoC 0–100 and within ±5 of the snapshot SoC, load ≈
   inverter + grid (±max(300 W, 10 %)); settings — the installer ranges;
   power limits — U32 high word 0, ≤ 30000 W; current limits 2–100 A;
   meter compensation ±500 W; work mode 0–3; schedule — master 0/1, per
   group valid HH:MM, work mode, SoC bytes, fd power ≤ 10500, data
   version `+9 = 2`. The poller stores the raw live blocks so these run
   on measured values. A KH whose version registers have not answered is
   `pending`, not `unknown`.
3. **foxess_modbus import** (done): `tools/fmimport/fmimport.py` reads a
   pinned checkout without executing it (profile table via `ast`, entity
   maps from upstream's own test snapshots, deduplicated per generation)
   into `internal/fmdata/foxess_modbus.json`, embedded by
   `internal/fmdata`; `make fmdata` regenerates it; MIT notice in
   `THIRD_PARTY_NOTICES.md`. Pinned commit `94252f06` (2026-11-06): 27
   profiles, 16 maps. Live checks now run on any map (`compat.FromMap`).
   Findings:
   - Upstream picks the KH generation from the **manager** version
     (≤ 1.33 → KH_PRE133), and it is a user-set option, not detected. The
     reference KH10 (manager 1.71) answers KH_PRE133, so the threshold
     does not hold: our layout probe (which version block answers) stays
     the authority, and the import keeps the threshold as data only.
   - On the real 14:52 probe registers, every other map is rejected. The
     H1 LAN and H1-G2 maps share the KH PV / grid / SoC registers and are
     caught only by the power balance (their load register is
     elsewhere), which the test pins as a real failure. H3-family maps
     fail on grid frequency.
   - Pinned divergence: 41010 (ours max_soc_from_grid, theirs max_soc).
     `GET /compatibility` reports the map, commit and divergences.
   Not yet: polling another family's map (the poller reads the KH
   layout only), so `tentative` has no live source until a non-KH
   device appears — its bundle will drive that work.
4. **Gating** (done): C-008 enforced at the gateway (`gateway.WithAuthorizer`,
   wired in production) for every writer — API, simulator, scheduler.
   - **Drivers** (`internal/driver`): one per register layout we at
     least partially support, selected by the layout the evaluation
     established (never a model name alone). Each declares per write
     feature: `verified` (encoding proven on a verified profile),
     `untested` (encoding known, unproven), `unmapped` (impossible: no
     known register or encoding), `not_supported` (impossible: the device
     lacks it — e.g. EPS recover SoC, 41037 illegal address on fw 1.73).
     Today: `kh-pre133`.
   - **Device view** (`internal/device`): identity + report + driver,
     shared by `/state`, `/compatibility` and the gate.
   - **Outcomes** (distinct HTTP codes): `501 write_unsupported` with
     `reason` unmapped / not_supported; `403 write_unverified` (an
     installer can grant); `409 checks_failing` (not overridable); `503
     device_not_identified`.
   - **Installer grant** (`GET/PUT/DELETE /api/v1/write-override`,
     C-008 as amended): persisted (schema v5 `settings`), bound to the
     exact model + firmware, audited, `acknowledge_risk: true` required;
     the control tab shows it only for an unverified device, behind a
     warning dialog.
   - The no-op write (write-grade evidence without a grant) is not built
     yet.
5. **Tentative reads for other families** (done):
   - The poller classifies each session (start, then every 5 min;
     retried every tick for the first minute while unestablished):
     identity reads, then the verified KH_PRE133 layout when the 30016
     block answers on a KH, else the first foxess_modbus candidate map
     (upstream order, deduplicated) whose live reads pass
     `compat.Classifiable`: no check fails; grid V / Hz, SoC and a real
     power balance pass; the PV product passes or is skipped (no active
     string — so a device can be classified at night).
   - A tentative layout polls its map's plan (`fmdata.PlanReads`: upstream
     invalid ranges never read, individual-read ranges read singly, ≤125
     per block, ≤8-register gaps merged), with the map's function code
     (input registers for H1 G1). Each poll is an ordinary telemetry row.
     A poll that fails a check is dropped; three in a row drop the layout
     (re-classified). The KH settings blocks are never read on another
     layout; its only settings read is the map's work-mode select (every
     5 min).
   - compat runs the same selection over the stored registers, grades
     live values and work mode `tentative`, everything else unknown (not
     mapped). The derived driver allows only the work mode (untested,
     installer grant) where the map's select is holding 41000 with our
     0/1 encoding.
   - Checks hardened on the way: `dc_balance` (PV ≈ inverter out +
     battery charging; 400 W floor for the fixed losses) and a no-flow
     balance is a skip, not a pass.

   **Assumptions not yet verified (other families):**
   - Other models' dataloggers speak the same cloud protocol (framing,
     bootstrap, Modbus-in-`7f7f` command channel) as the KH10's. Nothing
     but the KH10 has been seen. If not, the session never establishes and
     the first bundle shows how it differs.
   - The cloud channel reaches the same register server as foxess_modbus's
     AUX (RS485) maps — true for the KH10, assumed elsewhere. LAN-profile
     maps (H1_LAN) are also candidates.
   - foxess_modbus power sensors are kW, `grid_ct` positive = export,
     `invbatpower` positive = discharge (true for the KH maps against the
     probe; assumed for the rest).
   - Input-register (fn04) reads tunnel like holding reads, and the
     registers table may hold an input and a holding register at the same
     address without conflict (keyed by address only — no family mixes
     them in the ranges used).
   - A KH whose model reads empty but whose 30016 block answers is the
     KH_PRE133 layout (the pre-classification behaviour, kept).
   - Three-phase families: grid voltage = phase R; grid import = Σ phase
     CTs when there is no total.
   - Upstream's invalid ranges are conservative: the KH10 answers inside
     KH's (41012, 41019–43999). They are honoured for tentative layouts
     only.
6. **Bundles** (export done): `internal/bundle`, `GET /api/v1/bundle?hours=N`
   (installer, ≤168 h, audited). Identifiers come from the device's own
   data (snap01 SN, snap60 module SNs, the registration frame's ASCII
   field); each is replaced by a same-length pseudonym in every payload;
   RTC read responses lose their date; connections are renumbered; dates
   shift to 2000-01-01. The fail-closed scan checks every file for each
   identifier in ASCII and both hex cases. Deviation from the design:
   payloads, not raw wire bytes, are exported, so there is no CRC to
   recompute. Validated on a copy of the live database (2026-11-16: 26 875
   frames, 11 identifiers — inverter, datalogger, nine modules — and an
   independent fragment scan found no 15-character identifier; the only
   alphanumeric runs left are measurement words in snap02). Not yet: a
   `foxess-local bundle` CLI, the `compat/` corpus test over submitted
   bundles, guided cloud sessions in the UI.
