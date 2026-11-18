package gateway

import (
	"context"
	"errors"
	"testing"
)

var (
	pPct  = ParamPct
	pLoad = ParamLoad
)

// TestSettingFramesFn06: each installer setting is one fn06 write to its
// register, in table order (the encoding of the 2026-11-16 cloud writes:
// fn06 + verbatim echo — protocol profile §5).
func TestSettingFramesFn06(t *testing.T) {
	clk := &vclock{now: fixedNow()}
	gw, _ := newTestGW(t, clk)
	for _, tc := range []struct {
		cmd  Command
		want [][2]int // addr, value
	}{
		{Command{Kind: KindCurrentLimits, Params: map[Param]int{ParamImport: 550, ParamExport: 300}}, [][2]int{{41027, 550}, {41028, 300}}},
		{Command{Kind: KindBatteryCurrents, Params: map[Param]int{ParamDischarge: 450}}, [][2]int{{41008, 450}}},
		{Command{Kind: KindPowerLimit, Params: map[Param]int{pPct: 80}}, [][2]int{{41016, 80}}},
		{Command{Kind: KindPortLimits, Params: map[Param]int{pLoad: 10000, ParamGeneration: 7000}}, [][2]int{{41200, 10000}, {41201, 7000}}},
		{Command{Kind: KindEPSMode, Params: map[Param]int{pMode: 3}}, [][2]int{{41015, 3}}},
	} {
		frs, err := gw.BuildFrames(tc.cmd, nil, 0x10)
		if err != nil {
			t.Fatalf("%s: %v", tc.cmd.Kind, err)
		}
		if len(frs) != len(tc.want) {
			t.Fatalf("%s: %d frames, want %d", tc.cmd.Kind, len(frs), len(tc.want))
		}
		for i, w := range tc.want {
			p := frs[i].Payload
			if p[1] != fnWriteOne || int(p[2])<<8|int(p[3]) != w[0] || int(p[4])<<8|int(p[5]) != w[1] {
				t.Errorf("%s frame %d: %x, want fn06 @%d = %d", tc.cmd.Kind, i, p, w[0], w[1])
			}
		}
	}
}

// TestSettingBounds: the installer schema's ranges (wire units: 0.1 A,
// %, W); EPS mode is UPS (3) or EPS (2) only — "disabled" was never
// written and is refused.
func TestSettingBounds(t *testing.T) {
	clk := &vclock{now: fixedNow()}
	gw, _ := newTestGW(t, clk)
	bad := []Command{
		{Kind: KindCurrentLimits, Params: map[Param]int{ParamImport: 19}},
		{Kind: KindCurrentLimits, Params: map[Param]int{ParamExport: 1001}},
		{Kind: KindBatteryCurrents, Params: map[Param]int{ParamCharge: 501}},
		{Kind: KindBatteryCurrents, Params: map[Param]int{ParamCharge: -1}},
		{Kind: KindPowerLimit, Params: map[Param]int{pPct: 101}},
		{Kind: KindPortLimits, Params: map[Param]int{pLoad: 65536}},
		{Kind: KindEPSMode, Params: map[Param]int{pMode: 1}},
		{Kind: KindEPSMode, Params: map[Param]int{pMode: 0}},
		{Kind: KindPowerLimit, Params: map[Param]int{}},
		{Kind: KindPowerLimit, Params: map[Param]int{pPct: 50, {"other"}: 1}},
	}
	for _, c := range bad {
		if _, err := gw.Submit(context.Background(), c); err == nil {
			t.Errorf("%s %v accepted", c.Kind, c.Params)
		}
	}
	if _, err := gw.Submit(context.Background(), Command{Kind: KindEPSMode, Params: map[Param]int{pMode: 2}}); err != nil &&
		!errors.Is(err, ErrNoFlow) {
		t.Errorf("EPS mode 2 refused: %v", err)
	}
}

// TestSettingSkipUnchanged: the setting's registers are read first; only
// the one that differs is written.
func TestSettingSkipUnchanged(t *testing.T) {
	clk := &vclock{now: fixedNow()}
	gw, inj := newWearGW(t, clk, WithSkipUnchanged())
	id, err := gw.Submit(context.Background(), Command{Kind: KindPortLimits, Params: map[Param]int{pLoad: 10000, ParamGeneration: 7000}})
	if err != nil {
		t.Fatal(err)
	}
	if p := lastPDU(t, inj); p[pduFnOff] != fnRead || int(p[2])<<8|int(p[3]) != 41200 || p[5] != 2 {
		t.Fatalf("first frame %x, want the 41200 x2 read", p)
	}
	// 41200 = 10000 (unchanged), 41201 = 14300 (differs)
	if err := ack(gw, id, readRespPDU(2, map[int]uint16{0: 10000, 1: 14300})); err != nil {
		t.Fatal(err)
	}
	if n := len(inj.injected()); n != 2 {
		t.Fatalf("injected %d frames, want the read + one write", n)
	}
	if p := lastPDU(t, inj); int(p[2])<<8|int(p[3]) != 41201 {
		t.Fatalf("wrote %x, want 41201 only", p)
	}
	if err := ack(gw, id, fn06AckPDUVal(41201, 7000)); err != nil {
		t.Fatal(err)
	}
	if res := waitRes(t, gw, id); res.Status != StatusAcked || res.Unchanged {
		t.Fatalf("result %+v", res)
	}
}

// fn06AckPDUVal is the inverter's fn06 echo: 01 06 <addr> <value>.
func fn06AckPDUVal(addr, val int) []byte {
	p := make([]byte, 6)
	p[0], p[pduFnOff] = fnUnit, fnWriteOne
	u16BE(p, pduAddrOff, addr)
	u16BE(p, pduField2Off, val)
	return p
}
