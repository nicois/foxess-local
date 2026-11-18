#!/usr/bin/env python3
"""One-shot extraction of the golden-transaction cases into
internal/framesvc/testdata/transactions/.

Source: /var/lib/prox/conns (prox capture corpus on the gateway host).

No hand-computed value enters the output: frames are selected by
structural properties and sliced from the scanned stream; timestamps are
copied verbatim from each session's meta.jsonl (ts is MILLISECONDS); the
structural asserts (CRC16, markers, stamped-second identity) compare
computed values against computed values. The only external constant
referenced is trioDelay (4 s, internal/framesvc/service.go) in the
stamped-second assert, which encodes "the baseline's stamp second equals
what our service stamps at 2a + trioDelay". The trio's (S, fn) stamp is
u = 256*S + fn = unix_seconds % (256*65536) (frame/trio.go).
"""
import json
import os
import struct
import sys

ROOT = "/var/lib/prox/conns"
OUT = "./internal/framesvc/testdata/transactions"
TRIO_DELAY_MS = 4000  # service.go trioDelay
STAMP_MOD = 256 * 65536  # frame/trio.go: u = 256*S + fn wraps here
KEEPALIVE_DEV0 = 0x06
SESSION_DEV1 = 0x6A        # internal/frame: trio responses carry 0x6a in dev[1]


def die(msg):
    sys.exit("extract: " + msg)


def crc16(b):
    crc = 0xFFFF
    for x in b:
        crc ^= x
        for _ in range(8):
            crc = (crc >> 1) ^ 0xA001 if crc & 1 else crc >> 1
    return crc


def nibswap(b):
    return ((b & 0x0F) << 4) | (b >> 4)


def scan(data, what):
    """Scan a clean frame stream; assert markers/terminator/CRC on every
    frame; return frame dicts. Offsets are derived, never assumed."""
    frames = []
    off = 0
    while off < len(data):
        if off + 9 > len(data):
            die(f"{what}: truncated head at {off}")
        s = data[off]
        if s != data[off + 1] or s not in (0x7E, 0x7F):
            die(f"{what}: marker mismatch at {off}")
        fn = data[off + 6]
        plen = struct.unpack(">H", data[off + 7:off + 9])[0]
        total = 9 + plen + 4
        if off + total > len(data):
            die(f"{what}: frame at {off} overruns by {off + total - len(data)}")
        body = data[off + 2:off + 9 + plen]
        want = crc16(body)
        got = struct.unpack("<H", data[off + 9 + plen:off + total - 2])[0]
        if got != want:
            die(f"{what}: CRC at {off}: file {got:04x} != computed {want:04x}")
        term = data[off + total - 2:off + total]
        if term != bytes([nibswap(s), nibswap(s)]):
            die(f"{what}: terminator at {off}: {term.hex()}")
        frames.append(dict(
            off=off, end=off + total, start=s, fn=fn, plen=plen,
            dev=data[off + 2:off + 6],
            payload=data[off + 9:off + 9 + plen],
        ))
        off += total
    if off != len(data):
        die(f"{what}: unconsumed tail {len(data) - off} B")
    return frames


def meta(sess):
    with open(os.path.join(ROOT, sess, "meta.jsonl")) as f:
        lines = [json.loads(l) for l in f if l.strip()]
    opens = [l for l in lines if l.get("type") == "conn_open"]
    if not opens:
        die(f"{sess}: no conn_open in meta")
    t0 = opens[0]["ts"]
    s2c = [(l["start"], l["end"], l["ts"]) for l in lines
           if l.get("type") == "bytes_s2c"]
    return t0, s2c


def load(sess, dirn):
    with open(os.path.join(ROOT, sess, dirn + ".bin"), "rb") as f:
        return f.read()


def write_case(name, case, blobs):
    d = os.path.join(OUT, name)
    os.makedirs(d, exist_ok=True)
    with open(os.path.join(d, "case.json"), "w") as f:
        json.dump(case, f, indent=2)
        f.write("\n")
    for fname, b in blobs.items():
        with open(os.path.join(d, fname), "wb") as f:
            f.write(b)
    print(f"{name}: " + ", ".join(f"{k}={len(v)}B" for k, v in blobs.items()))


def pdu(f):
    p = f["payload"]
    if len(p) < 3:
        return None
    return dict(unit=p[0], fn=p[1], addr=struct.unpack(">H", p[2:4])[0]
                if len(p) >= 4 else None,
                count=struct.unpack(">H", p[4:6])[0] if len(p) >= 6 else None)


# ---------------------------------------------------------------- session A
A = "20261115/1794729390376-1"
t0a, s2c_ts_a = meta(A)
c2s_a = scan(load(A, "c2s"), "A c2s")
s2c_a = scan(load(A, "s2c"), "A s2c")

# The bootstrap registrations: the first three c2s frames, dev0 2a/2c/2b.
regs = c2s_a[:3]
for i, want_dev0 in enumerate((0x2A, 0x2C, 0x2B)):
    if regs[i]["dev"][0] != want_dev0:
        die(f"A c2s frame {i}: dev0 {regs[i]['dev'][0]:02x} != {want_dev0:02x}")
    if regs[i]["off"] != regs[i - 1]["end"] if i else regs[0]["off"] != 0:
        die("A c2s registrations are not contiguous from 0")

# The cloud's bootstrap trio: the first three s2c frames.
trio = s2c_a[:3]
if [len(x["payload"]) for x in trio] != [5, 0, 1]:
    die("A s2c trio payload lens != [5, 0, 1]")
for i, x in enumerate(trio):
    if x["dev"][0] != regs[i]["dev"][0] | 0x80 or x["dev"][1] != SESSION_DEV1:
        die(f"A s2c trio frame {i} dev {x['dev'].hex()} "
            f"!= {regs[i]['dev'][0] | 0x80:02x}6a..")
s1 = struct.unpack(">H", trio[0]["dev"][2:4])[0]
for i, x in enumerate(trio):
    s = struct.unpack(">H", x["dev"][2:4])[0]
    if s != s1 or x["fn"] != trio[0]["fn"]:
        die(f"A s2c trio frame {i}: stamp (S,fn) {(s, x['fn'])} != "
            f"frame 0 {(s1, trio[0]['fn'])}")
u = (256 * s1 + trio[0]["fn"]) % STAMP_MOD  # stamp, from bytes
u_expect = ((t0a + TRIO_DELAY_MS) // 1000) % STAMP_MOD
if u != u_expect:
    die(f"A trio stamp {u} != (t0+trioDelay) seconds mod {STAMP_MOD} "
        f"({u_expect})")

# The trigger reads: the s2c frames after the trio whose Modbus PDU is a
# read at the corpus addresses (fn03 @0x9C40 x6, fn04 @0x277E x28).
reads = [f for f in s2c_a if f["off"] >= trio[-1]["end"]]
reads = [f for f in reads if (lambda p: p and p["fn"] in (3, 4)
                              and p["addr"] in (0x9C40, 0x277E))
         (pdu(f))]
if len(reads) != 2:
    die(f"A s2c: expected 2 trigger reads, found {len(reads)}")
if pdu(reads[0])["addr"] != 0x9C40 or pdu(reads[0])["count"] != 6:
    die("A s2c read1 PDU != fn03 @0x9C40 x6")
if pdu(reads[1])["addr"] != 0x277E or pdu(reads[1])["count"] != 28:
    die("A s2c read2 PDU != fn04 @0x277E x28")
if any(len(x["payload"]) != 6 for x in reads):
    die("A s2c trigger read payload != 6 B")

# BB80/BB8A: the first fn03 read pair at 0xBB80 x10 + 0xBB8A x80 (the
# pair recurs on a ~31 min cadence; only the first is the bootstrap
# response to the wrapper stream starting).
bb = [f for f in s2c_a
      if (lambda p: p and p["fn"] == 3 and p["addr"] in (0xBB80, 0xBB8A))
      (pdu(f))]
if len(bb) < 2:
    die("A s2c: BB80/BB8A pair not found")
bb = bb[:2]
if bb[0]["end"] != bb[1]["off"]:
    die("A s2c: first BB80/BB8A matches are not an adjacent pair")
if pdu(bb[0])["addr"] != 0xBB80 or pdu(bb[0])["count"] != 10 \
        or pdu(bb[1])["addr"] != 0xBB8A or pdu(bb[1])["count"] != 80:
    die("A s2c: first BB80/BB8A pair PDU mismatch")

# The 32 B fn10 clock write.
cw = [f for f in s2c_a if (lambda p: p and p["fn"] == 0x10)(pdu(f))]
if len(cw) != 1 or cw[0]["plen"] != 19:
    die("A s2c: expected exactly one fn10 clock write (plen 19)")

# The keepalive carrier: first c2s frame with dev0 0x06.
ka = next((f for f in c2s_a if f["dev"][0] == KEEPALIVE_DEV0), None)
if ka is None or ka["plen"] != 135:
    die("A c2s: keepalive (dev0 0x06, plen 135) not found")

# ------------------------------------------------- case: bootstrap (off)
write_case(
    "bootstrap",
    {
        "session": A,
        "policy": "off",
        "epoch_ms": t0a,
        "inputs": [
            {"leg": "c2s", "file": "in1.bin", "at_us": 0},
            {"leg": "c2s", "file": "in2.bin", "at_us": 5000},
            {"leg": "c2s", "file": "in3.bin", "at_us": 10000},
        ],
        "want": [
            {"leg": "client", "file": "want1.bin", "after_input": 2},
            {"leg": "client", "file": "want2.bin", "after_input": 2},
            {"leg": "client", "file": "want3.bin", "after_input": 2},
            {"leg": "client", "file": "want4.bin", "after_input": 2},
            {"leg": "client", "file": "want5.bin", "after_input": 2},
        ],
        "audit": [
            {"decision": "forwarded", "dir": "c2s", "class": "register", "rel_to": 0},
            {"decision": "forwarded", "dir": "c2s", "class": "other", "rel_to": 1},
            {"decision": "forwarded", "dir": "c2s", "class": "other", "rel_to": 2},
            {"decision": "injected", "dir": "s2c", "origin": "bootstrap", "rel_to": 0},
            {"decision": "injected", "dir": "s2c", "origin": "bootstrap", "rel_to": 0},
            {"decision": "injected", "dir": "s2c", "origin": "bootstrap", "rel_to": 0},
            {"decision": "injected", "dir": "s2c", "origin": "trigread", "rel_to": 0},
            {"decision": "injected", "dir": "s2c", "origin": "trigread", "rel_to": 0},
        ],
        "note": ("The full registration burst (2a/2c/2b, meta carries no "
                 "per-c2s-frame ts, so 0/5/10 ms burst spacing is nominal) "
                 "and the service's whole local bootstrap response: the "
                 "trio (18/13/14 B) + the two trigger reads (19 B each). "
                 "Client-leg bytes are the baseline s2c bytes, byte-exact "
                 "including the stamp (u == floor((t0+trioDelay)/1s), "
                 "asserted at extraction)."),
        "extract": (f"python3 /tmp/extract_golden_tx.py; session {A}; c2s "
                    f"frames dev0 2a/2c/2b contiguous from 0; s2c frames 0-2 "
                    f"(trio, dev0 reg|0x80 + 6a, stamp u asserted) and the "
                    f"post-trio PDU reads @0x9C40/@0x277E"),
    },
    {
        "in1.bin": load(A, "c2s")[regs[0]["off"]:regs[0]["end"]],
        "in2.bin": load(A, "c2s")[regs[1]["off"]:regs[1]["end"]],
        "in3.bin": load(A, "c2s")[regs[2]["off"]:regs[2]["end"]],
        "want1.bin": load(A, "s2c")[trio[0]["off"]:trio[0]["end"]],
        "want2.bin": load(A, "s2c")[trio[1]["off"]:trio[1]["end"]],
        "want3.bin": load(A, "s2c")[trio[2]["off"]:trio[2]["end"]],
        "want4.bin": load(A, "s2c")[reads[0]["off"]:reads[0]["end"]],
        "want5.bin": load(A, "s2c")[reads[1]["off"]:reads[1]["end"]],
    },
)

# ------------------------------------------------ case: keepalive-denied (off)
write_case(
    "keepalive-denied",
    {
        "session": A,
        "policy": "off",
        "epoch_ms": t0a,
        "inputs": [{"leg": "c2s", "file": "in1.bin", "at_us": 0}],
        "want": [],
        "audit": [
            {"decision": "forwarded", "dir": "c2s", "class": "keepalive", "rel_to": 0},
        ],
        "silence_ms": 2000,
        "note": ("Off mode is byte-level deny: the keepalive is audited "
                 "forwarded (the c2s path has no s2c verdict) but nothing "
                 "reaches the cloud leg (endFlow wantC2S=0) and the client "
                 "leg stays silent (no upstream to relay)."),
        "extract": f"python3 /tmp/extract_golden_tx.py; session {A}; first c2s frame with dev0 0x06",
    },
    {"in1.bin": load(A, "c2s")[ka["off"]:ka["end"]]},
)

# --------------------------------------------- case: tele-forward (tele-only)
B = "20261105/1793875087573-1"
t0b, _ = meta(B)
c2s_b = scan(load(B, "c2s"), "B c2s")
tele = next((f for f in c2s_b if f["plen"] == 150), None)
if tele is None:
    die("B c2s: no 150 B-payload tele frame")

write_case(
    "tele-forward",
    {
        "session": B,
        "policy": "telemetry_only",
        "epoch_ms": t0b,
        "inputs": [{"leg": "c2s", "file": "in1.bin", "at_us": 0}],
        "want": [],
        "audit": [
            {"decision": "forwarded", "dir": "c2s", "class": "tele", "rel_to": 0},
        ],
        "note": ("telemetry_only mirrors the c2s uplink: the 150 B power "
                 "telemetry frame reaches the cloud leg byte-exact (the "
                 "mirror-identity expectation is computed from the input, "
                 "the audit row pins its class)."),
        "extract": f"python3 /tmp/extract_golden_tx.py; session {B}; first c2s frame with plen 150",
    },
    {"in1.bin": load(B, "c2s")[tele["off"]:tele["end"]]},
)

# ----------------------------------------- case: clockwrite-denied (tele-only)
write_case(
    "clockwrite-denied",
    {
        "session": A,
        "policy": "telemetry_only",
        "epoch_ms": t0a,
        "inputs": [
            {"leg": "c2s", "file": "in1.bin", "at_us": 0},
            {"leg": "s2c", "file": "in2.bin", "at_us": 10000},
        ],
        "want": [],
        "audit": [
            {"decision": "forwarded", "dir": "c2s", "class": "keepalive", "rel_to": 0},
            {"decision": "dropped", "dir": "s2c", "reason_prefix": "mode:telemetry_only deny:", "rel_to": 1},
        ],
        "silence_ms": 2000,
        "note": ("C-002: cloud-initiated inverter modification (the fn10 "
                 "clock write) is deny-all in telemetry_only — one dropped "
                 "audit row, client-leg silence."),
        "extract": f"python3 /tmp/extract_golden_tx.py; session {A}; first c2s dev0 0x06 (opens the flow); the s2c frame whose Modbus PDU fn is 0x10 (32 B, plen 19)",
    },
    {
        "in1.bin": load(A, "c2s")[ka["off"]:ka["end"]],
        "in2.bin": load(A, "s2c")[cw[0]["off"]:cw[0]["end"]],
    },
)

# -------------------------------------------------- case: bb80-forward (full)
def meta_ts(start, end):
    hit = [ts for (s, e, ts) in s2c_ts_a if s == start and e == end]
    if len(hit) != 1:
        die(f"A meta: no unique bytes_s2c chunk for [{start}:{end})")
    return hit[0]


# meta ts is ms; the drive offsets in case.json are us.
bb_gap_us = (meta_ts(bb[1]["off"], bb[1]["end"])
             - meta_ts(bb[0]["off"], bb[0]["end"])) * 1000
write_case(
    "bb80-forward",
    {
        "session": A,
        "policy": "full",
        "epoch_ms": t0a,
        "inputs": [
            {"leg": "c2s", "file": "in1.bin", "at_us": 0},
            {"leg": "s2c", "file": "in2.bin", "at_us": 10000},
            {"leg": "s2c", "file": "in3.bin", "at_us": 10000 + bb_gap_us},
        ],
        "want": [
            {"leg": "client", "file": "want1.bin", "after_input": 1},
            {"leg": "client", "file": "want2.bin", "after_input": 2},
        ],
        "audit": [
            {"decision": "forwarded", "dir": "c2s", "class": "keepalive", "rel_to": 0},
            {"decision": "forwarded", "dir": "s2c", "class": "mbread", "rel_to": 1},
            {"decision": "forwarded", "dir": "s2c", "class": "mbread", "rel_to": 2},
        ],
        "note": ("full mode relays s2c: the baseline's BB80/BB8A read pair "
                 "reaches the client byte-exact, in order, at the corpus "
                 "spacing (meta s2c timestamps). The keepalive opens the "
                 "flow (c2s ts absent from meta; 10 ms is nominal)."),
        "extract": f"python3 /tmp/extract_golden_tx.py; session {A}; first c2s dev0 0x06; s2c PDU fn03 @0xBB80 x10 then @0xBB8A x80; spacing from meta.jsonl",
    },
    {
        "in1.bin": load(A, "c2s")[ka["off"]:ka["end"]],
        "in2.bin": load(A, "s2c")[bb[0]["off"]:bb[0]["end"]],
        "in3.bin": load(A, "s2c")[bb[1]["off"]:bb[1]["end"]],
        "want1.bin": load(A, "s2c")[bb[0]["off"]:bb[0]["end"]],
        "want2.bin": load(A, "s2c")[bb[1]["off"]:bb[1]["end"]],
    },
)

print("epoch A =", t0a, " epoch B =", t0b,
      " trio S/fn = %04x/%02x" % (s1, trio[0]["fn"]),
      " bb_gap = %d us" % bb_gap_us)
