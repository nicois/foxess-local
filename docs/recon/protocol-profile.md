# KH10 Protocol Profile

> **About the sources cited here.** This profile was built from packet
> captures of one installation, the owner's session logs and a cutover
> runbook. Those are not part of the public repository: the captures and
> logs identify a specific home and device. Citations to them (capture
> sessions, "cutover log §…", "runbook …", "live-env") are kept as
> provenance. Serial numbers in this document are placeholders
> (`…EXAMPLE…`); register addresses, values and firmware versions are as
> observed.

Filled from the 2026-11-04 prox capture (`captures/2026-11-04/`, session
`1793772365132-1`: 1793772365→1793779362, 6997 s ≈ 1 h 57 min; 86 837 B c2s /
216 B s2c, decoded with the prior's frame parser: **427 + 12 frames, 0 bytes
residual**).

**OQ-1 is resolved: the KH10 cloud uplink is a proprietary framed-binary
protocol over TCP/14431, not MQTT.** The spec's demotion path activates:
the service behind the egress gate is a frame-decoding proxy, not an MQTT
broker (spec D-013 anticipated exactly this).

## 0. Prior expectations → outcomes

| # | Prior expectation (M/Q family) | KH10 outcome |
|---|---|---|
| 1 | MQTT-over-TLS to `*.foxesscloud.com:8883` | **REFUTED.** Proprietary binary, TCP/14431, SNI=IP, no DNS name on the channel. |
| 2 | `7e7e`/`7f7f` framing, 4B device, func, BE len, CRC-16 (0xA001, init 0xFFFF, LE), ends `e7e7`/`f7f7` | **CONFIRMED.** Whole 86 837+216 B capture parses with zero residual; all CRCs valid. |
| 3 | Fixed ~238 B telemetry payload, offsets/scales per `telemetry.py` | **REFUTED as-shape.** KH10 uses a 135 B keepalive + a 6-frame/1839 B snapshot series; field-level decode is plan-2 work (§4). |
| 4 | Self-signed local-cloud cert accepted (T1) | **UNVERIFIED on KH10.** KH10 accepted a forged leaf under the compromised eMudhra intermediate (T2) — 1 h 57 min session, no rejection. T2 adopted. |
| 5 | Firmware updates travel as distinct frames on the same channel | **CONFIRMED as mechanism** (func `0xA2` magic `fa6b7c7a4d`, or 7f7f/`0x99` with `7c7a4d` tail). None observed in this session (§6). |

## 1. Endpoints

| Name | Host | Port | Purpose | Source |
|---|---|---|---|---|
| Cloud uplink | `8.209.116.72` (Aliyun) | 14431 | TCP+TLS, proprietary binary — the only observed channel | capture `conn_open`/`mitm_attempt`, 2026-11-04T06:06Z |
| Failover set (cloud-leaf SAN) | 11 Aliyun IPs incl. `47.91.86.144` | 14431 | not observed dialed on this device; possible failover | cloud-leaf SAN, 11-10 (§2.1) |
| NTP | ? | 123 | not observed | — |

The inverter dials the **raw IP** (no TLS SNI extension at all — the dialed IP rides in a proprietary block appended inside the ClientHello record, §2.1):
there is no cloud hostname on this channel, so DNS override is not needed
for interception — the egress gate/tproxy scope is the whole mechanism.
No other channel (no MQTT, no plain HTTP, no API) appeared in 6997 s of
continuous telemetry.

## 2. TLS

- Real cloud leaf (prior pin; fully extracted 11-10 from any raw-relay
  s2c.bin, record @72): SHA-256
  `0ff6d2d0b548f0a03dced31ce7621a8c9497bdc074d723bc152094e4d299c1b7`.
  **Self-signed** (issuer = subject) `C=CN, ST=JiangSu, L=Wuxi, O=FoxESS,
  CN=monitor`, RSA-2048 sha256WithRSAEncryption, valid 2024-06-14 →
  2124-05-21, no chain. SAN = the 11-IP failover set
  (`8.209.116.72 8.209.80.124 47.91.86.144 8.211.20.96 8.209.79.219
  47.254.142.98 47.254.159.190 101.133.233.208 47.102.203.29
  139.224.232.119 139.196.46.234`) + `*.maitian-yun.com` +
  `*.foxesscloud.com` — closes §8 "failover endpoint IP set" and
  "real cloud chain dump".
- **Accepted anchor: T2 — verified, but rejection is intermittent.** The
  inverter accepted a forged leaf (iPAddress SAN `8.209.116.72`) signed
  by the compromised eMudhra intermediate **EM OV TLS CA - G3C**; MITM
  succeeded (`mitm_attempt: success`) and the session ran 6997 s with
  zero certificate events. Nov tally: 19 attempts — 17 success, **2
  `client_rejected`** (11-04 14:51, 11-08 10:32 AEDT); each rejection
  writes a `patched` row that raw-relays until the 720 h recheck, so one
  transient rejection costs a month of MITM. Not cert pinning — **root
  cause: a prox bug, fixed 11-10** (owner-confirmed; MITM success
  verified on the following session).
- T1 (no validation / plain self-signed): untested on this KH10. T2
  requires no firmware work and is proven, so it is the adopted anchor.
- certlab (plan 1) remains the re-verification tool for the anchor; its
  shipped MQTT variant checks the optional LAN surface — the inverter-facing
  variant (bootstrap handshake, profile §3) lands with the plan-2 frame service.

### 2.1 Handshake dialect (11-10, from the threshold-6 raw-relay captures)

The inverter is a **TLS 1.2-only, non-conformant client**, and the cloud
answers with a matching 1.2 dialect flight:

- **ClientHello** (245 B record): 59 suites — ECDHE-RSA/ECDSA 1.2 list plus
  1.3 codes (`c02f`/`c030`/`c09c`/`c09d`) offered inside a 1.2 hello, no
  `supported_versions` ext; comp = `0x01` (zlib — invalid per RFC 5246);
  **no extension block (extlen = 0, hence no SNI)**; ends `00ff` SCSV; then
  a **proprietary 78 B block appended inside the same record** after the
  standard fields: `4d` tag + TLVs incl. the dialed endpoint IP in ASCII
  (`8.209.116.72`), a 16-entry named-curve list and a 6-entry sigalg list.
  Byte-identical across sessions except the random.
- **Server flight — 4-record / 1373 B variant** (every one of the 217
  raw-relay sessions of the 11-09 `s2c_max_packet: 6` experiment; s2c.bin
  @0): ServerHello(67; version 0303, RFC 8448 DOWNGRD\\1 sentinel,
  selected-suite field `c02f`, 23 B non-standard ext block) +
  Certificate(982; single 972 B DER leaf, §2 above) +
  ServerKeyExchange(300; ECDHE, P-256 `0x001d`, 65 B uncompressed point,
  server signature) + ServerHelloDone(4).
- **Server flight — 6-record / 1517 B variant** (healthy no-cap sessions):
  the same 4 records + **CertificateRequest(133; empty CA list — client
  certificate not required)** + Finished(1). The inverter's response flight
  carries no client Certificate (hs `0x16` 37 B dialect record, Finished,
  hs `0x00` 40 B record, then type-23 app data). The hellos are identical
  apart from the random, so the 4-vs-6 choice is server-side state.
- With a conformant client the same endpoint does standard **TLS 1.3**
  (live openssl probe 11-10: TLS_AES_128_GCM_SHA256, same cert) — the
  dialect flight is the server's accommodation of the inverter's hello.
- **Consequence for `s2c_max_packet`** (11-09 experiment, limit 6): in the
  raw-relay era s2c.bin is on-the-wire TLS, so the first s2c chunk of every
  session is the flight (1373 or 1517 B). Any cap below that drops the
  whole server flight → the inverter times out at 60 s → reconnect every
  ~63 s (217 sessions, all `s2c_drop 1373/6@0-1373`, zero app bytes). A cap
  ≥ the flight cannot hold 1024 B firmware chunks either — a size cap
  discriminates only in the MITM era, where s2c.bin holds decrypted app
  frames (trio 18/13/14 B, telemetry 150 B, config pushes, firmware).

## 3. Session / bootstrap

Transport: TCP + TLS. After handshake the inverter streams unidirectionally;
it kept uploading the full 2 h while the (proxied) cloud side went silent —
**cloud polling is not required to hold the session open.** The session
ended on the upstream side (cloud read timeout), not the inverter.

**2026-11-08 (active drop experiment, prox `isolation: readonly`)** — the
"unidirectional after handshake" claim above holds only for an *established*
session. Session *establishment* is gated on downlink delivery: with s2c
captured but never delivered to the inverter (prox read-only isolation,
enabled 11-08 07:52 local), the inverter degrades to a
**registration-retry loop** — it sends only the 36 B `2a` frame (no `2c`,
no `2b`, no keepalive, no snapshot burst, no telemetry), repeating every
**12.0 s** (790 frames / 9478 s and 844 frames / 10121 s across two
consecutive sessions; `mon150` class `register`, `tele=0`). The cloud —
which *does* receive the mirrored c2s — answers each `2a` with only the 18 B
resp1 ACK (`aa … 00 05 01 01 01 00 00`, resp1 of the trio below); no read
polls, no RTC sync, no session-control frames. Both sides sit in
"registration not completed". Consequences:

- The bootstrap burst (`2c`/`2b` + keepalive/451 B/snap series at ~T+6 s)
  and the 5 s telemetry start only after the cloud's bootstrap responses are
  **delivered** — a local cloud must generate *and deliver* the trio
  (resp2/resp3 load-bearing-ness: open item below) before uplink starts.
- A Wi-Fi reset does not clear the stuck state: it is a property of the s2c
  delivery mode, not of the TCP session; the retry counter persists across
  reconnects. (A one-off `client_rejected` MITM attempt on the first
  post-reset reconnect, 11-08 10:31, accepted on the next attempt —
  unrelated to isolation.)
- **Open item — resp1-only test:** does resp1 (`aa`) alone arm the burst,
  or does the inverter progress per exchange (resp2/resp3 also
  load-bearing)? Decisive test: deliver only the 18 B resp1 to the inverter
  and watch the next 12 s cycle — `2c`/`2b` + burst appearing = arms on
  first ACK; a new `2c`/`2b` 12 s retry loop = per-exchange gating. (The
  11-08 isolation session's 'resp1-only' cloud answer is a pre-cap-20
  reading; the 2026-11-15 per-write capture shows the full trio delivered
  per 2a on a fresh establishment.)
- framesvc implication: in **every** forward/hold mode the s2c bootstrap
  trio (+ keepalive traffic) must reach the inverter; hold/deny decisions
  queue frames and must never blackhole the downlink. prox `full`
  isolation (no upstream, no s2c) stalls the uplink the same way.

Three-exchange bootstrap, observed verbatim. Timing (11-04 session, byte
offsets + linear map, mtime-anchored): all 3 responses arrive **within the
first ~1.5 min** (they precede the first read poll in arrival order; that
poll's cfg91 echo sits at c2s offset 1,123 of 86,837 ≈ T+90 s); first poll
burst (48000/48010/10110) ≈ T+1.5 min, then 2-read polls at ~30 min
(T+31.5/61.6/91.6 min); last s2c byte at T+94 min (s2c.bin mtime), session
ended T+116 min. The local cloud must **generate** the 3 responses per the
pattern in the table below (the prior's computed func-offset/tail-subtraction
rules do **not** match this firmware and the inverter evidently does not
validate them):

| # | Dir | Frame | Notes |
|---|---|---|---|
| 1 | c2s | `7e7e 2a 00 7b c5 5f 00 17` + `01 00 01 11 0f` + `"609WEXAMPLE0001"` + `00 00 4c` | registration; `payload[4]` = 15 = serial length; raw 36 B |
| 2 | c2s | `7e7e 2c 00 7b ce 76 00 04 e1 01 04 00` | raw 17 B |
| 3 | c2s | `7e7e 2b 00 7b d3 0c 00 01 00` | raw 14 B |
| 3 responses | s2c | `7e7e <req_dev0\|0x80> 6a <S S> <q> <len2> <payload> <crc2> e7 e7` — resp1 `… 00 05` + `01 01 01 00 00` (18 B), resp2 `… 00 00` empty (13 B), resp3 `… 00 01` + `01` (14 B); 45 B total | device first byte = request's \| 0x80 (0x2a→0xaa, 0x2c→0xac, 0x2b→0xab). **Time-derived, not per-session constants**: `<S S>` = the 256 s slot number, `<q>` = the slot offset of the emission instant (2026-11-15 live capture, per-write ms timestamps: S=`f965` q=`b2` at unix 1794729394 — 1794729394/256 % 65536 = `f965`, % 256 = `b2` — all three responses share one (S, q) pair there; the corpus shows uniform triplets or +1 q stepping within a triplet — a per-response second-boundary stamping artifact, not a protocol field (confirmation below)). The earlier '2 per-session bytes / q steps +1 across the triplet' readings were pre-cap-20 linear-map reconstruction artifacts; the +12-per-12 s q stepping on retransmission (11-08) is the same time model at re-emission time. Constant: dev echo, leading `6a`, len + payload. Verified across the 11-session corpus plus the live capture; byte-exact generator `internal/frame.BootstrapTrio` / `TrioParamsAt` (pinned by tests) — the local cloud must **generate** these per the pattern, not replay recorded bytes |

**2026-11-15 live ground truth (emission algorithm)** — prox session 1794729390376-1 (per-write ms timestamps) against the current inverter state: the real cloud answered the first `2a` with the full trio **~4.2 s after conn open** (~2.5 s after the 2a; corpus envelope 5–8 s) as **three separate TCP writes 255–327 ms apart** (18/13/14 B), and the inverter established immediately (`2c` → `2b` → keepalive → telemetry). A trio delivered on the same tick as the `2a` as one 45 B write was **not picked up**: 32 h of 12 s re-registration loops against foxess-local's +0 ms single-write trio (telemetry_only, the real cloud's own responses denied by C-001). foxess-local now mirrors the cloud's algorithm: 4 s delay after the 2a, 300 ms between the three writes, and a fresh trio (fresh time-derived S/q) on **every** `2a` — the cloud re-emits on the inverter's 12 s re-request cadence and the re-requests stop once established. **Corpus confirmation (2026-11-04→11-15, 10 sessions)**: the time model holds for every trio ever captured — S = the slot of conn open (Δ = 0 in all 10), q = the slot offset **+4–9 s** after conn open (the cloud latency above — the inverter accepts the cloud's delayed answer, never a same-tick one). The triplet's q is uniform in 4/10 sessions (11-10, 11-11 s2/s3, 11-15 live) and steps +1 in the rest (5 pre-cap-20 + 11-11 s1: `d7`/`d8`/`d8`) — consistent with per-response stamping crossing a wall-clock second at the ~300 ms intra-triplet gap, not a protocol field; the inverter establishes with both forms when the trio is delayed, so the local generator keeps one (S, q) per triplet (`BootstrapTrio`, Deltas = 0). The pending 'session c q=ed' pin = 20261111/1794392805731-1 (uniform `ed`×3). Live re-verification pending the stage-1b cutover.

**Post-bootstrap cloud handshake (11-15 "new cloud" pattern)** — after the
trio, the cloud issues two 19 B Modbus reads within ~11 s of conn open
(per-write ts, 11-15 session): `0x9C40` (40000) ×6 (dev `1101ba0b`) at
2a+4.8 s, answered with a 15 B cfg91 frame 27 ms later; `0x277E` (10110)
×28 (dev `11e6ba0b`) at 2a+9.9 s, answered with a 59 B cfg91 frame 36 ms
later. The cloud then polls the **BB80/BB8A pair** (fn03 0xBB80 (48000)
×10 = the mode-scheduler block; fn03 0xBB8A (48010) ×80 = time groups 1–8,
§5) at t0+32.5 s and every **~30.6 min** after (7 pairs in the 11-15
session; counts verified with mon150 2026-11-16). The 11-11 "old cloud"
instead sent 70 B serial queries + a BB80/BB8A pair at +115 s and **no
0x9C40 read** — and produced zero wrapper frames for 48 h (§4.1).
0x9C40 ×6 is the inverter's **RTC block** (year/mon/day/h/m/s — §5 "RTC
write"; the 11-15 session also carries the cloud's time-sync write there,
19:41:41, dropped by cap-20): the first read is a clock poll. The meaning
of the 10110 ×28 input block is open (§8).

Registration serial `609WEXAMPLE0001` (15 c) is the **Wi-Fi/gateway module**
serial, not the inverter's. Device identities observed:

| Serial | Where | Role |
|---|---|---|
| `K1210460KB10EXAMPLE01` | 135 B keepalive frames | **KH10 inverter** (HA identity) |
| `60KB10EXAMPLE01` | snapshot 01/141 B frame | module |
| `609WEXAMPLE0001` | registration | gateway/Wi-Fi module |
| `60E5MEXAMPLE116`, `60E4SEXAMPLE105` | snapshot 60/280 B frame | battery candidates |

Firmware versions from the 01/141 B frame: `1.73 1.03 1.71 0.00` (×2 blocks);
model string `KH10`, module tag `KH`.

## 4. Telemetry (c2s) — 427 frames / 6997 s

| Class | Count | Cadence | Shape |
|---|---|---|---|
| Keepalive | 233 | ~30 s | `7f7f 06 6a eb <ctr>` func rolling ~0x1e, len 135: `"K1210460KB10EXAMPLE01"` + 115 × `00`. **Identical payload all session** — presence only. |
| Snapshot series | 158 (23 × 6 + overlap) | ~304 s (5 min) | 6 chunk frames sharing one func + device tail `6a eb <ctr>`; lead byte selects chunk: `01`/141, `02`/425, `04`/202, `60`/280, `61`/626, `65`/165 → 1839 B total |
| RSSI heartbeat | 23 | ~304 s, co-incident with snapshots | `7e7e 3f 00 <ctr BE>` func rolling, 16 B: 3 B header + JSON `{"RSSI":-59…-68}` |
| Modbus read responses | 9 | §5 | `7f7f 91 <tail>` func `0x10`, fn-03/04 read responses (`3+bc` B, bc even) |
| fn=64 wrapper | 1 (t0) | dense runs on the new-cloud handshake (542 in the 11-15 session) | `7f7f 37 …` dev0 `0x37`, 451 B payload = 13 B prefix + nested 7f7f frame — §4.1 |

Snapshot chunk contents (field-level decode = plan-2 codec task):

### 451 B fn=64 wrapper stream (2026-11-15/16)

Structure (byte-verified 11-16 on the 11-15 corpus): outer frame dev0 `0x37`; the envelope fn varies (0x64/0x83/…) and is **not** a discriminator — the classifier key is payload 451 + dev0 `0x37`. Payload = 13 B prefix (`6a` + inner S + zeros + `00 01 b6`) + one complete inner 7f7f frame (438 B: inner dev `02 6a …` rolls, inner fn varies, inner plen 425 carrying the SN header `60KB10EXAMPLE01` — the snap02 TLV payload in the first run). 13+438 = 451 exactly; frame scanners count wrapper and embedded frame separately.

Observed behaviour (11-15 session, 12 452 s span; 11-16 foxess-local sessions, first minutes):

- The 11-15 session carries **542 wrappers in 6 dense runs of ~97 byte-adjacent frames**, interleaved with the ~300 s bare snapshot-burst series (48 bare snap02 + 48 each of snap61/65/60/04/01); the runs are separated by 6 gaps (2628/2132/2095/1739/2071/2355 B). The earlier "3.1 s cadence" / "starts at t+31 s" readings were **mon150 clock artifacts** (end-anchored keepalive period; fixed 2026-11-16) — in byte order the first wrapper precedes the first initial read's response by ~26 s.
- The **queue-flush model is the working interpretation** (2026-11-16; hypothesis — fits all four data points, not yet proven by a decisive test): wrapper count = snapshot data accumulated since the last data flow, flushed in dense runs on (re)connect. Data points: 11 s outage → 1 wrapper; 9 min → 3; 48 h data-less (11-11, old-cloud handshake, below) → 542 (likely a cap); data-flowing (5 s telemetry on) → ~1 per session. The cloud's initial reads are not observably involved in any of the four.

Open (also §8): the flush trigger and cap, the purpose of the wrapped re-send, and why the old-cloud handshake produced zero wrappers for 48 h.

Snapshot chunk contents (field-level decode = plan-2 codec task):

- `01`/141 B: device ID — `60KB10EXAMPLE01` + 15 × `00` + `"1.73" "1.03" "1.71" "0.00"` (6 B each) + 2 × `00` + same four versions again + `00 01 4b 48` + 6 × `00` + `"KH10"` + 12 spaces + 10 × `00` + `27 10 00 3c` + zero tail.
- `60`/280 B: `60KB10EXAMPLE01` + `xeR` + battery serials `60E5MEXAMPLE116` / `60E4SEXAMPLE105` (×2) + binary.
- `02`/425 B: SN + version block + binary register data (variable fields from [33]; layout TBC). `04`/202, `65`/165 B: static identity/config across rounds. `61`/626 B: full TLV register map in the subsection below. Not compressed (no zlib header found).

### 150 B continuous telemetry (5 s cadence) — 2026-11-05 session

> **Presence depends on the cloud's handshake (2026-11-16).** The 5 s 150 B
> stream appears only in the 11-04/05 sessions (cloud-protocol variant A,
> cutover log §3.7). It is **absent** from every session since 11-11 —
> the 11-11 and 11-15 real-cloud sessions and every foxess-local session
> (including b126713 with the injected initial reads). Under the current
> handshake the inverter's c2s data is the ~5 min snapshot series + 30 s
> keepalives only. What re-enables the 5 s stream is open (§8).

`7f7f 5a 6a ec <ctr>` func `0xEC`, len `0x0096` (150 B): `"60KB10EXAMPLE01"` + power block. Decoded (values cross-checked against HA pushes within 0–3 W at fresh push age; both identities hold exactly in every frame, both charge/discharge polarities, >200 frames):

| Offset | Type | Field | Sign convention |
|---|---|---|---|
| [35:39] | s32 BE | PV generation (W) | + |
| [39:43] | s32 BE | grid at panel (W) | + import / − export |
| [43:47] | s32 BE | household load (W) | + (dup u16 at [65:67]) |
| [59:63] | s32 BE | inverter AC port (W) | + out / − in |
| [67:71] | s32 BE | battery (W) | + charge / − discharge |
| [71], [79] | u8 | battery SoC (%) | duplicated (always equal) |
| [72] | u8 | charge state | 04 charging / 02 idle / 03 (low batt, meaning open) |
| [73:75] | u16 | charge bit | 256 (bit8) while charging, else 0 |
| [75] | u8 | charge state machine | 0 idle/light, 1 batt idle (0 W), 5 force-charge active — inferred from transitions |
| [76:78] | u16 | unknown | constant 256 all session |
| [83], [97] | u8 | unknown | constant 0xFF all session |
| [149] | u8 | charge-burst flag | 0x7B set during active charge bursts, 0 otherwise |

Identities (near-exact — full-corpus check 2026-11-07: 0.7–2.1 % of tele frames deviate, almost all ±1 W from independent metering roundings, one 3 W, one 54 W transient; use as a sanity check, not an exact invariant):
- `grid = haus + batt − sol` (panel balance)
- `acport = sol − batt` (PV bypasses the AC stage; the AC port carries only the PV/battery net)

Topology (owner-confirmed): the KH10 inverts up to 10.5 kW AC; the household load sits on the panel in addition to the inverter's AC port; PV feeds the DC bus without inversion; max battery charge rate 11 kW regardless of source. Forced-charge steady state pins `batt ≈ +10 997…11 000 W`, `grid ≈ +7.6…+11 kW`. Charge-state flags at charge start: [72] `02`→`04`, [73:75] `0`→`256` (bit 8); no other status byte changed. SoC moved 31→47 % over the 35 min session.

### 626 B snapshot chunk (snap61) — TLV register map (2026-11-05, 7 live bursts)

Decoded from seven unsolicited bursts — A at 12:10 (charging @ 11 kW, soc 63) + six at 17:35–18:05 (discharging @ 6.3 kW, soc 95→88) — each cross-checked against HA cloud-ws values 1–2 s after the burst. TLV tags are u16 BE; value sub-fields are **little-endian** (verified against HA, 2026-11-07 — a BE decode gives garbage). Payload layout (byte offsets):

| Offset | Content |
|---|---|
| [0:15] | Inverter SN `60KB10EXAMPLE01` (ASCII) |
| [15:40] | Header (near-zero; `[32:34]` = `01 01`, `[37]` = `0x31`) |
| [40:292] | TLV table: 21 × 12 B entries (u16 tag + 10 B value) — tags `1872`–`187b`, `187f`, `1900`–`1909` |
| [292:412] | Zero (120 B) |
| [412:520] | Module table: 9 × 12 B entries (u16 tag `0c05`–`0c0d` + 10 B value) |
| [520:626] | Zero (106 B) |

Confirmed fields (multi-burst, state-diverse; offsets within the 10 B TLV value):

| Tag | Offset | Field | Evidence |
|---|---|---|---|
| `1905` | [6:8] u16 | **Battery SoC %** | 63/94/92/91/90/88/87 vs HA 63/95/93/92/91/89/88 (frame = HA − 1 in all six discharge bursts; exact in the charge burst — floor/round or one-step update lag) |
| `1907` | [0:2] u16 | **Stored-charge counter, proportional to SoC — unit undetermined** | counter ÷ SoC fraction is constant (970–1015 at 100 % over 131 frames; 620 @ 63 % … 863 @ 87.5 %; live 989 @ 100 %). **Correction 2026-11-16**: the earlier "50 Wh units → 49.2 kWh" reading was an assumed unit (only proportionality was ever checked; the "HA cross-check" covered SoC, not energy). The pack is a Fox ESS **EQ4800-L9** = 9 × 4.66 kWh = **41.93 kWh nominal, 100 % DoD** (owner) — matching the **9 module records** in snap60/snap61. 41 930 / 989 ≈ 42.4 Wh per count is not a plausible unit; one fit is 0.1 Ah counts (98.9 Ah vs the module's nominal ~91 Ah — over-provisioning reserved outside SoC; hypothesis). foxess-local reports usable energy as SoC × `battery.capacity_wh` (default 41930) and does not convert this counter. |
| `1903` | [6:8] u16 | ≈ SoC × 4 (0.25 % resolution; parallel SoC estimate) | 249/63 = 3.95 … 383/94.5 = 4.05 |
| `0c05`–`0c0d` | [6:8] u16 | **Pack DC-bus voltage, 0.01 V (scale TBC)** | 540.3–540.9 V while charging @ 11 kW vs 527.0–527.8 V while discharging @ 6.3 kW; all 9 records agree within ± 0.08 V; consistent with the 45 A max_charge_current installer parameter (11 kW @ 540 V = 20 A) |

Static across all 7 bursts: `1900` = `e8 03 61 10 00 00 3c 41 00 00`, `1901` (tail `33 3b`), `1909` (zero), `187f` = `01 01 01 01` + zero.
Slow monotone counters (direction confirmed, scale open): `1902` [0:2]/[2:4], `1904`, `1906`, `1907` [4:6], `1908` [4:6] (Δ −13 / burst), and the `1872`–`187b` group (1872 [4:6] steps 94→160→416→500 between states; 1877 [6:8] −0x1000; 1878 [0:2] +0x2000). `1872` [6:8] = 500 constant — matches export limit 5000 W / 10 (hypothesis; snap02 carries the limit at full scale).
`1905` [2:4] (1343→1372→1366) and [4:6] (16481→23649→23388): track charge/discharge state, scale open.

Resolved 2026-11-16: the **9 module records** are the 9 modules of the Fox ESS EQ4800-L9 pack (9 × 4.66 kWh = 41.93 kWh nominal); the 10th slot of the 280 B list is the duplicated master SN. Open: module-table sub-fields v[0:2], v[2:4] (char pair), v[4:6] vary per record — meaning open; voltage scale pending a concurrent Modbus DC-voltage read; `1902`/`1904`/`1908` counter meanings.

### Downstream protocol facts beyond 150 B telemetry (11-05)
- **K12104 carrier** (135 B; 450 frames across the full 3.5 h capture, ~30 s cadence): `K12104` + SN (15 B) + zero padding — periodic carrier/keepalive; K12104 is the installer-configuration protocol name ("protocol" field of the installer JSON parameter groups).
- **RSSI / event family** (`0b 01 <len> <ctl>` + JSON payload): RSSI 16 B `0b 01 0d 01` + `{"RSSI":-59}` (~5 min cadence, co-incident with the snapshot series); event messages e.g. 32 B `0b 01 1d 03` + `{"msgs":"485_data_abnormal"}` (RS485/battery data anomaly, one-off per session so far).
- **Device-info snapshot series** (inverter **pushes to the cloud, unsolicited** — no s2c request precedes a burst; the only nearby downlinks are the cloud's own Modbus polls (fn04 @10110, fn03 @48000/48010), whose responses are cfg91 frames, not snaps. 11-05 session: **cadence 292.94 s** (4 min 52.94 s, stable to ±0.001 s over 56 rounds 12:10:35→16:50:21, still running at write time; the 11-16 foxess-local sessions measure **exactly 300 s, phase-locked** to :x1:41/:x6:41 past the hour — cutover log §3.8 — so the period differs between eras/sessions; treat it as ~5 min, not a constant), plus a **bootstrap burst 6 s after conn open**; each burst is a multi-device sweep: snap01/141, snap02/425 ×2, snap04/202, snap60/280 ×1–2, snap61/626 ×1–2, snap65/165, occasionally + cfg91_59B). 141 B = SN + firmware 1.73/1.03/1.71 + "KH10"; 280 B = inverter SN + 10 module SN slots (60E5MEXAMPLE116 master ×2 + 60E4SEXAMPLE1xx); 425/626/202/165 B = further register blocks; the t0-only 23 B `01 00 …` frame carries data-logger SN 609WEXAMPLE0001.
- **Installer identity** (installer-portal view): device SN 60KB10EXAMPLE01 (KH10; serials in this document are placeholders), firmware Master 1.73 / Slave 1.03 / ARM 1.71, data logger 609WEXAMPLE0001.
- **K12104 window-block read responses** (103 B, 15 frames = 5 each of 57000/57050/57100): carry r0=2, r1=1, r2/r4 = 0x86A0 (= 100000−65536), r6 = 0x4E20 (= 20000) in a mixed-width encoding. Reads of the 15000/15050 blocks return 0xFF fill with a single field 0x07D0 (= 2000 W) — resolves the earlier “15000 r10 = 0x07D0” open item (block mostly unset).
- **451 B fn=64 wrapper**: 13 B prefix + one complete inner 7f7f frame (438 B; own marker, CRC, e7e7 terminator; inner plen 425 = the snap02 TLV payload in the first run) — §4.1; frame scanners count wrapper and embedded frame separately (per-session snap02 counts include the embedded one).
- **Named parameter groups (installer view, K12104 protocol)**: BasicParameters1/2, WorkModeParameters, ChargingTime, Safety*, G100Setting, AFCI, PeakShaving, Modbus-RTU/TCP, SegmentedTimeMode×2, EPS, Dry, Operation. Live basicparameters2 values: export_limit 5000 / import_limit 12500 W (both 0–30000), meter_compensation −51 (−500…500), max_charge_current 45 A (0–50), min/max SoC 0/100, max_soc_fromgrid 100, min_soc_on_grid 11, power_limit 100 %, EPS UpsMode 230 V, ground_protection On. Device rounds limit writes (4995→5000, 12501→12500; 50 W step fits both, unverified).

## 5. Commands (s2c) — 12 frames

| Class | Count | Purpose | Shape |
|---|---|---|---|
| Bootstrap ACKs | 3 | §3 | as observed |
| Modbus reads | 9 (3 cycles, ~39 min) | cloud polls registers | `7f7f 11 <tail>` func `0x10`, 6 B PDU: fn `0x03` @48000 ×10; fn `0x03` @48010 ×80; fn `0x04` @10110 ×28 |

Inverter echoes read responses with device `91 <same tail>` (request
device[0] \| 0x80). Poll blocks: **holding 48000/48010, input 10110**.

2026-11-05 additions (session `1793779365405-2`, s2c 4 861 B): same poll loop (`01 03 bb80 000a`, `01 03 bb8a 0050`, `01 04 277e 001c`) plus two new classes:
- **RTC write**: `01 10 9c40 0006 0c <yy><mo><d><h><mi><s>` — fn-`0x10` write-multiple of 6 u16 registers at **0x9C40 (40000)** = year/mon/day/h/m/s; observed `07ea 000b 0004 0015 0023 000c` (2026-11-04 21:35:12) and `07ea 000b 0005 0004 000f 000d` (2026-11-05 04:15:13) — the cloud re-syncs the inverter clock (values are not true local time; the inverter clock is simply off).
- **57 B serial query**: `7f7f 17 <tail>` func `0x10`, payload `"60KB10EXAMPLE01"` + zeros + `5a 00 05 00 05 00` — per-inverter status query every ~3–4 s.

The 00:00Z forced-charge start produced **no downlink frame** — it ran from the inverter's internal scheduler, not a cloud command.

### Downlink command map — 2026-11-05 (7-operation capture)

Seven app operations (12:21–12:26 local, session `1793840764468-1`) each produced a distinct downlink signature; all register values below are from that capture (parsed by `tools/mon150` replay mode).

**Protocol:**
1. **Config read-modify-write push.** For config changes the cloud first reads config blocks (fn03: 53400×16, 53100×10, 41007×40, 41000×1, 57000/15000/57050/15050/57100 ×50), then writes back the **entire** block set — 17–18 blocks per push — with only the changed registers differing. Written blocks: 48000×10, 48010×80, 48090…48890 step-80 (**eleven** ×80 blocks — the range is inclusive, (48890−48090)/80+1; the 14:21 capture log decodes "all eleven mirror blocks"), 57000/57050/57100 ×50 (57100 first seen 14:22), 15000/15050 ×50. (53400/53100 are read but never written back.)
2. **Targeted writes** for state toggles: 48000×10 (scheduler enable), fn06 41000 (work mode), fn06 41029 (meter compensation).
3. **Write ACK = `01 10 <addr> <cnt>` (6 B)** — the standard trailing `00` is omitted (KH10 deviation). Read responses are standard Modbus (`01 03 <bc> <data>`).
4. The 57 B serial query is answered by a 40 B frame (serial + `00 10` at [32:34]; dev `0x97`).

**Operation → downlink mapping (exact):**

| Op | Action | Downlink |
|---|---|---|
| 12:21 | disable future force-discharge window (18:00–20:00) | read set + full push (21 writes); 48010 r0=1 (charge still active), r10=0 |
| 12:22 | stop force charge → self-use | full push (20 writes, no preceding read); 48010 r0=0 |
| 12:23 | disable mode scheduler | 48000×10 with r0=0 + fn06 41000=0 |
| 12:24 | switch to feed-in priority | fn06 41000=1, then readback read 41000×1 |
| 12:24 | meter compensation 41 → 51 | fn06 41029 = 0xFFCD |
| 12:25 | switch back to mode scheduler | 48000×10 with r0=1 |
| 12:26 | enable future force-discharge + resume force charge | full push (21 writes); 48010 r0=1, r10=1 |

**Register map (validated by the exact diffs across the three push events):**

| Register | Meaning | Evidence |
|---|---|---|
| 48000 r0 (0xBB80) | mode-scheduler enable (0 off / 1 on) | 12:23:44 written 0 (disable scheduler) / 12:26:23 written 1 (re-enable); other 9 regs = 0 |
| 48010 r0 (0xBB8A) | time group 1 enable (spec "Time Group (1) Disable/Enable"; group 1 = the force-charge window, so this doubles as "FC active") | 12:23:11 1→0 (stop force charge) / 12:26:56 0→1 (resume) |
| 48010 r10 | time2 (force-discharge window) enable | 12:21 push r10=0 (window disabled) / 12:26:56 0→1 (window enabled) |
| 48010 r1 / r2 | time1 (force-charge) start / end, packed HHMM | 0x0B01/0x0D3B (11:01–13:59) → 14:21 0x0B02/0x0D3A (11:02–13:58, op change start/end by 1 minute); = JSON time1 11:02–13:58 ForceCharge |
| 48010 r5 / r6 | time1 fd_soc (cut-off SOC %) / fd_pwr (W) | 14:21 r5 100→99, r6 10500→9948 (op cut-off SOC 99%, limit charge to 9948W); = JSON time1 fd_soc 99, fd_pwr 9948 |
| 48010 r11 / r12 | time2 (force-discharge) start / end, packed HHMM | 13:29:24 r11 0x1200→0x1132 (18:00→17:50, op change force discharge start time); r12 0x1400 (20:00) stable; = JSON time2 17:50–20:00 ForceDischarge |
| 48010 r15 / r16 | time2 fd_soc / fd_pwr | 50 / 10500 stable; = JSON time2 fd_soc 50, fd_pwr 10500 |
| 48010 r21 / r22 | time3 (default-window) start / end, packed HHMM | 00:00 / 23:59 (0x0000/0x173B) stable; = JSON time3 00:00–23:59 SelfUse |
| 48010 r23 | default work mode: 1 SelfUse, 2 FeedInPriority | 14:22 1→2 (switch default to feed-in), 14:24 2→1 (revert) |
| 48010 r4/r14/r24 | **packed U8 MaxSoC : U8 MinSoCOnGrid** | high byte = max_soc, low byte = min_soc_on_grid: 0x640A = 100:10 (time1/time2), 0x640B = 100:11 (SelfUse), 0x6411 = 100:17 (FeedIn) — both bytes = JSON max_soc/min_soc_on_grid in all three slots; r24 tracks r23 both ways. (The earlier "sentinel 0x6400" was a misread: 0x64 = max_soc 100.) |
| 41000 (0xA028) | work mode: 0 self-use, 1 feed-in priority | fn06 12:23:44 = 0 / 12:24:28 = 1 + readback |
| 41021 (0xA03D) | import limit, W (installer range 0–30000) — **U32 across 41021:41022, high word first** (every captured write is r0=0 r1=W; resolved 2026-11-16 — a live read of 41007 ×40 gave 41021 = 0) | 14:32 fn10 write x2 = 12501 → device rewrites 12500 (14:33); JSON import_limit=12500; PDF 12500 (0~30000)W |
| 41023 (0xA03F) | export limit, W (installer range 0–30000) — **U32 across 41023:41024, high word first** (r0=0 r1=W) | 14:31 fn10 write x2 = 4995 → device rewrites 5000 (14:33); JSON export_limit=5000; PDF 5000 (0~30000)W |
| 41007–41039 (cloud write-through 2026-11-16) | **BasicParameters2 block — confirmed by cloud writes** (mirrored session 20:22–21:34 AEDT, run sheet `cloud-ops-plan-2026-11-16.md`; every write fn06 + verbatim echo, every value reverted and the post-session 41007 ×40 read byte-identical to the pre-session read). The installer schema (`dump-2026-11-16.json`, `k121__basic2`) lists the properties in register order. **Confirmed**: 41007 max charge current / 41008 max discharge current (0.1 A); 41009 min SoC; 41010 **max SoC from grid**; 41011 min SoC on grid; 41012 **max SoC** (write of 99 → exception 03 twice — hypothesis: max SoC must be ≥ max SoC from grid, 100 at the time); 41015 **EPS output** (3 = UpsMode, 2 = EpsMode; Disable untested, presumably 1); 41016 power limit %; 41021:41022 / 41023:41024 import / export limit W (U32, fn10 r0=0 r1=W); **41027 import limit current** (0.1 A: 550 = 55.0 A); **41028 export limit current** (0.1 A); 41029 meter compensation; 41032 standby load power threshold (W); **41037 EPS recover SoC** (fn06 → exception 02, illegal address on KH10 fw 1.73 — reads 0). **From schema order, unconfirmed**: 41013 sys on/off (1), 41014 EPS frequency (1 = 50 Hz), 41019 EPS rated voltage (0.1 V, 2300), 41020 EPS overload auto-recovery (1), 41030 grid-lost set, 41031 standby state, 41033–41036 PV anti-hiccups / 14 A trigger / EPS PF / fan, 41038 force-charge power. **Not assigned**: 41017 = 158, 41025 = 560 (not the import current limit), 41026 = 0. Supersedes the earlier misaligned 41007 ×40 decode. | frames table, conn 1794820938317-1; installer schema dump |
| 41200–41202 (2026-11-16) | `k121__limit_power`: 41200 LoadLimitPower W, 41201 GenerationLimitPower W, 41202 TBD (read 41200 ×5 = 14500, 14500, 0, 0, 0). Confirmed by cloud fn06 writes 41200 = 14400, 41201 = 14300. **LoadLimitPower caps the inverter's draw from its grid port — battery charging plus EPS pass-through combined** (measured 2026-11-17 force charge, fd_pwr 9948 W: at 6000 the grid-port intake (31008 − EPS) held 6.00 kW, and when a 3.8 kW EV load arrived on the UPS circuits the battery charge fell from 6.9 to 4.1 kW to keep it there; at 10000 it held 10.0 kW; the CT read intake + main-board load). GenerationLimitPower: presumably the output cap — untested. | frames table 21:31–21:32 AEDT |
| 41029 (0xA045) | meter compensation, int16 signed (-500..500 W) | 12:24 op 41→51 wrote 0xFFCD = -51 (two's complement); installer view meter_compensation -51 (-500~500)W. Earlier read 0x90FF for 41 inconsistent — unexplained |
| 57000 / 57050 / 57100 ×50 | **parameter blocks of schedule groups 1 / 2 / 3** (group k at 57000 + 50·(k−1)): r0 = 2 (constant), **r1:r2 import limit, r3:r4 export limit — U32 W, high word first** (0x0001:0x869F = 99999; the app's "no limit" is 99999 / 100000), **r6 = from-PV power** (20000 = "charge / discharge from PV" on, 0 = off), r49 stable per block (55810 / 9465; role open) | 2026-11-18 owner session (below): FC import limit 99999 → 35000 W = 57000 r1:r2 1:0x869F → 0:35000; FD export limit 100000 → 10000 W = 57050 r3:r4 1:0x86A0 → 0:10000; FC "charge from PV" off / on = 57000 r6 20000 → 0 → 20000; FD "discharge from PV" off = 57050 r6 20000 → 0; 57100 written only when the remaining-time (group 3) settings changed. Supersedes the 11-05 "value − 65536" reading (that was the low word of the U32). |
| 48010 +8 | **action after a force window's cut-off SoC: 1 standby, 3 self-use** | 2026-11-18 FC "after charging cutoff" standby → self-use = slot-1 r8 1 → 3; FD "after discharging cutoff" self-use reads 3. The eleven mirror blocks change 0 → 2 at the same moment (48090/48170: 1 → 3, like 48010) — their encoding is open. The gateway preserves +8/+9 from its read, so foxess-local's schedule writes keep it. |
| 48010 +6 on a force-charge group | **0 = no charging from the grid** | 2026-11-18: the app's "charge from grid" off wrote r6 9948 → 0; on wrote 0 → 10500 (the maximum, not the earlier 9948). There is no separate flag — resolves the 11-05 open item "grid-charge-disable flag". |

Envelope func is a **per-session constant** (0x10 in the 11-04 session, 0xc7 in the 11-05 session) — all s2c frames of a session share it. Three small session-control frames (fn 0x80/0x81) arrive at connection start. 53400×16, 53100×10 are read but never written back. 57100×50 (same layout as 57000/57050) is written (first seen 14:22 batch).

**Installer ranges (schema dump `dump-2026-11-16.json`).** The cloud's
parameter schema gives each setting's type, unit and allowed range. Any
future write path must use these bounds. Wire scale is what we observed,
not what the schema states: the schema shows amps and volts, the wire
carries 0.1 units.

| Register | Setting | Wire | Installer range | Written by foxess-local |
|---|---|---|---|---|
| 41000 | work mode | enum (0 self-use, 1 feed-in; schema also Backup, PeakShaving) | — | yes (self-use / feed-in) |
| 41007 / 41008 | max charge / discharge current | 0.1 A | 0–50 A | yes (2026-11-17, fn06) |
| 41009 | min SoC | % | 0–100 | no |
| 41010 | max SoC from grid | % | 10–100 | no |
| 41011 | min SoC on grid | % | 0–100 | no |
| 41012 | max SoC | % | 10–100 (rejects < max SoC from grid? unconfirmed) | no |
| 41015 | EPS output | enum 3 UpsMode / 2 EpsMode / Disable | — | yes, 3 / 2 only (2026-11-17, fn06; Disable never written) |
| 41016 | power limit | % | 0–100 | yes (2026-11-17, fn06) |
| 41019 | EPS rated voltage (unconfirmed) | 0.1 V | 150–250 V | no |
| 41021:22 / 41023:24 | import / export limit | U32 W | 0–30000 W | yes (0–30000) |
| 41027 / 41028 | import / export limit current | 0.1 A | 2–100 A | yes (2026-11-17, fn06) |
| 41029 | meter compensation | int16 W | −500–500 W | yes (−500–500) |
| 41032 | standby load power threshold | W | 20–200 W | no |
| 41037 | EPS recover SoC | % | 10–100 (illegal address on fw 1.73) | no |
| 41038 (unconfirmed) | force charge power | W | 500–20000 W | no |
| 41200 / 41201 | load / generation limit power | W | 0–65535 W | yes (2026-11-17, fn06) |
| 48010 groups | max SoC from grid, min SoC on grid, fd SoC | % | 10–100 | yes (enabled groups; 2026-11-16) |
| 48010 groups | fd power | W | 0–10500 W | yes (2026-11-16) |

The scheduler writes a cleared group in the cloud's unused-group shape
(00:00–00:00, work mode 1, 100:10, fd SoC 10, fd power 0), never as zeros.

**Segmented time mode structure (cfgscan + installer JSON, 11-05):** the mode scheduler is 16 named time slots — installer groups k121__segmented_time_mode1 (time1–8) and k121__segmented_time_mode2 (time9–16); each slot has enable, start/end hour-minute, work_mode (SelfUse/ForceCharge/ForceDischarge), fd_soc, fd_pwr, max_soc, min_soc_on_grid, extended_parameter_enable, parameter_enable. Live state: time1 = ForceCharge 11:02–13:58 (fd_soc 99, fd_pwr 9948, min_soc_on_grid 10), time2 = ForceDischarge 17:50–20:00 (fd_soc 50, fd_pwr 10500, min_soc_on_grid 10), time3 = SelfUse 00:00–23:59 (min_soc_on_grid 11; 17 when default mode = FeedIn), time4–16 disabled. Wire: **48010 = time groups 1–8** — per group of 10 registers (slot k at 48010+10·(k−1)): +0 enable, +1 start HHMM, +2 end HHMM, +3 work mode, +4 packed MaxSoC:MinSoCOnGrid, +5 fd_soc %, +6 fd_pwr W (0 on a force-charge group = no grid charging), +7 reserved (0), +8 action after the cut-off SoC (1 standby, 3 self-use — 2026-11-18; the default group carries 1), +9 = 2. The default "remaining time" mode is an ordinary enabled group spanning 00:00–23:59 (group 3 here). Cross-checked against the **official spec** — FoxESS Modbus Protocol V1.05.03.00 (2025-01-15; table footer V1.02.00.00), "FOX commercial inverter Modbus interface definition", **Table 3-11 "Time Period Table"** (public copy via `wimb0/home-assistant-nextenergy-battery-modbus` on GitHub; text at /tmp/foxess-modbus-spec.txt, re-download): identical layout except +4 MaxSoC and +5 MinSoCOnGrid are separate U8 registers with +6 FDSOC / +7 FDPWR — the KH10 fw 1.73 packs max:min into +4 and shifts the fd fields left by one. Spec work-mode enum (full): 1 Self-Use, 2 Feed-In, 3 BackUp, 4 Peak Shaving, 5 Power Station, 6 Force Charge, 7 Force Discharge. Spec allows 24 groups (base 48010+10·(N−1), N≤24; block layout 48010–48089 g1–8 / 48090–48169 g9–16 / 48170–48249 g17–24). **Mirror semantics (resolved 11-06 by per-push Δ diff):** the eleven 48090–48890 ×80 blocks are **not** groups 9–16 — every group enable there is 0 in every push. They are partial copies of the 48010 table receiving only the +4/+5/+6 field updates of each push (the 14:21 99/9948 change and the 14:22 0x6411 change appear in all eleven; the 13:27 r11 17:50 time change and the 14:22 r23 mode change appear only in 48010). 48090/48170 additionally receive the +8 updates ("trio"); the other nine blocks do not. Role not established (redundant storage / backup setpoints) — the cloud re-pushes them on every scheduler op, so the plan-2 gateway should replicate the same writes. 57000/57050/57100 ×50: r0 = 2, r1 = 1, r2 = per-window import limit (value−65536 when > 65536), r4 = 0x86A0 (100000, unchanged), r6 = 0x4E20 (20000), r49 = 0xD9F2 (55810, stable across the 11-05 window; 57100 r49 = 0; 15000/15050 r49 = 0xFFFF). 41007×40 readback: 41010/41012/41016/41017/41033 = 100 (percentages?), 41022 = 48, bitfields 41025 = 0x8802, 41026 = 0x3000, 41028 = 0x3001, 41030 = 0xD700.

**Capture log** (owner's action log: `tools/mon150/changes.txt`; downlinks from the named capture — do not re-attribute across windows):
- 11-04 session `1793779365405-2`: 7 ops (see op table; window fn 0x10).
- 11-05 12:21–12:26 session `1793840764468-1`: 7 ops (op table above; window fn 0xc7). Final state: scheduler on, force charge active, future discharge window enabled, work mode self-use… (feed-in set 12:24, unchanged after).
- 11-05 13:27 same session (observed 13:25–13:50): **1 op** — force-discharge start 18:00 → 17:50 → 13:29:24 48010 r11 0x1200 → 0x1132.
- 11-05 14:21–14:34 same session (**installer window**, ops per changes.txt):
  - 14:21 force-charge screen: start/end ±1 min (11:01–13:59 → 11:02–13:58), cut-off SOC 100→99, charge power → 9948 W, import limit → 99999 W, disable charge from grid → push: 48010 r1/r2/r5/r6, 57000 r2 0x86A0→0x869F (100000→99999 W), **all eleven mirror blocks** slot1 r5/r6 99/9948. The grid-charge-disable flag appeared in no pushed block (open item; likely the installer ChargingTime group, not dumped).
  - 14:22 default scheduler action SelfUse → FeedInPriority → 48010 r23 1→2, r24 0x640B→0x6411 (all ten ×80 blocks; 48090/48170 also r28 0→1); first 57100 write.
  - 14:24 revert → r23 2→1, r24 0x6411→0x640B (r28 not reverted — open item).
  - 14:25 installer login; 14:26 CSV export; 14:29 device-details and BasicParameters2 exports (not in the repository).
  - 14:31–14:33 export limit → 4995, import limit → 12501 → **fn10 write x2** @41023=4995 (14:31:59), @41021=12501 (14:32:15), then the device rewrites @41023=5000 (14:33:00), @41021=12500 (14:33:05) (limit rounding; a 50 W step fits both, unverified).
  - 14:34 JSON dumps of the named K12104 parameter groups with ranges (not in the repository; the ranges are recorded in §5).
- Cloud behaviour: re-reads 48000+48010 after ~30 min idle (13:00:21, read-only, no write). The installer ops produced no new c2s frame shapes — the new c2s frames below are periodic or session-start.

- **2026-11-18 00:29–00:44 UTC (owner session in `full` mode, notes `mode_scheduler.md`, local UTC+11).** Start: FC 11:02–13:58 target 80 %, after cut-off standby, grid 9948 W, PV on, import 99999 / export 100000; FD 17:50–20:00 target 50 %, after cut-off self-use, 10500 W, PV on; remaining time self-use 11–100 %. Each op was one full push (48010 + eleven mirrors + 57000/57050 [+57100] + 15050 [+15000]):
  - 11:35 remaining time → feed-in 22–90 % = 48010 r23 1 → 2, r24 → 0x5A16 (90:22) [00:35 push; 57100 written].
  - 11:37 FC target 85 %, after cut-off self-use, PV off, import 35 kW = r5 80 → 85, **r8 1 → 3**, **57000 r6 20000 → 0**, **57000 r1:r2 99999 → 35000**.
  - 11:39 FD PV off, export 10 kW, power 10076 W = **57050 r6 20000 → 0**, **57050 r3:r4 100000 → 10000**, r16 10500 → 10076.
  - 11:42 remaining time → self-use 11–100 %, FC grid off, PV on, target 81 % = r23 2 → 1, r24 → 0x640B, **r6 9948 → 0**, 57000 r6 0 → 20000, r5 → 81 [57100 written].
  - 11:44 FC grid on = r6 0 → 10500.
  15000 / 15050 ×50 were written unchanged every time (r3 = 1, r10 = 2000, r49 = 0xFFFF): role open.

**Open items (11-05; updated 2026-11-18):** ~~grid-charge-disable flag register~~ (resolved 2026-11-18: force-charge +6 = 0); limit-rounding step (set e.g. 4973 export → observe rewrite); the 14:26 CSV (save into tools/mon150/ if available); r28 semantics (set, not reverted, on mode switch — latch?); the mirror blocks' purpose (redundant storage vs backup setpoints); 41029 historical read 0x90FF for 41 (inconsistent with the signed encoding).

Not observed in this window: work-mode, schedule, config, export-limit
writes. The prior's validated control path (fn-`0x06` single-register write,
M1 firmware ≥ 1.80) is **gated off for this device** (KH10 fw 1.73 < 1.80)
— KH10 command registers are unknown (OQ-4, open item §8). Envelope func
for injected commands: real cloud used `0x10`; the prior injects with `0xE2`
(accepted on M/Q, untested on KH10).

### Live-value registers — KH_PRE133 map (2026-11-16, live probe)

The 5 s 150 B telemetry is absent under the current cloud handshake
(§4 note), so foxess-local **polls** these registers itself (gateway
`KindRead`, `OriginPoll`, every 5 s once the session is established —
`internal/framesvc/poll.go`, decoder `internal/frame/khrealtime.go`).

**Model string** (2026-11-16, settings poll): holding 30000 ×15 =
`'K' 'H' '1' '0'` one ASCII character per register, then 0x20 padding
→ `KH10` (the foxess_modbus decode). /state's `identity` matches the
verified profile `kh10-fw1.73`.

**Generation**: holding 30016–30018 = `0x173 / 0x103 / 0x171` = fw
master 1.73 / slave 1.03 / manager 1.71 (matches the snap01 firmware
string) → the **KH_PRE133** layout of the open-source foxess_modbus
integration (nathanmarlor/foxess_modbus). The other generations'
blocks answer **Modbus exception 2** (illegal address): input
10016 / 11000–11058 / 11096–11104, holding 36001, 37617–37632,
39070–39077, 39168–39169, 39279–39286.

Probe of 14:52:57 AEDT (sunny afternoon, battery full; raw: `docs/recon/probes/2026-11-16T145257-kh-live-values.txt`) —
`probe-registers.sh`; two reads cover it all: holding **31000 ×29**
and **31039 ×16** (fn03).

| reg | field | scale | probe | API mapping |
|---|---|---|---|---|
| 31000/31001/31002 | PV1 V / A / W | 0.1 V, 0.1 A, 1 W | 126.1 V, 15.8 A, 2003 W (V×A = 1992 ✓) | `pv_w` = Σ PV1–4 W |
| 31003/31004/31005 | PV2 V / A / W | same | 295.8 V, 9.2 A, 2723 W (✓) | |
| 31039–31044 | PV3/PV4 V / A / W | same | ~1 V, 0, 0 (unconnected) | |
| 31006 | grid voltage | 0.1 V | 242.6 V | |
| 31007 | inverter AC current | 0.1 A | 18.1 A | |
| 31008 / 31045:31046 | inverter AC power (s32 hi:lo) | 1 W, + = output | 4398 W (242.6 × 18.1 = 4391 ✓) | `ac_port_w` |
| 31009 | grid frequency | 0.01 Hz | 49.99 Hz | |
| 31010/31011/31013 | EPS V / A / Hz | 0.1 V, 0.1 A, 0.01 Hz | 241.8 V, 0.5 A, 50.00 Hz | |
| 31047:31048 / 31012 | EPS power | 1 W | 0 | `eps_w` (s32, + = output; 2026-11-17 live: EPS 238.6 V 50.0 Hz 0.5 A, 0 W in UPS mode 41015 = 3, nothing loaded — **31008 and 31053 include the EPS output** — verified 2026-11-17 09:38:26: a ~1.6 kW heat pump switched on a UPS circuit raised EPS 489→2093, inverter AC 1804→3329 and household load 1760→3300 alike; grid (−45 W) and DC−AC loss (≈ 340 W) unchanged. Grid-port output = 31008 − EPS; main-board load = 31053 − EPS) |
| 31014 | unknown — **not a temperature**: swings 35–51 between 5 s polls (2026-11-18) | | 65 | |
| 31018 / 31019 | inverter / ambient temp | 0.1 °C | 41.8 / 44.6 °C | history `t_inv` / `t_amb` |
| 31020 | "inverter battery voltage" (foxess_modbus) | 0.1 V | 422.1 V — **disagrees** with snap61 pack 537.7 V (10 LFP modules at full ≈ 540 V); meaning open | not used |
| 31021 | battery current | 0.1 A | 0 | |
| 31022 | battery power (s16) | 1 W, + = discharge | −28 W | `battery_w` = −value (+ = charging) |
| 31023 | battery temp | 0.1 °C | 27.6 °C | history `t_batt` |
| 31024 | battery SoC | % | 100 (snap61: 99–100) | `soc_pct` |
| 31025 / 31026 | BMS charge / discharge current limit | 0.1 A | 0 / 50.0 A (full: no charge) | history `bms_chg_a` (31025; the BMS derives it from battery temp + SoC) |
| 31027 | inverter state enum | | 3 (on-grid, normal) | |
| 31028 | BMS connected | | 1 | |
| 31049:31050 | grid CT (s32 hi:lo) | 1 W, + = import | −76 W (export) | `grid_w` |
| 31051:31052 | CT2 meter | 1 W | 0 | |
| 31053:31054 | household load (s32 hi:lo) | 1 W | 4326 W | `load_w` |

**Identities**: these are measured values, not the cloud model behind
the 150 B frame, so `acport = pv − batt` does **not** hold (4698 vs
4398 W: ~300 W DC→AC conversion loss) and neither does the old panel
identity. The measured balance that holds is **load ≈ ac_port + grid**
(4398 − 76 = 4322 vs 4326 W). The telemetry row's identity-delta
columns therefore carry the losses instead of ~0.

**Lifetime energy totals — 32000 ×24** (holding, fn03; foxess_modbus
KH_PRE133 / KH_133). Verified live 2026-11-19 07:42 AEDT against the
FoxESS cloud's cumulative sensors (via Home Assistant). Each total is
u32, **high word first**, 0.1 kWh, followed by a u16 "today" word (0.1
kWh since local midnight). Read every 30 s for `/state.energy`.

| regs | total | read | cloud (HA, earlier that morning) | API |
|---|---|---|---|---|
| 32000:32001 / 32002 | PV generation | 4429.4 kWh / today 0.2 | `PVEnergyTotal` 4429.2 | `pv_kwh` |
| 32003:32004 / 32005 | battery charge | 6368.5 / 0.0 | `chargeEnergyToTal` 6368.5 | `battery_charge_kwh` |
| 32006:32007 / 32008 | battery discharge | 7210.8 / 3.6 | `dischargeEnergyToTal` 7207.7 (01:02) | `battery_discharge_kwh` |
| 32009:32010 / 32011 | feed-in (export) | 2484.8 / 0.3 | `feedin` 2484.5 | `grid_export_kwh` |
| 32012:32013 / 32014 | grid consumption (import) | 5676.5 / 0.0 | `gridConsumption` 5676.5 | `grid_import_kwh` |
| 32015:32016 / 32017 | yield (inverter AC output) | 7573.1 / 1.8 | `generation` 7571.6 | `output_kwh` |
| 32018:32019 / 32020 | "input energy" | 4242.1 / 0.0 | — | not used (meaning open) |
| 32021:32022 / 32023 | load (household) | 6421.5 / 1.2 | `loads` 6420.5 | `load_kwh` |

Discharge and yield exceed 65 535 tenths, which confirms the word
order. The cloud's "generation" (labelled "Solarenergie" in the German
Home Assistant UI) is the yield total, not PV.

Open: 31014; 31020's meaning; the 31027 enum values; 32018 "input
energy"; stored energy (no KH_PRE133 register — snap61 stays its
source).

## 6. Firmware / update messages

Mechanism (prior `firmware.py`, same vendor platform): transfer frames carry
envelope func `0xA2` (7e7e family, magic `fa 6b 7c 7a 4d`) or func `0x99`
(7f7f family, 5-byte transfer ID with stable tail `7c 7a 4d`); metadata frame
(size, CRC, filename, serial) → inverter ACK → 1024 B chunks → ACK →
flash-progress signalling. **None observed in this session.**

Hold strategy (C-002), plan-2 decision:

- (a) **ACK-to-cloud + strip-from-inverter** — prior `FirmwareCapture`
  approach: cloud believes the update succeeded, inverter never enters
  updater mode. Proven prior implementation.
- (b) Silent drop — cloud may retry or time out; inverter stays on current
  fw.

## 7. Native schedule structure

**Resolved by the 11-05 induced capture (§5 command map).** Schedule delivery =
Modbus fn10 block pushes in `7f7f`/fn-c7 envelopes: 48000 (×10, scheduler
master flag) + 48010 (×80, groups 1–8) + eleven mirror ×80 blocks + four/five
×50 window blocks (57000/57050/[57100]/15000/15050) — a read-modify-write
full-state re-push on every scheduler op. Immediate toggles (work mode 41000,
compensation 41029) are fn06 single-register writes with fn03 read-back
verification; the limits (41021/41023) are fn10 ×2 writes of a U32, high
word first (r0=0, r1=W). The plan-2 scheduler maps HA schedule entities
onto the 48010 group layout and must replicate the mirror writes. The cloud
API exposes 8 groups (foxess-control `scheduler/get`); the installer UI
exposes 16 slots (`segmented_time_mode1/2`) — slots 9–16 have no distinct
wire location observed; treat the 48010 table (8 groups) as the canonical
surface.

## 8. Open items

- [x] Induced config capture (2026-11-05, 7 operations): §5 command map
      filled — command channel (Modbus PDUs in 7f7f/`0x10`), write-ACK shape,
      48000/48010/41000/41029 register semantics, read-modify-write push
      protocol.
- [x] Installer-window capture (2026-11-05 14:21–14:34, 8 operations incl.
      installer login/CSV/PDF/JSON): window times (48010 r1/r2/r11/r12/r21/r22,
      confirmed against segementedtimemode JSON), limits (41021 import / 41023
      export + 57000 r2 per-window), 41029 scale (int16 signed), default work
      mode r23, packed MaxSoC:MinSoCOnGrid at r4/r14/r24, K12104 parameter groups +
      installer identity. OQ-4 **largely resolved**; open items below remain.
- [ ] Grid-charge-disable flag register (ChargingTime group; ask owner to
      dump the installer ChargingTime JSON).
- [ ] Limit-rounding step (set e.g. export 4973 → observe device rewrite).
      2026-11-16: fn10 41023 = [0, 4973] was accepted and echoed verbatim,
      but nothing read 41023 before the revert, so it remains open (repeat
      with a read before reverting).
- [x] 48090–48890 ×80 blocks: partial mirror copies of the 48010 table (+4/+5/+6 field mirrors; +8 trio latch) — not groups 9–16 (enables always 0). Purpose still open (see §5).
- [ ] The 14:26 CSV export file (save into tools/mon150/ if available).
- [ ] Firmware transfer frames: capture on next cloud push, or accept the
      prior's model (validated on M/Q). Note: the 11-04 19:28 "storm" (16
      connections, 4–22 MB s2c each) was identified as **not inverter** —
      client 10.88.0.4, sni registry.npmjs.org (Cloudflare), tunnel-fallback
      ciphertext — so it is not a firmware push.
- [ ] Total s2c silence never observed in an established session: all 5
      established sessions have s2c (3 bootstrap responses + polls); the 13
      zero-byte sessions aborted with 0 B in both directions. Whether the
      inverter tolerates an endpoint that sends nothing after registration
      is untested.
- [x] Plan-2 bootstrap generation (2026-11-15): the local cloud replies
      to each 7e7e registration with the §3 pattern on the real cloud's
      algorithm (4 s delay, 300 ms segmented writes, per-2a re-emit,
      (S, q) time counters derived from the service clock — 556be26,
      3da6e73), plus the two initial reads (0x9C40/0x277E, corpus-verbatim)
      pinned byte-exact by the golden-transaction suite
      (`internal/framesvc/testdata/transactions/`); live-verified on the
      11-15/16 foxess-local sessions (trio accepted, reads answered
      27/36 ms, inverter established).
- [ ] Field-level decode of the snapshot chunk types (plan-2 codec): 11-05
      pass shows snap01/snap04/snap60/snap65 (141/202/280/165 B) are fully
      static across rounds (identity/config only). **snap61 (626 B) TLV map
      decoded 2026-11-05** (SoC %, a stored-charge counter — unit corrected
      to undetermined 2026-11-16, see §4 `1907` — the earlier "50 Wh units
      → 49.2 kWh" reading is withdrawn), DC-bus voltage candidate — see §4. Open: snap02 (425 B)
      layout, snap61 slow counters (1872–187b, 1902–1904, 1906–1908),
      module-table sub-fields. Cross-check against the
      **HA-oracle reconciliation**: the inverter's live HA sensor state
      (foxess-control integration, cloud-fed; owner-provided HA auth token)
      queried and compared lagged against the decoded frames/prox logs.
      Validates the decode before and while the local backend replaces the
      cloud feed. We query HA only — no websocket code on our side.
- [x] Real cloud chain dump (11-10): single self-signed 972 B leaf, no chain — full DER in any raw-relay s2c.bin record @72; fingerprint matches the prior pin (§2).
- [ ] NTP usage (inverter → UDP/123?): egress gate allows it low-risk;
      confirm on the gate's drop log.
- [x] Failover endpoint IP set (11-10): the cloud leaf's SAN **is** the
      set — 11 Aliyun IPs + `*.maitian-yun.com` + `*.foxesscloud.com`
      (full list §2.1). The gate must cover all 11 IPs on 14431; which
      besides `8.209.116.72` are actually dialed remains a runtime
      observation.
- [x] Role of the 451 B t0 frame: 13 B header + complete nested 425 B
      snap02 wire frame (own marker/CRC/terminator) — session init.
- [ ] T1 (self-signed) untested on KH10 — moot while T2 is adopted.
- [x] Root cause of the intermittent MITM `client_rejected` rejections
      (2/19 in Nov, §2): a **prox bug, fixed 2026-11-10** (owner-confirmed;
      MITM success on the following session) — not inverter cert pinning.
- [ ] Global min-SoC register: the cloud's `soc/set` (`{minSoc, minSocOnGrid}`) has no mapped register — the SoC pairs live per schedule slot (packed at 48010+4k, §5) with non-uniform live values (10/10/11/17), so a global push would clobber per-slot config. Candidate: the BasicParameters2 Modbus address (installer view shows `min/max SoC 0/100, min_soc_on_grid 11`); resolvable with one installer JSON dump + a concurrent Modbus read (the 41021/41023 method). Until resolved, task 8's sim holds the pair locally and never forwards it (task-8-report.md).
- [ ] BMS battery-temperature frame field unmapped — blocks non-empty `/dew/v0/device/detail?category=battery` data in the sim (task 8 returns an empty `battery` object; the integration degrades to "no BMS temperature").
- [ ] fn=64 wrapper stream semantics (§4.1): flush trigger and cap (542 in
      the 48 h data-less session — cap or backlog?), purpose of the wrapped
      re-send (redundancy vs a separate cloud consumer), and why the
      old-cloud handshake (11-11: serial queries, no 0x9C40) produced zero
      wrappers for 48 h. The queue-flush model is the working
      interpretation — decisive test: a time-boxed outage of known length
      and the resulting wrapper count (scaling check).
- [x] What re-enables the 5 s 150 B telemetry (resolved 2026-11-16): the
      cloud's **`serialquery`** (s2c, 57 B: SN, then bytes 40/42/44 =
      0x5A, 0x05, 0x05 — likely interval 5 s and duration 5 min). The
      inverter answers `cfg40` and streams 150 B telemetry every 5 s for
      ~300 s (09:35:56–09:40:51Z: 60 frames), then stops. The cloud sends
      it when the app opens the live view. The two earlier `serialquery`
      frames (03:54/04:00Z, `telemetry_only`) were dropped by policy, and no
      telemetry followed. Local re-arming every 5 min would restore the
      150 B stream without the cloud (not needed while the register poller
      runs).
- [x] The 10110 ×28 input block (fn04 0x277E — the second initial read,
      §3): **static, not live values** (2026-11-16). All 8 responses on
      record — 11-04 ×2, 11-05 ×4 (5 s telemetry running alongside),
      foxess-local 11-16 ×2 — are byte-identical:
      `003c 0000 0000 0000 fe00 0000×7 1c39 000e 2000 102f 7c40 0e00 0040 0000×9`.
      Status/config/identity content; meaning open, not needed for
      telemetry. (0x9C40 ×6, the first initial read, is the RTC block — §5.)
- [x] Live-value registers for local polling: **KH_PRE133** map,
      verified live 2026-11-16 (§5 "Live-value registers"); polled every
      5 s since. Original note — candidates from the
      open-source foxess_modbus KH map (three firmware generations:
      input 11000–11104 / holding 31000–31054 / holding 31000–31028 +
      39070–39286 + 37617–37632; version registers 10016 / 30016 / 36001).
      Probe with `POST /api/v1/registers/read`
      (`docs/recon/tools/probe-registers.sh`) and check against the live
      oracles: snap61 SoC + stored energy, pack voltage, PV ≈ 0 at night.

## 9. Tooling

**mon150** — source `docs/recon/tools/mon150/main.go` (single-file Go, stdlib only), binary at repo root (`./mon150`). The working recon tool for this capture corpus; use it before re-parsing captures by hand.

- Input: prox raw-payload dumps `/var/lib/prox/conns/<date>/<session>/c2s.bin` + `s2c.bin` (one dir per cloud connection, named by start timestamp ms).
- Live tail: `mon150 [-dur 60s]` — follows the newest capture, auto-switches on inverter reconnect / midnight rollover. DEF-004 fixed 11-06: `newestCapture()` globs `conns/<date>/<session>/c2s.bin` and mon150 fails loud when no capture is found (no silent fallback).
- Replay: `mon150 -from 12:20 -to 12:28 [-file …]` — merged, time-ordered c2s+s2c timeline. s2c timestamps: meta.jsonl per-write `bytes_s2c` ts (exact); `s2c_drop` ranges get a DROPPED annotation. c2s clock: correlated anchors (the inverter's cfg91 response to an s2c read is stamped at the read's write ts + 50 ms, which pins the handshake region) + the steady 30 s keepalive cadence end-anchored (cluster keepalive dropped from the period set), byte-linear interpolation between anchors; byte-fraction `~` only with <2 anchors. First-interval regression guard: `TestC2SClockResponseBrackets` (`main_test.go`). `-from/-to` accept HH:MM[:SS] or seconds since conn open.
- Decodes: 150 B power telemetry (KNOWN fields, grid/acport identity checks, HA-oracle cross-check with `HA_TOKEN=…`), 135 B K12104 keepalive (SN), 16 B RSSI JSON, t0 registration (logger SN), 6 B Modbus write/read echoes with register names, device-info snapshot series (141 B SN+fw+model, 280 B battery module SN list, 425 B inverter + 626 B battery status snapshots as hex dump with live HA state alongside — 626 B TLV map in §4, 425 B layout pending), 40 B SN frame, cfg91 (K12104 config / Modbus fn03 responses), downlink Modbus reads/writes annotated with the §5 register names, a per-push **Δ diff line** (registers changed vs the prior write to the same base — the scheduler push protocol is read-modify-write, so the Δ is the op signature), and a **`SLOTS[base]` line** for every ×80 48xxx block (eight groups: enable, window, work-mode short code, max:min soc, fd soc/pwr, non-zero reserved-tail registers).
- Build: `go build -o mon150 ./docs/recon/tools/mon150`. When a register meaning is added to §5, update `regName()` (and `printSlotTable`/`pushDiff` if the 48010 layout grows) in the same commit.
