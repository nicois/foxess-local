package gateway

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// clockAt is a KindClock command for one wall time.
func clockAt(y, mo, d, h, mi, s int) Command {
	return Command{Kind: KindClock, Origin: "clock-sync", Params: map[Param]int{
		ParamYear: y, ParamMonth: mo, ParamDay: d, ParamHour: h, ParamMinute: mi, ParamSecond: s,
	}}
}

// TestClockFrame: the clock is one fn10 write of six registers at 40000
// (year, month, day, hour, minute, second): the cloud's own clock write,
// echoed by the inverter (profile §5, three pre-cutover sessions).
func TestClockFrame(t *testing.T) {
	clk := &vclock{now: fixedNow()}
	gw, _ := newTestGW(t, clk)
	frs, err := gw.BuildFrames(clockAt(2026, 11, 19, 16, 17, 26), nil, 0x10)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0x01, 0x10, 0x9c, 0x40, 0x00, 0x06, 0x0c,
		0x07, 0xea, 0x00, 0x0b, 0x00, 0x13, 0x00, 0x10, 0x00, 0x11, 0x00, 0x1a,
	}
	if len(frs) != 1 || string(frs[0].Payload[:len(want)]) != string(want) {
		t.Fatalf("got %d frames: %x", len(frs), frs[0].Payload)
	}
}

// TestClockBounds: every field is required and in range, and the date
// exists (the installer schema: year 2000–2099).
func TestClockBounds(t *testing.T) {
	clk := &vclock{now: fixedNow()}
	gw, _ := newTestGW(t, clk)
	gw.SetFlow("c1")
	missing := clockAt(2026, 11, 19, 16, 17, 26)
	delete(missing.Params, ParamSecond)
	for name, c := range map[string]Command{
		"year 1999":   clockAt(1999, 1, 1, 0, 0, 0),
		"year 2100":   clockAt(2100, 1, 1, 0, 0, 0),
		"month 13":    clockAt(2026, 13, 1, 0, 0, 0),
		"31 November": clockAt(2026, 11, 31, 0, 0, 0),
		"29 Feb 2026": clockAt(2026, 2, 29, 0, 0, 0),
		"hour 24":     clockAt(2026, 11, 19, 24, 0, 0),
		"minute 60":   clockAt(2026, 11, 19, 0, 60, 0),
		"second 60":   clockAt(2026, 11, 19, 0, 0, 60),
		"no second":   missing,
	} {
		if _, err := gw.Submit(context.Background(), c); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// TestClockNeverQueues: a clock value is only right when it is sent, so
// with no inverter session it is refused, never queued.
func TestClockNeverQueues(t *testing.T) {
	clk := &vclock{now: fixedNow()}
	gw, inj := newTestGW(t, clk)
	if _, err := gw.Submit(context.Background(), clockAt(2026, 11, 19, 16, 17, 26)); !errors.Is(err, ErrNoFlow) {
		t.Fatalf("no flow: %v, want ErrNoFlow", err)
	}
	if len(gw.Pending()) != 0 {
		t.Fatalf("queued: %+v", gw.Pending())
	}
	gw.SetFlow("c1")
	id, err := gw.Submit(context.Background(), clockAt(2026, 11, 19, 16, 17, 26))
	if err != nil || len(inj.injected()) != 1 {
		t.Fatalf("with a flow: %v, %d frames", err, len(inj.injected()))
	}
	if err := ack(gw, id, writeAckPDU(40000, 6)); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if _, ok := pendingStatus(gw, id); ok {
		t.Error("still pending after the echo")
	}
	if !slices.Contains(writeKinds, KindClock) {
		t.Error("the clock is not a budgeted write")
	}
}
