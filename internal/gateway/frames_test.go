package gateway

import (
	"encoding/binary"
	"fmt"
	"testing"

	"foxesslocal/internal/frame"
)

// ---------- shared test helpers ----------

var invDev = [4]byte{0x91, 0, 0, 0} // inverter-originated device lead (a read answer: 0x11 | 0x80; a write echo is 0x92)

func hex(b []byte) string { return fmt.Sprintf("%x", b) }

// pdu extracts the Modbus PDU from a raw 7f7f envelope frame.
func pdu(t *testing.T, raw []byte) []byte {
	t.Helper()
	if len(raw) < 13 {
		t.Fatalf("frame too short: %x", raw)
	}
	plen := int(raw[7])<<8 | int(raw[8])
	if len(raw) != 13+plen {
		t.Fatalf("frame length %d != 13+plen %d", len(raw), 13+plen)
	}
	return raw[9 : 9+plen]
}

// baseSchedule is the full 8-group table as read from the 2026-11-04 capture
// (session 1793779365405-2): slot 1 force-charge 11:01–13:59, slot 2
// force-discharge 18:00–20:00, slot 3 default 00:00–23:59 self-use, slots
// 4–8 disabled but carrying the inverter's held (non-zero) fields — a
// real GET /state → PUT /schedule round-trip reproduces exactly these
// values, so the no-op push has an empty Δ.
func baseSchedule() Command {
	c := Command{Kind: KindSchedule, Origin: "test", Params: map[Param]int{pEnabled: 1}}
	// add one slot: (k, en, start, end, workmode, minsoc, fdsoc, fdpwr);
	// maxsoc is 100 for every group in the capture.
	add := func(k, en, start, end, wm, minsoc, fdsoc, fdpwr int) {
		setSlot(&c, k, sEn, en)
		setSlot(&c, k, sStart, start)
		setSlot(&c, k, sEnd, end)
		setSlot(&c, k, sWorkMode, wm)
		setSlot(&c, k, sMaxSoc, socMax)
		setSlot(&c, k, sMinSoc, minsoc)
		setSlot(&c, k, sFdSoc, fdsoc)
		setSlot(&c, k, sFdPwr, fdpwr)
	}
	add(1, 1, 0x0B01, 0x0D3B, 6, 10, 100, 10500)
	add(2, 1, 0x1200, 0x1400, 7, 10, 50, 10500)
	add(3, 1, 0x0000, 0x173B, 1, 11, 10, 0)
	for _, k := range []int{4, 5, 6, 7, 8} {
		add(k, 0, 0, 0, 1, 10, 10, 0)
	}
	return c
}

// capture48000 / capture48010 are the fn03 read responses captured on
// 2026-11-04 (c2s, device 0x91): 48000×10 and 48010×80.
var capture48000 = [10]uint16{0x0001, 0, 0, 0, 0, 0, 0, 0, 0, 0}

var capture48010 = [80]uint16{
	// g1 force-charge 11:01-13:59
	0x0001, 0x0B01, 0x0D3B, 0x0006, 0x640A, 0x0064, 0x2904, 0x0000, 0x0003, 0x0002,
	// g2 force-discharge 18:00-20:00
	0x0001, 0x1200, 0x1400, 0x0007, 0x640A, 0x0032, 0x2904, 0x0000, 0x0003, 0x0002,
	// g3 default 00:00-23:59 self-use
	0x0001, 0x0000, 0x173B, 0x0001, 0x640B, 0x000A, 0x0000, 0x0000, 0x0000, 0x0002,
	// g4-g8 disabled
	0x0000, 0x0000, 0x0000, 0x0001, 0x640A, 0x000A, 0x0000, 0x0000, 0x0000, 0x0002,
	0x0000, 0x0000, 0x0000, 0x0001, 0x640A, 0x000A, 0x0000, 0x0000, 0x0000, 0x0002,
	0x0000, 0x0000, 0x0000, 0x0001, 0x640A, 0x000A, 0x0000, 0x0000, 0x0000, 0x0002,
	0x0000, 0x0000, 0x0000, 0x0001, 0x640A, 0x000A, 0x0000, 0x0000, 0x0000, 0x0002,
	0x0000, 0x0000, 0x0000, 0x0001, 0x640A, 0x000A, 0x0000, 0x0000, 0x0000, 0x0002,
}

// mirrorBases are the mirror ×80 blocks 48090–48890 step 80 (profile §5:
// eleven blocks; the 11-05 capture log's "all eleven mirror blocks").
var mirrorBases = []int{48090, 48170, 48250, 48330, 48410, 48490, 48570, 48650, 48730, 48810, 48890}

// currentSet scripts the read results the gateway would accumulate: the
// captured 48000/48010 blocks plus the eleven mirror blocks as described by
// profile §5 — all group enables 0, +4/+5/+6 mirroring the 48010 slot
// values, +8 = 0 (r28 not yet latched), +9 = 2.
func currentSet() []RegisterValue {
	var cur []RegisterValue
	for i, v := range capture48000 {
		cur = append(cur, RegisterValue{Addr: regAddr(48000 + i), Value: v})
	}
	for i, v := range capture48010 {
		cur = append(cur, RegisterValue{Addr: regAddr(48010 + i), Value: v})
	}
	for _, base := range mirrorBases {
		for k := range slotCount {
			g := []uint16{0, 0, 0, 0, capture48010[slotRegs*k+4], capture48010[slotRegs*k+5], capture48010[slotRegs*k+6], 0, 0, 2}
			for i, v := range g {
				cur = append(cur, RegisterValue{Addr: regAddr(base + slotRegs*k + i), Value: v})
			}
		}
	}
	return cur
}

// blockVals maps one ×N block out of a current set, keyed by register offset.
func blockVals(cur []RegisterValue, base, cnt int) map[int]uint16 {
	out := make(map[int]uint16, cnt)
	for _, rv := range cur {
		if int(rv.Addr) >= base && int(rv.Addr) < base+cnt {
			out[int(rv.Addr)-base] = rv.Value
		}
	}
	return out
}

// writeBlocks parses every fn10 write PDU in raws into addr -> offset -> value.
func writeBlocks(t *testing.T, raws [][]byte) map[int]map[int]uint16 {
	t.Helper()
	out := map[int]map[int]uint16{}
	for _, raw := range raws {
		p := pdu(t, raw)
		m, err := frame.ParseModbus(p)
		if err != nil {
			t.Fatalf("ParseModbus(%x): %v", p, err)
		}
		if m.Fn != 0x10 {
			continue
		}
		reg := make(map[int]uint16, m.Count)
		for i := range m.Count {
			reg[i] = binary.BigEndian.Uint16(m.Data[2*i : 2*i+2])
		}
		if _, dup := out[m.Addr]; dup {
			t.Fatalf("duplicate write block at %d", m.Addr)
		}
		out[m.Addr] = reg
	}
	return out
}

// delta is the per-push Δ (mon150 pushDiff semantics): offsets that changed
// vs the prior write to the same base.
func delta(cur, push map[int]uint16) map[int][2]uint16 {
	d := map[int][2]uint16{}
	for i, v := range push {
		if c, ok := cur[i]; ok && c != v {
			d[i] = [2]uint16{c, v}
		}
	}
	return d
}

// ---------- fn06 vs fn10 selection, byte-exact ----------

func TestFrameWorkModeByteExact(t *testing.T) {
	clk := &vclock{now: fixedNow()}
	gw, _ := newTestGW(t, clk)
	for mode, want := range map[int]string{
		0: "7f7f120000001000060106a0280000ba20f7f7",
		1: "7f7f120000001000060106a02800017be0f7f7",
	} {
		frs, err := gw.BuildFrames(Command{Kind: KindWorkMode, Params: map[Param]int{pMode: mode}}, nil, 0x10)
		if err != nil {
			t.Fatalf("mode %d: %v", mode, err)
		}
		if len(frs) != 1 {
			t.Fatalf("mode %d: %d frames, want 1", mode, len(frs))
		}
		if got := hex(frs[0].Raw); got != want {
			t.Errorf("mode %d: frame = %s, want %s", mode, got, want)
		}
		p := frs[0].Payload
		if len(p) != 6 || p[1] != 0x06 || int(p[2])<<8|int(p[3]) != 41000 {
			t.Errorf("mode %d: PDU %x, want fn06 @41000", mode, p)
		}
	}
}

func TestFrameMeterCompensationByteExact(t *testing.T) {
	clk := &vclock{now: fixedNow()}
	gw, _ := newTestGW(t, clk)
	for val, want := range map[int]string{
		-51:  "7f7f120000001000060106a045ffcdab98f7f7", // the §5 capture op (0xFFCD)
		500:  "7f7f120000001000060106a04501f42beaf7f7",
		-500: "7f7f120000001000060106a045fe0c6b98f7f7", // 0xFE0C two's complement
		0:    "7f7f120000001000060106a04500002bfdf7f7",
	} {
		frs, err := gw.BuildFrames(Command{Kind: KindMeterCompensation, Params: map[Param]int{pValue: val}}, nil, 0x10)
		if err != nil {
			t.Fatalf("value %d: %v", val, err)
		}
		if len(frs) != 1 {
			t.Fatalf("value %d: %d frames, want 1", val, len(frs))
		}
		if got := hex(frs[0].Raw); got != want {
			t.Errorf("value %d: frame = %s, want %s", val, got, want)
		}
	}
}

func TestFrameLimits(t *testing.T) {
	clk := &vclock{now: fixedNow()}
	gw, _ := newTestGW(t, clk)

	frs, err := gw.BuildFrames(Command{Kind: KindLimits, Params: map[Param]int{pImport: 12500}}, nil, 0x10)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	// a limit is a U32 across the pair, high word first: the cloud wrote
	// r0=0 r1=12500 (11-05 14:33 capture, profile §5)
	if len(frs) != 1 || hex(frs[0].Payload) != "0110a03d000204000030d4" {
		t.Errorf("import 12500: %d frames %s", len(frs), hexOrNone(frs))
	}

	frs, err = gw.BuildFrames(Command{Kind: KindLimits, Params: map[Param]int{pExport: 5000}}, nil, 0x10)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(frs) != 1 || hex(frs[0].Payload) != "0110a03f00020400001388" {
		t.Errorf("export 5000: %d frames %s", len(frs), hexOrNone(frs))
	}

	// both: import first, then export
	frs, err = gw.BuildFrames(Command{Kind: KindLimits, Params: map[Param]int{pImport: 12500, pExport: 5000}}, nil, 0x10)
	if err != nil {
		t.Fatalf("both: %v", err)
	}
	if len(frs) != 2 {
		t.Fatalf("both: %d frames, want 2", len(frs))
	}
	if got := hex(frs[0].Payload); got != "0110a03d000204000030d4" {
		t.Errorf("import PDU = %s", got)
	}
	if got := hex(frs[1].Payload); got != "0110a03f00020400001388" {
		t.Errorf("export PDU = %s", got)
	}
}

func hexOrNone(frs []frame.Frame) string {
	if len(frs) == 0 {
		return "none"
	}
	return hex(frs[0].Raw)
}

// ---------- schedule: read set ----------

func TestScheduleReadSet(t *testing.T) {
	clk := &vclock{now: fixedNow()}
	gw, _ := newTestGW(t, clk)
	frs, err := gw.BuildFrames(baseSchedule(), currentSet(), 0x10)
	if err != nil {
		t.Fatalf("BuildFrames: %v", err)
	}
	// 13 reads + 13 writes
	if len(frs) != 26 {
		t.Fatalf("%d frames, want 26 (13 reads + 13 writes)", len(frs))
	}
	reads := frs[:13]
	wantAddr := []int{48000, 48010, 48090, 48170, 48250, 48330, 48410, 48490, 48570, 48650, 48730, 48810, 48890}
	for i, f := range reads {
		m, perr := frame.ParseModbus(f.Payload)
		if perr != nil {
			t.Fatalf("read %d: %v", i, perr)
		}
		if m.Fn != 0x03 || m.Addr != wantAddr[i] {
			t.Errorf("read %d: fn=%#04x addr=%d, want fn03 @%d", i, m.Fn, m.Addr, wantAddr[i])
		}
		wantCnt := 10
		if wantAddr[i] != 48000 {
			wantCnt = 80
		}
		if m.Count != wantCnt {
			t.Errorf("read %d: count=%d, want %d", i, m.Count, wantCnt)
		}
	}
	// byte-exact against the §5 poll shapes (with the gateway's constant device field)
	if got := hex(reads[0].Raw); got != "7f7f110000001000060103bb80000a74e0f7f7" {
		t.Errorf("48000 read = %s", got)
	}
	if got := hex(reads[1].Raw); got != "7f7f110000001000060103bb8a0050d4d9f7f7" {
		t.Errorf("48010 read = %s", got)
	}
	if got := hex(reads[2].Raw); got != "7f7f110000001000060103bbda0050d4c8f7f7" {
		t.Errorf("48090 read = %s", got)
	}
}

// ---------- schedule: read-modify-write Δ signature ----------

// mirrorsOf indexes the current set's mirror blocks by base.
func mirrorsOf(cur []RegisterValue) map[int]map[int]uint16 {
	out := map[int]map[int]uint16{}
	for _, b := range mirrorBases {
		out[b] = blockVals(cur, b, blockSlotCnt)
	}
	return out
}

// wantDelta asserts a per-block Δ is exactly the expected set.
func wantDelta(t *testing.T, name string, d, want map[int][2]uint16) {
	t.Helper()
	if len(d) != len(want) {
		t.Fatalf("%s Δ = %v, want exactly %v", name, d, want)
	}
	for k, v := range want {
		if got, ok := d[k]; !ok || got != v {
			t.Errorf("%s r%d: Δ = %v, want %v", name, k, got, v)
		}
	}
}

// assertMirrorDelta checks every mirror block's Δ against the expected set.
func assertMirrorDelta(t *testing.T, writes map[int]map[int]uint16, cur []RegisterValue, want map[int][2]uint16) {
	t.Helper()
	for base, ccur := range mirrorsOf(cur) {
		p, ok := writes[base]
		if !ok {
			t.Fatalf("no write block at mirror %d", base)
		}
		wantDelta(t, fmt.Sprintf("mirror %d", base), delta(ccur, p), want)
	}
}

// pushedFrames builds the write phase of a schedule command (the frames
// after the 13 reads) and parses them into blocks.
func pushedFrames(t *testing.T, gw *Gateway, c Command, cur []RegisterValue) map[int]map[int]uint16 {
	t.Helper()
	frs, err := gw.BuildFrames(c, cur, defaultEnvFn)
	if err != nil {
		t.Fatalf("BuildFrames: %v", err)
	}
	if len(frs) != 26 {
		t.Fatalf("%d frames, want 26 (13 reads + 13 writes)", len(frs))
	}
	return writeBlocks(t, rawsOf(t, frs[13:]))
}

func TestScheduleDeltaSignature(t *testing.T) {
	clk := &vclock{now: fixedNow()}
	gw, _ := newTestGW(t, clk)
	cur := currentSet()

	// op: slot 1 window ±1 min + fd cut-off (mirrors the 14:21 installer op)
	c := baseSchedule()
	setSlot(&c, 1, sStart, 0x0B02)
	setSlot(&c, 1, sEnd, 0x0D3A)
	setSlot(&c, 1, sFdSoc, 99)
	setSlot(&c, 1, sFdPwr, 9948)
	writes := pushedFrames(t, gw, c, cur)

	// 48000: r0 = enabled, rest 0
	b48000, ok := writes[48000]
	if !ok {
		t.Fatalf("no write block at 48000")
	}
	if b48000[0] != 1 {
		t.Errorf("48000 r0 = %d, want 1 (enabled)", b48000[0])
	}
	for i := 1; i < 10; i++ {
		if b48000[i] != 0 {
			t.Errorf("48000 r%d = %d, want 0", i, b48000[i])
		}
	}

	// 48010: only the four target registers differ; +8/+9 preserved
	wantDelta(t, "48010", delta(blockVals(cur, 48010, blockSlotCnt), writes[48010]), map[int][2]uint16{
		1: {0x0B01, 0x0B02},
		2: {0x0D3B, 0x0D3A},
		5: {100, 99},
		6: {10500, 9948},
	})
	if writes[48010][8] != 3 || writes[48010][9] != 2 {
		t.Errorf("48010 g1 +8/+9 = %d/%d, want 3/2 (preserved)", writes[48010][8], writes[48010][9])
	}

	// mirrors: only slot-1 +5/+6 differ (+4 unchanged — max:min 100:10)
	assertMirrorDelta(t, writes, cur, map[int][2]uint16{
		5: {100, 99},
		6: {10500, 9948},
	})
}

func TestScheduleNoChangePushIsNoOp(t *testing.T) {
	clk := &vclock{now: fixedNow()}
	gw, _ := newTestGW(t, clk)
	cur := currentSet()
	frs, err := gw.BuildFrames(baseSchedule(), cur, 0x10)
	if err != nil {
		t.Fatalf("BuildFrames: %v", err)
	}
	writes := writeBlocks(t, rawsOf(t, frs[13:]))
	if d := delta(blockVals(cur, 48010, 80), writes[48010]); len(d) != 0 {
		t.Errorf("48010 no-op push Δ = %v, want empty", d)
	}
	for _, base := range mirrorBases {
		if d := delta(blockVals(cur, base, 80), writes[base]); len(d) != 0 {
			t.Errorf("mirror %d no-op push Δ = %v, want empty", base, d)
		}
	}
}

// ---------- r28 latch ----------

// assertR28 checks r28 across all mirrors: latched (1) only on the two latch
// blocks when latch == true, 0 elsewhere.
func assertR28(t *testing.T, writes map[int]map[int]uint16, latch bool) {
	t.Helper()
	for _, base := range mirrorBases {
		want := uint16(0)
		if latch && (base == 48090 || base == 48170) {
			want = 1
		}
		if got := writes[base][28]; got != want {
			t.Errorf("mirror %d r28 = %d, want %d", base, got, want)
		}
	}
}

// pushedCurrent is the register state after the first latch push: r28 = 1 on
// 48090/48170, 48010 r23/r24 as pushed (wm 2, packed 100:17).
func pushedCurrent() []RegisterValue {
	cur := currentSet()
	for i := range cur {
		switch cur[i].Addr {
		case 48090 + 28, 48170 + 28:
			cur[i].Value = 1
		case 48010 + 23:
			cur[i].Value = 2
		case 48010 + 24:
			cur[i].Value = 0x6411
		}
	}
	return cur
}

func TestScheduleR28Latch(t *testing.T) {
	clk := &vclock{now: fixedNow()}
	gw, _ := newTestGW(t, clk)

	// slot-3 default mode self-use → feed-in (the 14:22 op)
	c := baseSchedule()
	setSlot(&c, 3, sWorkMode, 2)
	setSlot(&c, 3, sMinSoc, 17) // FE default min_soc_on_grid (the operator's table)
	writes := pushedFrames(t, gw, c, currentSet())

	// 48010: r23 1→2, r24 0x640B→0x6411 (packed max:min)
	wantDelta(t, "48010 (mode change)",
		delta(blockVals(currentSet(), 48010, blockSlotCnt), writes[48010]),
		map[int][2]uint16{
			23: {1, 2},
			24: {0x640B, 0x6411},
		})
	assertR28(t, writes, true) // latched on exactly 48090 and 48170

	// revert (the 14:24 op): r23 2→1 — r28 must NOT revert
	writes2 := pushedFrames(t, gw, baseSchedule(), pushedCurrent())
	assertR28(t, writes2, true)
}

// ---------- missing current data ----------

func TestScheduleMissingCurrent(t *testing.T) {
	clk := &vclock{now: fixedNow()}
	gw, _ := newTestGW(t, clk)

	// missing a whole mirror block
	cur := filterOut(currentSet(), func(a uint16) bool { return int(a) >= 48410 && int(a) < 48490 })
	if _, err := gw.BuildFrames(baseSchedule(), cur, 0x10); !isMissingCur(err) {
		t.Errorf("missing mirror block 48410: %v, want ErrMissingCurrent", err)
	}
	// missing one 48010 register (a preserved +8)
	cur = filterOut(currentSet(), func(a uint16) bool { return a == 48010+18 })
	if _, err := gw.BuildFrames(baseSchedule(), cur, 0x10); !isMissingCur(err) {
		t.Errorf("missing 48010 r18: %v, want ErrMissingCurrent", err)
	}
	// nil current
	if _, err := gw.BuildFrames(baseSchedule(), nil, 0x10); !isMissingCur(err) {
		t.Errorf("nil current: %v, want ErrMissingCurrent", err)
	}
	// non-schedule kinds do not need current
	if _, err := gw.BuildFrames(Command{Kind: KindWorkMode, Params: map[Param]int{pMode: 1}}, nil, 0x10); err != nil {
		t.Errorf("workmode with nil current: %v", err)
	}
}

// ---------- envelope fn parameterization ----------

// fnAndCRCOnlyDiffers asserts a and b differ only at fn byte [6] and the two
// CRC bytes (the CRC covers the fn byte).
func fnAndCRCOnlyDiffers(t *testing.T, kind CommandKind, i int, a, b []byte) {
	t.Helper()
	if len(a) != len(b) {
		t.Fatalf("%v frame %d: length mismatch", kind, i)
	}
	plen := int(a[7])<<8 | int(a[8])
	crc0, crc1 := 9+plen, 10+plen
	for j := range a {
		if a[j] == b[j] {
			continue
		}
		if j != 6 && j != crc0 && j != crc1 {
			t.Errorf("%v frame %d: byte %d differs (%#02x vs %#02x), want only fn[6] and CRC", kind, i, j, a[j], b[j])
		}
	}
	if a[6] == b[6] {
		t.Errorf("%v frame %d: fn byte [6] did not change", kind, i)
	}
}

// compareEnvFn asserts two envFn builds differ only at fn byte [6] and the
// two CRC bytes (the CRC covers the fn byte).
func compareEnvFn(t *testing.T, gw *Gateway, c Command, cur []RegisterValue) {
	t.Helper()
	frs10, err := gw.BuildFrames(c, cur, 0x10)
	if err != nil {
		t.Fatalf("%v (0x10): %v", c.Kind, err)
	}
	frsC7, err := gw.BuildFrames(c, cur, 0xc7)
	if err != nil {
		t.Fatalf("%v (0xc7): %v", c.Kind, err)
	}
	if len(frs10) != len(frsC7) {
		t.Fatalf("%v: %d vs %d frames", c.Kind, len(frs10), len(frsC7))
	}
	for i := range frs10 {
		fnAndCRCOnlyDiffers(t, c.Kind, i, frs10[i].Raw, frsC7[i].Raw)
	}
}

func TestEnvelopeFnParameterization(t *testing.T) {
	clk := &vclock{now: fixedNow()}
	gw, _ := newTestGW(t, clk)
	cur := currentSet()

	for _, c := range []Command{
		baseSchedule(),
		{Kind: KindWorkMode, Params: map[Param]int{pMode: 1}},
		{Kind: KindLimits, Params: map[Param]int{pImport: 12500, pExport: 5000}},
		{Kind: KindMeterCompensation, Params: map[Param]int{pValue: -51}},
	} {
		compareEnvFn(t, gw, c, cur)
	}
	// byte-exact 0x10 vs 0xc7 for the §5 compensation op
	frs10, _ := gw.BuildFrames(Command{Kind: KindMeterCompensation, Params: map[Param]int{pValue: -51}}, nil, 0x10)
	frsC7, _ := gw.BuildFrames(Command{Kind: KindMeterCompensation, Params: map[Param]int{pValue: -51}}, nil, 0xc7)
	if got := hex(frs10[0].Raw); got != "7f7f120000001000060106a045ffcdab98f7f7" {
		t.Errorf("0x10 comp frame = %s", got)
	}
	if got := hex(frsC7[0].Raw); got != "7f7f12000000c700060106a045ffcd106df7f7" {
		t.Errorf("0xc7 comp frame = %s", got)
	}
}

// TestDeviceLeadByFunction (profile §5, 2026-11-20): the envelope's first
// device byte is the request type — 0x11 for a read (the inverter answers
// 0x91), 0x12 for a write (it echoes 0x92). Across 30 days of the real
// cloud's traffic every read (464) led 0x11 and every write (29 fn06, 91
// fn10) led 0x12; a write led 0x11 was silently ignored by the KH10
// (2026-11-19, 41015).
func TestDeviceLeadByFunction(t *testing.T) {
	cases := map[string]struct {
		pdu  []byte
		lead byte
	}{
		"fn03 read":  {fn03PDU(41015, 1), 0x11},
		"fn04 read":  {readPDU(0x04, 10016, 3), 0x11},
		"fn06 write": {fn06PDU(41015, 2), 0x12},
		"fn10 write": {fn10PDU(40000, []uint16{2026, 11, 20, 10, 0, 0}), 0x12},
	}
	for name, c := range cases {
		f := modbusFrame(0x10, c.pdu)
		if f.Device[0] != c.lead || f.Raw[2] != c.lead {
			t.Errorf("%s: device lead %#02x (raw %#02x), want %#02x", name, f.Device[0], f.Raw[2], c.lead)
		}
		if crc := frame.CRC16(f.Raw[2 : 9+len(c.pdu)]); binary.LittleEndian.Uint16(f.Raw[9+len(c.pdu):]) != crc {
			t.Errorf("%s: CRC not over the lead", name)
		}
	}
}
