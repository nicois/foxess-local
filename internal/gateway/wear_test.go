package gateway

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Inverter-wear protections (2026-11-17): every write is read first and
// skipped when the inverter already holds the value (WithSkipUnchanged),
// and real writes are budgeted per setting (WithWriteBudget).

func newWearGW(t *testing.T, clk *vclock, opts ...Option) (*Gateway, *spyInjector) {
	t.Helper()
	inj := &spyInjector{}
	gw := New(inj, clk.get, opts...)
	gw.SetFlow("c1")
	return gw, inj
}

// lastPDU is the newest injected frame's Modbus PDU.
func lastPDU(t *testing.T, inj *spyInjector) []byte {
	t.Helper()
	calls := inj.injected()
	return pdu(t, calls[len(calls)-1].raw)
}

func waitRes(t *testing.T, gw *Gateway, id string) Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	res, err := gw.Wait(ctx, id)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	return res
}

// TestSkipUnchangedWorkMode: the work-mode write reads 41000 first; the
// same value is acknowledged without a write, a different one is written.
func TestSkipUnchangedWorkMode(t *testing.T) {
	clk := &vclock{now: fixedNow()}
	gw, inj := newWearGW(t, clk, WithSkipUnchanged())

	id, err := gw.Submit(context.Background(), Command{Kind: KindWorkMode, Params: map[Param]int{pMode: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if p := lastPDU(t, inj); p[pduFnOff] != fnRead || int(p[2])<<8|int(p[3]) != regWorkMode {
		t.Fatalf("first frame %x, want the 41000 read", p)
	}
	if err := ack(gw, id, readRespPDU(1, map[int]uint16{0: 1})); err != nil {
		t.Fatal(err)
	}
	if res := waitRes(t, gw, id); res.Status != StatusAcked || !res.Unchanged {
		t.Fatalf("same value: %+v, want acked + unchanged", res)
	}
	if n := len(inj.injected()); n != 1 {
		t.Fatalf("injected %d frames, want only the read", n)
	}

	id, err = gw.Submit(context.Background(), Command{Kind: KindWorkMode, Params: map[Param]int{pMode: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if err := ack(gw, id, readRespPDU(1, map[int]uint16{0: 0})); err != nil {
		t.Fatal(err)
	}
	if p := lastPDU(t, inj); p[pduFnOff] != fnWriteOne {
		t.Fatalf("after a different read: %x, want the fn06 write", p)
	}
	if err := ack(gw, id, fn06AckPDU(regWorkMode)); err != nil {
		t.Fatal(err)
	}
	if res := waitRes(t, gw, id); res.Status != StatusAcked || res.Unchanged {
		t.Fatalf("written: %+v", res)
	}
}

// TestSkipUnchangedLimitsPerField: each limit is its own register pair;
// only the one that differs is written.
func TestSkipUnchangedLimitsPerField(t *testing.T) {
	clk := &vclock{now: fixedNow()}
	gw, inj := newWearGW(t, clk, WithSkipUnchanged())
	id, err := gw.Submit(context.Background(), Command{Kind: KindLimits, Params: map[Param]int{pImport: 12500, pExport: 5000}})
	if err != nil {
		t.Fatal(err)
	}
	// 41021..41024: import 12500 (unchanged), export 4000 (differs)
	if err := ack(gw, id, readRespPDU(4, map[int]uint16{1: 12500, 3: 4000})); err != nil {
		t.Fatal(err)
	}
	calls := inj.injected()
	if len(calls) != 2 {
		t.Fatalf("injected %d frames, want the read + one write", len(calls))
	}
	if p := lastPDU(t, inj); int(p[2])<<8|int(p[3]) != regExportLim {
		t.Fatalf("wrote %x, want the export limit only", p)
	}
	if err := ack(gw, id, writeAckPDU(regExportLim, 2)); err != nil {
		t.Fatal(err)
	}
	if res := waitRes(t, gw, id); res.Status != StatusAcked || res.Unchanged {
		t.Fatalf("result %+v", res)
	}
}

// TestSkipUnchangedSchedule: a push identical to the inverter's table
// writes nothing; any difference writes the whole read-modify-write
// sequence as before (never a partial push).
func TestSkipUnchangedSchedule(t *testing.T) {
	clk := &vclock{now: fixedNow()}
	gw, inj := newWearGW(t, clk, WithSkipUnchanged())
	// the current table = exactly what this command would write
	frs, err := gw.BuildFrames(baseSchedule(), currentSet(), defaultEnvFn)
	if err != nil {
		t.Fatal(err)
	}
	var same []RegisterValue
	for base, vals := range writeBlocks(t, rawsOf(t, frs[13:])) {
		for off, v := range vals {
			same = append(same, RegisterValue{Addr: regAddr(base + off), Value: v})
		}
	}
	id, err := gw.Submit(context.Background(), baseSchedule())
	if err != nil {
		t.Fatal(err)
	}
	feedReadResponses(t, gw, id, same)
	if res := waitRes(t, gw, id); res.Status != StatusAcked || !res.Unchanged {
		t.Fatalf("identical push: %+v, want acked + unchanged", res)
	}
	if n := len(inj.injected()); n != 13 {
		t.Fatalf("injected %d frames, want the 13 reads only", n)
	}

	id, err = gw.Submit(context.Background(), baseSchedule())
	if err != nil {
		t.Fatal(err)
	}
	changed := append([]RegisterValue(nil), same...)
	for i := range changed {
		if changed[i].Addr == regAddr(regSlotTable+slotFdPwr) {
			changed[i].Value++ // one register differs
		}
	}
	feedReadResponses(t, gw, id, changed)
	// the first write goes out on the read-phase completion; the rest
	// pace on the device's echoes (one write in flight at a time):
	// 13 reads (push 1) + 13 reads + 1 write
	if n := len(inj.injected()); n != 27 {
		t.Fatalf("after a difference: %d frames, want 27", n)
	}
}

// TestWriteBudget: real writes per setting are capped per window; an
// unchanged no-op does not count; the window rolls.
func TestWriteBudget(t *testing.T) {
	clk := &vclock{now: fixedNow()}
	gw, _ := newWearGW(t, clk, WithSkipUnchanged(), WithWriteBudget(2, time.Hour))
	write := func(cur uint16) error {
		id, err := gw.Submit(context.Background(), Command{Kind: KindWorkMode, Params: map[Param]int{pMode: 1}})
		if err != nil {
			return err
		}
		if err := ack(gw, id, readRespPDU(1, map[int]uint16{0: cur})); err != nil {
			t.Fatal(err)
		}
		if cur != 1 {
			if err := ack(gw, id, fn06AckPDU(regWorkMode)); err != nil {
				t.Fatal(err)
			}
		}
		waitRes(t, gw, id)
		return nil
	}
	for i := range 2 {
		if err := write(0); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	// no-ops never count — but over budget, every write is refused up front
	if err := write(0); !errors.Is(err, ErrWriteBudget) {
		t.Fatalf("third write in the hour: %v, want ErrWriteBudget", err)
	}
	// other settings have their own budget
	if _, err := gw.Submit(context.Background(), Command{Kind: KindMeterCompensation, Params: map[Param]int{pValue: 10}}); err != nil {
		t.Fatalf("another setting: %v", err)
	}
	clk.advance(time.Hour + time.Second)
	gw.SetFlow("")
	gw.SetFlow("c2") // drop the in-flight meter write
	if err := write(0); err != nil {
		t.Fatalf("after the window: %v", err)
	}
}

// TestUnchangedDoesNotCount: no-ops never use the budget.
func TestUnchangedDoesNotCount(t *testing.T) {
	clk := &vclock{now: fixedNow()}
	gw, _ := newWearGW(t, clk, WithSkipUnchanged(), WithWriteBudget(1, time.Hour))
	for i := range 5 {
		id, err := gw.Submit(context.Background(), Command{Kind: KindWorkMode, Params: map[Param]int{pMode: 1}})
		if err != nil {
			t.Fatalf("no-op %d: %v", i, err)
		}
		if err := ack(gw, id, readRespPDU(1, map[int]uint16{0: 1})); err != nil {
			t.Fatal(err)
		}
		if res := waitRes(t, gw, id); !res.Unchanged {
			t.Fatalf("no-op %d: %+v", i, res)
		}
	}
}

// TestWriteBudgetView (foxess-control local backend, 2026-11-19): the
// budget is visible per setting — limit, remaining, the window and when
// the oldest counted write leaves it (null while the budget is full) —
// limits can differ per setting, and a refusal carries the setting and
// the next free time.
func TestWriteBudgetView(t *testing.T) {
	clk := &vclock{now: fixedNow()}
	gw, _ := newWearGW(t, clk, WithSkipUnchanged(), WithWriteBudget(2, time.Hour),
		WithWriteBudgetFor(map[CommandKind]int{KindSchedule: 5}))
	checkFreshBudget(t, gw.Budget())
	first := clk.now
	for range 2 {
		writeWorkMode(t, gw)
		clk.advance(time.Minute)
	}
	b := gw.Budget()[KindWorkMode]
	if b.Remaining != 0 || b.NextFree == nil || !b.NextFree.Equal(first.Add(time.Hour)) {
		t.Fatalf("spent: %+v", b)
	}
	_, err := gw.Submit(context.Background(), Command{Kind: KindWorkMode, Params: map[Param]int{pMode: 0}})
	var be *BudgetError
	if !errors.As(err, &be) || !errors.Is(err, ErrWriteBudget) || be.Kind != KindWorkMode || be.Limit != 2 || !be.NextFree.Equal(first.Add(time.Hour)) {
		t.Fatalf("refusal: %v (%+v)", err, be)
	}
}

// TestBudgetCountsSentWrites (foxess-control local backend, 2026-11-19):
// a write that never reached the inverter (the injection failed) costs
// nothing; the budget counts what was sent.
func TestBudgetCountsSentWrites(t *testing.T) {
	clk := &vclock{now: fixedNow()}
	gw, inj := newWearGW(t, clk, WithWriteBudget(2, time.Hour)) // no read first: the write is the first frame
	inj.mu.Lock()
	inj.err = errors.New("session gone")
	inj.mu.Unlock()
	if _, err := gw.Submit(context.Background(), Command{Kind: KindMeterCompensation, Params: map[Param]int{pValue: 10}}); err == nil {
		t.Fatal("a failed injection was accepted")
	}
	if b := gw.Budget()[KindMeterCompensation]; b.Remaining != 2 {
		t.Fatalf("an unsent write was counted: %+v", b)
	}
}

// TestBudgetFromObserver: the observer may read the budget (the host's
// config event carries it) — the gateway calls the observer under its
// lock, so Budget must not need that lock.
func TestBudgetFromObserver(t *testing.T) {
	clk := &vclock{now: fixedNow()}
	seen := make(chan int, 1)
	var gw *Gateway
	gw, _ = newWearGW(t, clk, WithWriteBudget(3, time.Hour), WithObserver(func(Result) {
		seen <- gw.Budget()[KindWorkMode].Remaining
	}))
	id, err := gw.Submit(context.Background(), Command{Kind: KindWorkMode, Params: map[Param]int{pMode: 1}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- ack(gw, id, fn06AckPDU(regWorkMode)) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second): // a deadlock guard, not timing logic
		t.Fatal("the observer's Budget call deadlocked")
	}
	if got := <-seen; got != 2 {
		t.Fatalf("remaining seen by the observer: %d, want 2", got)
	}
}

// writeWorkMode is one real work-mode write (read first: the inverter
// holds 0, so 1 is written).
func writeWorkMode(t *testing.T, gw *Gateway) {
	t.Helper()
	id, err := gw.Submit(context.Background(), Command{Kind: KindWorkMode, Params: map[Param]int{pMode: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if err := ack(gw, id, readRespPDU(1, map[int]uint16{0: 0})); err != nil {
		t.Fatal(err)
	}
	if err := ack(gw, id, fn06AckPDU(regWorkMode)); err != nil {
		t.Fatal(err)
	}
	waitRes(t, gw, id)
}

// checkFreshBudget: an unused budget, with the schedule override, and no
// entry for reads.
func checkFreshBudget(t *testing.T, v map[CommandKind]BudgetState) {
	t.Helper()
	if b := v[KindWorkMode]; b.Limit != 2 || b.Remaining != 2 || b.Window != time.Hour || b.NextFree != nil {
		t.Fatalf("fresh work mode: %+v", b)
	}
	if b := v[KindSchedule]; b.Limit != 5 || b.Remaining != 5 {
		t.Fatalf("schedule override: %+v", b)
	}
	if _, ok := v[KindRead]; ok {
		t.Fatal("reads are not budgeted")
	}
}

// TestScheduleBudgetCountsOnePush (2026-11-24): a schedule push is 13
// paced write frames but one schedule write — one installer edit spent
// the whole budget of 10 and locked the override out for an hour.
func TestScheduleBudgetCountsOnePush(t *testing.T) {
	clk := &vclock{now: fixedNow()}
	gw, _ := newWearGW(t, clk, WithWriteBudget(10, time.Hour))
	id, err := gw.Submit(context.Background(), baseSchedule())
	if err != nil {
		t.Fatal(err)
	}
	feedReadResponses(t, gw, id, currentSet())
	feedWriteAcks(t, gw, id)
	if res := waitRes(t, gw, id); res.Status != StatusAcked {
		t.Fatalf("push: %+v", res)
	}
	if b := gw.Budget()[KindSchedule]; b.Remaining != 9 {
		t.Fatalf("one push left %d of 10, want 9", b.Remaining)
	}
}
