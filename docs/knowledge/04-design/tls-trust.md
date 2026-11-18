---
project: foxesslocal
level: 4
feature: tls-trust
last_verified: 2026-11-08
traces_up: [02-constraints.md, 03-architecture.md]
traces_down: [05-coverage.md, 06-tests.md]
---
# Design: TLS trust strategy

The inverter validates the broker's certificate against its own trust store. What we can control is only the certificate we present — the anchor is whichever branch of T1–T5 the inverter's store happens to accept.

## Design decisions

### D-005: Anchor priority T1→T5, one validation gate
**[GATE RETIRED 2026-11-18]**: `certlab` was removed with the MQTT surface. The anchor question was settled by observation (the inverter trusts any CA — OQ-2; the T2 anchor is in use), and since Go 1.27.3 a Go client refuses the T2 chain outright, so a Go-side gate no longer models the inverter.

**Decision**: anchor candidates are tried in fixed priority order — T1 no-validation / inverter accepts anything; T2 leaf minted with a cracked eMudhra small key under the compromised intermediate; T3 leaf under a CA extracted from the inverter firmware; T4 inverter-side custom CA (if the inverter allows adding one); T5 full certificate pinning (only if the inverter supports it). Every branch is validated by the same `certlab` strict-client check before it is considered.
**Context**: OQ-2 (the inverter's actual trust anchor) is unresolvable without the inverter in front of us; the eMudhra incident (Oct 2026: 8 revoked intermediates, P-384 small keys `4, 56, 66, 115, 155, 156, 230, 237`; named cert `EM OV TLS CA – G3C`, key=4) makes T2 plausible if the inverter anchors at the root that signed the compromised intermediates.
**Rationale**: a single validation gate (`certlab`) means anchor choice is a measured fact, not a hypothesis; the priority order ranks by effort, so the cheapest branch that works wins.
**Priority served**: P-001
**Trades against**: P-005 (each deeper branch costs effort; T1 is cheapest, T5 deepest) — 1 > 5 ✓
**Classification**: pacing
**Alternatives considered**:
- skip straight to firmware CA extraction (T3): rejected — T1/T2 are strictly cheaper and the certlab gate tells us we never needed to go deeper.
**Traces**: P-001; plan 1 task 4 (`internal/certlab::TestCertLabAcceptsMatchingRoot`, `internal/certlab::TestCertLabRejectsUnknownRoot`); runbook 1b.1 (T2 mint + strict-client handshake check — T2 is the proven anchor, no triage ladder needed).
**[RESOLVED 2026-11-04]**: OQ-2 answered by the prox capture — **T2 accepted on the KH10**: forged iPAddress-SAN leaf (`8.209.116.72`) under the compromised intermediate **EM OV TLS CA - G3C**; `mitm_attempt: success`, 6997 s session, zero certificate events. T1 (self-signed) untested — moot while T2 stands (profile §2).

### D-006: eMudhra small-key verifier
**Decision**: `internal/emudhra` verifies whether a candidate certificate's P-384 public key equals `k*G` for one of the eight small-k values; `emudhra-check` CLI + CT-log fixture discovery make the check runnable against any cert chain.
**Context**: T2 requires proving "this intermediate was actually one of the compromised ones" before minting under it — the small-k test is the published fingerprint.
**Rationale**: pure crypto, no I/O, independently testable against CT-log fixtures; it de-risks T2 before any inverter is touched.
**Priority served**: P-001
**Trades against**: P-005 (verifier work before we know T2 is needed) — 1 > 5 ✓
**Classification**: pacing
**Alternatives considered**:
- trust the incident report's named intermediates without key verification: rejected — the CT-log discovery step exists precisely so the fixture set is observed, not copy-pasted.
**Traces**: P-001; plan 1 task 2 — `internal/emudhra::TestSmallKeyForFindsK4`, `::TestSmallKeyForEachCandidate`, `::TestSmallKeyForRejectsRandomKey`, `::TestSmallKeyForRejectsNonP384`.

### D-007: Anchor manifest (JSON) — cert switching is a data change
**Decision**: `certgen` records the chosen anchor kind, issuer, small-k (if T2), SANs, and validity in a JSON manifest; switching anchors is `certgen` (new kind) + restart, with no code change.
**Context**: the inverter's anchor may turn out to be unexpected; re-cutting with a new anchor must be a routine operation, not a re-deploy.
**Rationale**: the manifest is the audit trail of the trust decision and the input to every future certlab run.
**Priority served**: P-004
**Trades against**: P-005 (manifest plumbing up front) — 4 > 5 ✓
**Classification**: pacing
**Alternatives considered**:
- encode the anchor choice in TOML config only: rejected — the manifest also carries derived facts (issuer, small-k, SANs) that config would have to duplicate by hand.
**Traces**: P-004; plan 1 task 3 — `internal/certgen::TestManifestRoundTrip`.

### D-008: Broker cert 5-year validity, dual SANs mandatory
**[RENAMED 2026-11-18]**: "broker cert" = the inverter-facing leaf the app serves; the manifest location is `[certs] manifest` (formerly `[broker] cert_manifest`).

**Decision**: generated broker certificates are valid 5 years by default and must carry both the configured hostname(s) and IP SANs.
**Context**: the inverter connects to a fixed address on the LAN; operator attention between cutovers should be near zero. A cert expiring mid-year would force a re-cutover event.
**Rationale**: 5 years amortises the only recurring trust event; dual SANs cover DNS-based and IP-based connection styles without a second cert.
**Priority served**: P-004
**Trades against**: P-005 (validity is free; SANs are a config field) — none material; recorded as "none".
**Classification**: pacing
**Alternatives considered**:
- 90-day certs (CA-style hygiene): rejected — the re-cutover ceremony is manual, so short validity converts hygiene into recurring operator work (P-004 beats P-005 here).
**Traces**: P-004; plan 1 task 3 (certgen defaults + tests).

## Edge cases

- eMudhra's claim that "no end-entity certs were issued" from the compromised intermediates: what matters is the inverter's trust anchor, not the real cert's issuer (owner correction, accepted 2026-11-02) — if the inverter anchors at the root that signed the compromised intermediates, a leaf minted with a cracked key validates with no firmware work.
- The agent sandbox's probes of `*.foxesscloud.com` (flaky: 0 certs on `mqtt.foxesscloud.com:8883`, NXDOMAINs on EU/CN hosts) are **not** evidence — the real hostname and chain come from step-0 capture on the owner's LAN. Capture landed 2026-11-04: the cloud is reached by **raw IP** (SNI = `8.209.116.72` as an iPAddress SAN) — there is no hostname on this channel at all; the full chain dump remains an open item (profile §8, record only).
