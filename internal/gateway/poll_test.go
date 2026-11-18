package gateway

import (
	"errors"
	"strings"
	"testing"
	"time"
)

const originAPI = "api"

var keyAddr = ParamAddr

func pollRead() Command {
	return Command{Kind: KindRead, Params: map[Param]int{ParamFn: 3, keyAddr: 31000, ParamCount: 1}, Origin: OriginPoll}
}

// TestOperatorQueuesBehindPoll: a poll read in flight never rejects an
// operator command — the command queues and starts the moment the
// read completes.
func TestOperatorQueuesBehindPoll(t *testing.T) {
	clk := &vclock{now: fixedNow()}
	gw, inj := newTestGW(t, clk)
	gw.SetFlow("c1")
	pid, err := gw.Submit(t.Context(), pollRead())
	if err != nil {
		t.Fatal(err)
	}
	oid, err := gw.Submit(t.Context(), Command{Kind: KindWorkMode, Params: map[Param]int{pMode: 1}, Origin: originAPI})
	if err != nil {
		t.Fatalf("operator submit behind a poll: %v", err)
	}
	if st, _ := pendingStatus(gw, oid); st != StatusQueued {
		t.Fatalf("operator command: %s, want queued behind the poll", st)
	}
	if err := ack(gw, pid, []byte{0x01, 0x03, 0x02, 0x00, 0x01}); err != nil {
		t.Fatal(err)
	}
	if st, _ := pendingStatus(gw, oid); st != StatusInFlight {
		t.Fatalf("operator command after the poll completed: %s, want in-flight", st)
	}
	if n := len(inj.injected()); n != 2 {
		t.Fatalf("injected %d frames, want 2 (poll read + work-mode write)", n)
	}
}

// TestPollYieldsToInFlight: a poll never queues — with anything in
// flight it is refused (the poller skips that tick).
func TestPollYieldsToInFlight(t *testing.T) {
	clk := &vclock{now: fixedNow()}
	gw, _ := newTestGW(t, clk)
	gw.SetFlow("c1")
	if _, err := gw.Submit(t.Context(), Command{Kind: KindWorkMode, Params: map[Param]int{pMode: 1}, Origin: originAPI}); err != nil {
		t.Fatal(err)
	}
	if _, err := gw.Submit(t.Context(), pollRead()); !errors.Is(err, ErrInFlight) {
		t.Fatalf("poll behind an operator command: got %v, want ErrInFlight", err)
	}
}

// TestPollRequiresFlow: a poll with no inverter flow is refused, never
// queued (it would pile up for the queue TTL during an outage).
func TestPollRequiresFlow(t *testing.T) {
	clk := &vclock{now: fixedNow()}
	gw, _ := newTestGW(t, clk)
	if gw.HasFlow() {
		t.Fatal("HasFlow before SetFlow")
	}
	if _, err := gw.Submit(t.Context(), pollRead()); !errors.Is(err, ErrNoFlow) {
		t.Fatalf("poll with no flow: got %v, want ErrNoFlow", err)
	}
	if n := len(gw.Pending()); n != 0 {
		t.Fatalf("a refused poll left %d pending entries", n)
	}
	gw.SetFlow("c1")
	if !gw.HasFlow() {
		t.Fatal("HasFlow after SetFlow")
	}
}

// TestInFlightTimeoutFreesSlot: a command the inverter never answers
// fails after inFlightTTL instead of wedging the gateway; the next
// submit proceeds.
func TestInFlightTimeoutFreesSlot(t *testing.T) {
	clk := &vclock{now: fixedNow()}
	gw, _ := newTestGW(t, clk)
	gw.SetFlow("c1")
	stuck, err := gw.Submit(t.Context(), Command{Kind: KindWorkMode, Params: map[Param]int{pMode: 1}, Origin: originAPI})
	if err != nil {
		t.Fatal(err)
	}
	clk.advance(inFlightTTL - time.Millisecond)
	if _, err := gw.Submit(t.Context(), pollRead()); !errors.Is(err, ErrInFlight) {
		t.Fatalf("before the TTL: got %v, want ErrInFlight", err)
	}
	clk.advance(time.Millisecond)
	if _, err := gw.Submit(t.Context(), pollRead()); err != nil {
		t.Fatalf("after the TTL: %v", err)
	}
	res, err := gw.Wait(t.Context(), stuck)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusFailed || !strings.Contains(res.Detail, "no response") {
		t.Fatalf("stuck command: %s %q, want failed / no response", res.Status, res.Detail)
	}
}
