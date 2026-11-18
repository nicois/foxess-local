package gateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"foxesslocal/internal/frame"
)

// ---------- shared helpers ----------

func fixedNow() time.Time { return time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC) }

// Short names for the params the tests use most.
var (
	pEnabled = ParamEnabled
	pMode    = ParamMode
	pValue   = ParamValue
	pImport  = ParamImport
	pExport  = ParamExport
)

// Short names for the schedule slot fields.
var (
	sEn       = SlotEnabled
	sStart    = SlotStart
	sEnd      = SlotEnd
	sWorkMode = SlotWorkMode
	sMaxSoc   = SlotMaxSoc
	sMinSoc   = SlotMinSoc
	sFdSoc    = SlotFdSoc
	sFdPwr    = SlotFdPwr
)

// slotKey is the schedule param of field f of slot k.
func slotKey(k int, f SlotField) Param { return SlotParam(k, f) }

// setSlot overwrites one field of slot k in the command.
func setSlot(c *Command, k int, f SlotField, v int) {
	c.Params[slotKey(k, f)] = v
}

// wantReadAddr is the schedule read set: 48000, 48010, the eleven mirrors.
var wantReadAddr = append([]int{regModeEn, regSlotTable}, mirrorBases...)

// rawsOf collects the raw bytes of a slice of frames.
func rawsOf(t *testing.T, frs []frame.Frame) [][]byte {
	t.Helper()
	out := make([][]byte, len(frs))
	for i := range frs {
		out[i] = frs[i].Raw
	}
	return out
}

// filterOut returns a copy of cur without registers matching keep.
func filterOut(cur []RegisterValue, keep func(uint16) bool) []RegisterValue {
	var out []RegisterValue
	for _, rv := range cur {
		if !keep(rv.Addr) {
			out = append(out, rv)
		}
	}
	return out
}

func isMissingCur(err error) bool { return errors.Is(err, ErrMissingCurrent) }

// writeAckPDU is the KH10 write-ACK shape (profile §5: 6 B, trailing 00
// omitted): 01 10 <addr> <cnt>.
func writeAckPDU(addr, cnt int) []byte {
	p := make([]byte, 6)
	p[0], p[pduFnOff] = fnUnit, fnWriteMulti
	u16BE(p, pduAddrOff, addr)
	u16BE(p, pduField2Off, cnt)
	return p
}

// readRespPDU is a standard fn03 response: 01 03 <bc> <data> (no address —
// the gateway correlates by request order).
func readRespPDU(cnt int, vals map[int]uint16) []byte {
	p := make([]byte, 3+2*cnt)
	p[0], p[pduFnOff] = fnUnit, fnRead
	//nolint:gosec // G115: byte count = 2*registers, bounded by blockSlotCnt.
	p[2] = byte(cnt * 2)
	for i := range cnt {
		u16BE(p, 3+2*i, int(vals[i]))
	}
	return p
}

// ack feeds an inverter-originated echo frame (device 0x91 lead) into the
// gateway.
func ack(gw *Gateway, id string, p []byte) error {
	return gw.Ack(id, frame.Frame{Device: invDev, Fn: defaultEnvFn, Payload: p})
}

// feedReadResponses feeds all 13 schedule read responses in request order.
func feedReadResponses(t *testing.T, gw *Gateway, id string, cur []RegisterValue) {
	t.Helper()
	for i, base := range wantReadAddr {
		cnt := blockModeEn
		if base != regModeEn {
			cnt = blockSlotCnt
		}
		if aerr := ack(gw, id, readRespPDU(cnt, blockVals(cur, base, cnt))); aerr != nil {
			t.Fatalf("ack read %d (base %d): %v", i, base, aerr)
		}
	}
}

// feedWriteAcks feeds the 13 schedule write ACKs.
func feedWriteAcks(t *testing.T, gw *Gateway, id string) {
	t.Helper()
	for _, base := range wantReadAddr {
		cnt := blockModeEn
		if base != regModeEn {
			cnt = blockSlotCnt
		}
		if aerr := ack(gw, id, writeAckPDU(base, cnt)); aerr != nil {
			t.Fatalf("ack write %d: %v", base, aerr)
		}
	}
}

// ---------- ACK correlation ----------

func TestAckWorkMode(t *testing.T) {
	clk := &vclock{now: fixedNow()}
	gw, inj := newTestGW(t, clk)
	gw.SetFlow("c1")

	id, err := gw.Submit(context.Background(), Command{Kind: KindWorkMode, Params: map[Param]int{pMode: 1}, Origin: "test"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if got := len(inj.injected()); got != 1 {
		t.Fatalf("injected %d frames, want 1", got)
	}
	// wrong value echoed -> fail
	if aerr := ack(gw, id, []byte{0x01, 0x06, 0xa0, 0x28, 0x00, 0x00}); aerr == nil {
		t.Fatalf("ack with wrong value: want error")
	}
	if st, ok := pendingStatus(gw, id); !ok || st != StatusFailed {
		t.Fatalf("after wrong ack: status = %q (ok=%v), want failed", st, ok)
	}
}

func TestAckLimitsPerWriteDone(t *testing.T) {
	clk := &vclock{now: fixedNow()}
	gw, inj := newTestGW(t, clk)
	gw.SetFlow("c1")

	id, err := gw.Submit(context.Background(), Command{Kind: KindLimits, Params: map[Param]int{pImport: 12500, pExport: 5000}})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	calls := inj.injected()
	if len(calls) != 2 {
		t.Fatalf("injected %d frames, want 2", len(calls))
	}
	// acks may arrive in any order; the command completes only after both
	if aerr := ack(gw, id, writeAckPDU(41023, 2)); aerr != nil {
		t.Fatalf("ack export: %v", aerr)
	}
	if st, _ := pendingStatus(gw, id); st != StatusInFlight {
		t.Fatalf("after one ack: status = %q, want in-flight", st)
	}
	if aerr := ack(gw, id, writeAckPDU(41021, 2)); aerr != nil {
		t.Fatalf("ack import: %v", aerr)
	}
	if _, ok := pendingStatus(gw, id); ok {
		t.Errorf("after both acks: command still pending, want acked/removed")
	}
}

func TestAckScheduleFullCycle(t *testing.T) {
	clk := &vclock{now: fixedNow()}
	gw, inj := newTestGW(t, clk)
	gw.SetFlow("c1")

	id, err := gw.Submit(context.Background(), baseSchedule())
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	calls := inj.injected()
	if len(calls) != 13 {
		t.Fatalf("injected %d frames, want 13 reads", len(calls))
	}

	// feed the 13 read responses in request order
	feedReadResponses(t, gw, id, currentSet())
	// all reads done -> the 13 write frames go out
	calls = inj.injected()
	if len(calls) != 26 {
		t.Fatalf("after last read: injected %d frames, want 26 (13+13)", len(calls))
	}
	var wraws [][]byte
	for _, c := range calls[13:] {
		wraws = append(wraws, c.raw)
	}
	writes := writeBlocks(t, wraws)
	if len(writes) != 13 {
		t.Fatalf("%d write blocks, want 13", len(writes))
	}
	for _, base := range wantReadAddr {
		if _, ok := writes[base]; !ok {
			t.Fatalf("missing write block at %d", base)
		}
	}
	feedWriteAcks(t, gw, id)
	if _, ok := pendingStatus(gw, id); ok {
		t.Errorf("after all acks: command still pending, want acked/removed")
	}
}

func TestAckScheduleWrongAddrFails(t *testing.T) {
	clk := &vclock{now: fixedNow()}
	gw, _ := newTestGW(t, clk)
	gw.SetFlow("c1")

	id, err := gw.Submit(context.Background(), baseSchedule())
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	// a write ACK for a register this command never writes
	if aerr := ack(gw, id, writeAckPDU(41021, 2)); aerr == nil {
		t.Fatalf("wrong-addr ack during read phase: want error")
	}
	if st, ok := pendingStatus(gw, id); !ok || st != StatusFailed {
		t.Errorf("after wrong-addr ack: status = %q (ok=%v), want failed", st, ok)
	}
}

func TestAckScheduleReadByteCountMismatch(t *testing.T) {
	clk := &vclock{now: fixedNow()}
	gw, _ := newTestGW(t, clk)
	gw.SetFlow("c1")

	id, err := gw.Submit(context.Background(), baseSchedule())
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	// first pending read is 48000×10 (bc=20); a 160-byte response mismatches
	if aerr := ack(gw, id, readRespPDU(80, blockVals(currentSet(), 48010, 80))); aerr == nil {
		t.Fatalf("byte-count mismatch: want error")
	}
	if st, ok := pendingStatus(gw, id); !ok || st != StatusFailed {
		t.Errorf("after mismatch: status = %q (ok=%v), want failed", st, ok)
	}
}

func TestAckUnknownCommand(t *testing.T) {
	clk := &vclock{now: fixedNow()}
	gw, _ := newTestGW(t, clk)
	gw.SetFlow("c1")
	if err := ack(gw, "cmd-0", writeAckPDU(41021, 2)); !errors.Is(err, ErrUnknownCommand) {
		t.Errorf("want ErrUnknownCommand, got %v", err)
	}
}

func TestAckCloudFrameIgnoredAsEcho(t *testing.T) {
	clk := &vclock{now: fixedNow()}
	gw, inj := newTestGW(t, clk)
	gw.SetFlow("c1")

	id, err := gw.Submit(context.Background(), Command{Kind: KindWorkMode, Params: map[Param]int{pMode: 1}})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	// a cloud-originated (device lead not 0x91) frame is not an inverter echo
	if aerr := gw.Ack(id, frame.Frame{
		Device: [4]byte{0x11, 0, 0, 0}, Fn: 0x10,
		Payload: []byte{0x01, 0x06, 0xa0, 0x28, 0x00, 0x01},
	}); aerr == nil {
		t.Fatalf("cloud-originated frame: want error")
	}
	if st, ok := pendingStatus(gw, id); !ok || st != StatusFailed {
		t.Errorf("status = %q (ok=%v), want failed", st, ok)
	}
	if got := len(inj.injected()); got != 1 {
		t.Errorf("injected %d frames, want 1", got)
	}
}

// a failed read-phase command leaves no in-flight slot; the queue drains.
func TestFailedCommandReleasesSlot(t *testing.T) {
	clk := &vclock{now: fixedNow()}
	gw, inj := newTestGW(t, clk)

	id1, _ := gw.Submit(context.Background(), Command{Kind: KindWorkMode, Params: map[Param]int{pMode: 1}})
	id2, _ := gw.Submit(context.Background(), Command{Kind: KindWorkMode, Params: map[Param]int{pMode: 0}})
	gw.SetFlow("c1")
	// id1 goes in flight, id2 stays queued
	if aerr := ack(gw, id1, writeAckPDU(41021, 2)); aerr == nil {
		t.Fatalf("wrong ack for id1: want error")
	}
	// id1 failed; the queued id2 must now start
	if st, _ := pendingStatus(gw, id2); st != StatusInFlight {
		t.Errorf("id2 status = %q, want in-flight", st)
	}
	if got := len(inj.injected()); got != 2 {
		t.Errorf("injected %d frames, want 2", got)
	}
	// complete id2 with its correct ack
	if aerr := ack(gw, id2, []byte{0x01, 0x06, 0xa0, 0x28, 0x00, 0x00}); aerr != nil {
		t.Fatalf("ack id2: %v", aerr)
	}
}
