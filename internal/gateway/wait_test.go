package gateway

import (
	"context"
	"errors"
	"testing"
	"time"
)

// waitResult bundles one Wait outcome for the channel handoff.
type waitResult struct {
	res Result
	err error
}

// wmFeedIn is the feed-in priority work-mode value (register 41000 = 1).
const wmFeedIn = 1

// fn06AckPDU is the KH10 fn06 write-ACK echo of the feed-in work-mode
// write: 01 06 <addr> <val> (profile §5: 6 B, trailing 00 omitted).
func fn06AckPDU(addr int) []byte {
	p := make([]byte, 6)
	p[0], p[pduFnOff] = fnUnit, fnWriteOne
	u16BE(p, pduAddrOff, addr)
	u16BE(p, pduField2Off, wmFeedIn)
	return p
}

// waitOne runs Wait on a goroutine and returns the outcome once known.
func waitOne(ctx context.Context, t *testing.T, gw *Gateway, id string) waitResult {
	t.Helper()
	ch := make(chan waitResult, 1)
	go func() {
		res, err := gw.Wait(ctx, id)
		ch <- waitResult{res: res, err: err}
	}()
	return <-ch
}

func submitWorkMode(t *testing.T, gw *Gateway) string {
	t.Helper()
	id, err := gw.Submit(context.Background(),
		Command{Kind: KindWorkMode, Params: map[Param]int{pMode: wmFeedIn}})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	return id
}

func TestWaitAcked(t *testing.T) {
	clk := &vclock{now: fixedNow()}
	gw, _ := newTestGW(t, clk)
	gw.SetFlow("c1")
	id := submitWorkMode(t, gw)
	// The inverter confirms the write before the Wait.
	if aerr := ack(gw, id, fn06AckPDU(regWorkMode)); aerr != nil {
		t.Fatalf("ack: %v", aerr)
	}
	wr := waitOne(context.Background(), t, gw, id)
	if wr.err != nil {
		t.Fatalf("Wait: %v", wr.err)
	}
	if wr.res.Status != StatusAcked {
		t.Fatalf("status: got %s, want acked", wr.res.Status)
	}
	if wr.res.ID != id || wr.res.Kind != KindWorkMode {
		t.Fatalf("result identity: got %+v, want id %s kind %s", wr.res, id, KindWorkMode)
	}
}

func TestWaitWakesOnAck(t *testing.T) {
	clk := &vclock{now: fixedNow()}
	gw, _ := newTestGW(t, clk)
	gw.SetFlow("c1")
	id := submitWorkMode(t, gw)
	// The ack lands after the Wait is running (the command is already
	// in flight when Submit returned, so a single ack suffices).
	go func() {
		for aerr := ack(gw, id, fn06AckPDU(regWorkMode)); aerr != nil; {
			// Ack raced ahead of the in-flight registration; retry.
			aerr = ack(gw, id, fn06AckPDU(regWorkMode))
		}
	}()
	wr := waitOne(context.Background(), t, gw, id)
	if wr.err != nil {
		t.Fatalf("Wait: %v", wr.err)
	}
	if wr.res.Status != StatusAcked {
		t.Fatalf("status: got %s, want acked", wr.res.Status)
	}
}

func TestWaitFailed(t *testing.T) {
	clk := &vclock{now: fixedNow()}
	gw, _ := newTestGW(t, clk)
	gw.SetFlow("c1")
	id := submitWorkMode(t, gw)
	// A mismatched echo (wrong address) fails the command.
	if aerr := ack(gw, id, fn06AckPDU(regMeterComp)); aerr == nil {
		t.Fatalf("mismatched ack: want error")
	}
	wr := waitOne(context.Background(), t, gw, id)
	if wr.err != nil {
		t.Fatalf("Wait: %v", wr.err)
	}
	if wr.res.Status != StatusFailed {
		t.Fatalf("status: got %s, want failed", wr.res.Status)
	}
	if wr.res.Detail == "" {
		t.Fatalf("failed result carries no detail")
	}
}

func TestWaitUnknownCommand(t *testing.T) {
	clk := &vclock{now: fixedNow()}
	gw, _ := newTestGW(t, clk)
	_, err := gw.Wait(context.Background(), "cmd-1")
	if !errors.Is(err, ErrUnknownCommand) {
		t.Fatalf("Wait: want ErrUnknownCommand, got %v", err)
	}
}

func TestWaitAckedConsumedOnce(t *testing.T) {
	clk := &vclock{now: fixedNow()}
	gw, _ := newTestGW(t, clk)
	gw.SetFlow("c1")
	id := submitWorkMode(t, gw)
	if aerr := ack(gw, id, fn06AckPDU(regWorkMode)); aerr != nil {
		t.Fatalf("ack: %v", aerr)
	}
	if wr := waitOne(context.Background(), t, gw, id); wr.res.Status != StatusAcked {
		t.Fatalf("first Wait: got %+v, want acked", wr.res)
	}
	// The acked result is consumed by the first Wait: a second Wait on
	// the same id is unknown (the id is not reused).
	_, err := gw.Wait(context.Background(), id)
	if !errors.Is(err, ErrUnknownCommand) {
		t.Fatalf("second Wait: want ErrUnknownCommand, got %v", err)
	}
}

func TestWaitExpired(t *testing.T) {
	clk := &vclock{now: fixedNow()}
	gw, _ := newTestGW(t, clk)
	// No flow: the command is queued and the virtual clock carries it
	// past the TTL (injected clock — C-004, no wall-clock).
	id, err := gw.Submit(context.Background(),
		Command{Kind: KindWorkMode, Params: map[Param]int{pMode: 0}})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	gw.SetTTL(time.Second)
	clk.advance(2 * time.Second)
	wr := waitOne(context.Background(), t, gw, id)
	if wr.err != nil {
		t.Fatalf("Wait: %v", wr.err)
	}
	if wr.res.Status != StatusExpired {
		t.Fatalf("status: got %s, want expired", wr.res.Status)
	}
}

func TestWaitCancelledContext(t *testing.T) {
	clk := &vclock{now: fixedNow()}
	gw, _ := newTestGW(t, clk)
	gw.SetFlow("c1")
	id := submitWorkMode(t, gw)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := gw.Wait(ctx, id)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait: want context.Canceled, got %v", err)
	}
}
