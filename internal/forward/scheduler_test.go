package forward

import (
	"context"
	"encoding/binary"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nicois/prox/pkg/capture"

	"foxesslocal/internal/frame"
	"foxesslocal/internal/gateway"
	"foxesslocal/internal/store"
)

const (
	connTest    = "conn-1"
	invLeadByte = byte(0x91) // session lead nibble set: ack-able echo
)

// testInv is a scripted inverter behind a real gateway: it records every
// injected frame in wire order and, with auto-ack on, returns the
// matching inverter-originated echo (fn03 read response — zero
// registers, fn06/fn10 write ACK) through a FIFO worker that feeds the
// gateway in the same correlation order the inverter would.
type testInv struct {
	gw      *gateway.Gateway
	autoAck atomic.Bool

	mu        sync.Mutex
	writes    []frame.Frame // every injected frame, in wire order
	origins   []string      // the origin of each command, in ack order
	lastCmdID string        // the command whose origin was last recorded
	unacked   []frame.Frame
	ackErr    error

	ackCh chan []byte
	acked chan struct{} // one signal per command reaching its terminal state
}

// newTestInv must be called INSIDE the synctest bubble: the worker is a
// bubble goroutine and its channels are bubble-owned.
func newTestInv(t *testing.T) *testInv {
	t.Helper()
	f := &testInv{
		ackCh: make(chan []byte, 128),
		acked: make(chan struct{}, 16),
	}
	f.autoAck.Store(true)
	go f.worker()
	return f
}

func (f *testInv) setGw(gw *gateway.Gateway) { f.gw = gw }

// worker feeds the echoes to the gateway in FIFO order (the inverter
// responds sequentially); a signal goes out when the in-flight command
// reaches its terminal state. The in-flight command's origin is recorded
// here — never from InjectS2C: the gateway calls InjectS2C while holding
// its own lock, and Pending would deadlock on it.
func (f *testInv) worker() {
	for pdu := range f.ackCh {
		pk := f.inflight()
		if pk.id == "" {
			continue
		}
		if pk.id != f.lastCmdID {
			f.mu.Lock()
			if pk.origin != "" {
				f.origins = append(f.origins, pk.origin)
			}
			f.lastCmdID = pk.id
			f.mu.Unlock()
		}
		if err := f.gw.Ack(pk.id, frame.Frame{
			Device:  [4]byte{invLeadByte, 0, 0, 0},
			Fn:      0x10,
			Payload: pdu,
		}); err != nil {
			f.mu.Lock()
			f.ackErr = err
			f.mu.Unlock()
			continue
		}
		if f.inflight() == (pendingKey{}) {
			select {
			case f.acked <- struct{}{}:
			default:
			}
		}
	}
}

type pendingKey struct{ id, origin string }

func (f *testInv) inflight() pendingKey {
	for _, p := range f.gw.Pending() {
		if p.Status == gateway.StatusInFlight {
			return pendingKey{id: p.ID, origin: p.Origin}
		}
	}
	return pendingKey{}
}

// InjectS2C records the injected frames and, with auto-ack on, queues
// their echoes (the FIFO worker feeds them in order). It must not call
// back into the gateway (the lock is held by the caller).
func (f *testInv) InjectS2C(_ string, raw []byte) error {
	frs, _ := frame.Split(raw, 0)
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, fr := range frs {
		f.writes = append(f.writes, fr)
		if f.autoAck.Load() {
			if pdu := f.echo(fr); pdu != nil {
				f.ackCh <- pdu
			}
		} else {
			f.unacked = append(f.unacked, fr)
		}
	}
	return nil
}

// echo builds the inverter response PDU for one injected request (zero
// register values — the scheduler never reads state back from the
// device, and the gateway requires every read to return the block).
func (f *testInv) echo(fr frame.Frame) []byte {
	mb, err := frame.ParseModbus(fr.Payload)
	if err != nil {
		return nil
	}
	switch mb.Fn {
	case 0x03:

		p := make([]byte, 3+2*mb.Count)
		p[0], p[1] = 0x01, 0x03
		//nolint:gosec // G115: the byte count is bounded by the request.
		p[2] = byte(2 * mb.Count)
		return p
	case 0x06:
		var addr, val [2]byte
		//nolint:gosec // G115: bounded 16-bit protocol values.
		binary.BigEndian.PutUint16(addr[:], uint16(mb.Addr))
		//nolint:gosec // G115: bounded 16-bit protocol values.
		binary.BigEndian.PutUint16(val[:], uint16(mb.Value))
		return append([]byte{0x01, 0x06}, append(addr[:], val[:]...)...)
	case 0x10:
		var addr, cnt [2]byte
		//nolint:gosec // G115: bounded 16-bit protocol values.
		binary.BigEndian.PutUint16(addr[:], uint16(mb.Addr))
		//nolint:gosec // G115: bounded 16-bit protocol values.
		binary.BigEndian.PutUint16(cnt[:], uint16(mb.Count))
		return append([]byte{0x01, 0x10}, append(addr[:], cnt[:]...)...)
	}
	return nil
}

// hold disables auto-ack (further injections are stashed unacked).
func (f *testInv) hold() { f.autoAck.Store(false) }

// release re-enables auto-ack and re-queues the stashed echoes.
func (f *testInv) release() {
	f.mu.Lock()
	unacked := f.unacked
	f.unacked = nil
	f.mu.Unlock()
	f.autoAck.Store(true)
	for _, fr := range unacked {
		if pdu := f.echo(fr); pdu != nil {
			f.ackCh <- pdu
		}
	}
}

// seedSlot is one 48010 time-group as decoded from the store.
type seedSlot struct {
	en, start, end, wm, maxMin, fdSoc, fdPwr int
}

// seedSchedule records an fn10 48010 ×80 push into the store (the
// scheduler preserves the other slots from this decode) and flushes it.
func seedSchedule(t *testing.T, st *store.Store, ts time.Time, groups [8]seedSlot) {
	t.Helper()
	data := make([]byte, 160)
	put := func(i, v int) {
		//nolint:gosec // G115: bounded 16-bit protocol values.
		binary.BigEndian.PutUint16(data[2*i:], uint16(v))
	}
	for k, g := range groups {
		b := 10 * k
		put(b+0, g.en)
		put(b+1, g.start)
		put(b+2, g.end)
		put(b+3, g.wm)
		put(b+4, g.maxMin)
		put(b+5, g.fdSoc)
		put(b+6, g.fdPwr)
	}

	p := make([]byte, 7+160)
	p[0], p[1] = 0x01, 0x10

	binary.BigEndian.PutUint16(p[2:4], 48010)

	binary.BigEndian.PutUint16(p[4:6], 80)
	p[6] = 160
	copy(p[7:], data)
	st.RecordFrame(store.FrameRecord{
		ConnID:  connTest,
		Seq:     1,
		Dir:     capture.DirS2C,
		TS:      ts,
		Class:   "mbwrite",
		Fn:      0x10,
		Payload: p,
		// a write that reached the inverter (a dropped one never applies)
		Decision: "forwarded",
	})
	st.Flush()
}

// schedWrites returns the injected fn10 48010 ×80 writes (one per
// scheduler push) in wire order.
func schedWrites(t *testing.T, f *testInv) [][]uint16 {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]uint16
	for _, fr := range f.writes {
		mb, err := frame.ParseModbus(fr.Payload)
		if err != nil || mb.Fn != 0x10 || mb.Addr != 48010 || mb.Count != 80 {
			continue
		}
		vals := make([]uint16, 80)
		for i := range vals {
			vals[i] = binary.BigEndian.Uint16(mb.Data[2*i:])
		}
		out = append(out, vals)
	}
	return out
}

// masterWrites returns the r0 (enable) value of each injected fn10
// 48000 ×10 write, in wire order.
func masterWrites(t *testing.T, f *testInv) []int {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []int
	for _, fr := range f.writes {
		mb, err := frame.ParseModbus(fr.Payload)
		if err != nil || mb.Fn != 0x10 || mb.Addr != 48000 || mb.Count != 10 {
			continue
		}

		out = append(out, int(binary.BigEndian.Uint16(mb.Data[0:2])))
	}
	return out
}

// assertPushTable checks one pushed 48010 table: slot 1 at wantSlot1,
// slot 2 at wantSlot2 (the preserved row), slots 3–8 zeroed.
func assertPushTable(t *testing.T, vals []uint16, wantSlot1, wantSlot2 [7]int) {
	t.Helper()
	if got := slotAt(vals, 0); got != wantSlot1 {
		t.Fatalf("slot 1: got %v, want %v", got, wantSlot1)
	}
	if got := slotAt(vals, 1); got != wantSlot2 {
		t.Fatalf("slot 2: got %v, want %v", got, wantSlot2)
	}
	for k := 2; k < 8; k++ {
		if got := slotAt(vals, k); got != unusedGroup {
			t.Fatalf("slot %d: got %v, want the unused-group shape", k+1, got)
		}
	}
}

// unusedGroup is a cleared group as the gateway writes it — the cloud's
// unused-group shape (work mode 1, max:min SoC 100:10, fd SoC 10).
var unusedGroup = [7]int{0, 0, 0, 1, 100<<8 | 10, 10, 0}

// slotAt returns one pushed group's fields (en, start, end, wm, maxMin,
// fdsoc, fdpwr).
func slotAt(vals []uint16, k int) [7]int {
	b := 10 * k
	r := func(i int) int {
		return int(vals[b+i])
	}
	return [7]int{r(0), r(1), r(2), r(3), r(4), r(5), r(6)}
}

// packHHMM packs a time into the wire HHMM.
func packHHMM(tm time.Time) int {
	return tm.Hour()<<8 | tm.Minute()
}

// sleepTo advances the (virtual) clock to tm.
func sleepTo(t *testing.T, tm time.Time) {
	t.Helper()
	if d := time.Until(tm); d > 0 {
		time.Sleep(d)
	}
}

// TestNewSchedulerConstructionErrors: the constructor rejects windows
// the gateway could never accept (slot out of range, empty or inverted
// span, same-slot overlap, out-of-range HHMM); distinct slots may share
// spans.
func TestNewSchedulerConstructionErrors(t *testing.T) {
	clk := &testClock{now: testBase}
	cases := []struct {
		name string
		wins []Window
	}{
		{"slot zero", []Window{{Name: "a", Slot: 0, Start: 6 << 8, End: 7 << 8}}},
		{"slot nine", []Window{{Name: "a", Slot: 9, Start: 6 << 8, End: 7 << 8}}},
		{"start equals end", []Window{{Name: "a", Slot: 1, Start: 6 << 8, End: 6 << 8}}},
		{"end before start", []Window{{Name: "a", Slot: 1, Start: 7 << 8, End: 6 << 8}}},
		{"same-slot overlap", []Window{
			{Name: "a", Slot: 2, Start: 6 << 8, End: 8 << 8},
			{Name: "b", Slot: 2, Start: 7 << 8, End: 9 << 8},
		}},
		{"same-slot identical", []Window{
			{Name: "a", Slot: 3, Start: 6 << 8, End: 7 << 8},
			{Name: "b", Slot: 3, Start: 6 << 8, End: 7 << 8},
		}},
		{"bad hhmm", []Window{{Name: "a", Slot: 1, Start: 24 << 8, End: 25 << 8, FdSoc: 90}}},
		// the installer schema's segmented-time ranges (dump 2026-11-16)
		{"fd soc below 10", []Window{{Name: "a", Slot: 1, Start: 6 << 8, End: 7 << 8, FdSoc: 9}}},
		{"fd soc above 100", []Window{{Name: "a", Slot: 1, Start: 6 << 8, End: 7 << 8, FdSoc: 101}}},
		{"fd pwr above 10500", []Window{{Name: "a", Slot: 1, Start: 6 << 8, End: 7 << 8, FdSoc: 90, FdPwr: 10501}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := NewScheduler(nil, nil, clk.get, c.wins); err == nil {
				t.Fatal("NewScheduler: want error, got nil")
			}
		})
	}
	if _, err := NewScheduler(nil, nil, clk.get, []Window{
		{Name: "a", Slot: 1, Start: 6 << 8, End: 7 << 8, FdSoc: 10},
		{Name: "b", Slot: 2, Start: 6 << 8, End: 7 << 8, FdSoc: 100, FdPwr: 10500},
		{Name: "c", Slot: 1, Start: 8 << 8, End: 9 << 8, FdSoc: 90},
	}); err != nil {
		t.Fatalf("valid windows: %v", err)
	}
}

// TestSchedulerOpenClose: the window opens at its start (one
// all-8-group push: target slot armed in force-charge, the other slots
// preserved from the store decode including a stored non-default
// maxSoC), closes at its end (target slot zeroed), and is idempotent
// across later ticks.
func TestSchedulerOpenClose(t *testing.T) {
	st := newTestStore(t)

	var (
		f     *testInv
		slot2 [7]int // expected preserved slot 2 (relative to the virtual start)
	)
	synctest.Test(t, func(t *testing.T) {
		now := time.Now()
		start := now.Add(20 * time.Minute).Truncate(time.Minute)
		end := now.Add(50 * time.Minute).Truncate(time.Minute)
		// The device's current table: slot 2 enabled (work mode 7, one
		// hour, well clear of the test window — the gateway rejects
		// partial cross-slot overlaps) with stored maxSoC 55
		// (non-default — the push must preserve 55, not the 100
		// default).
		s2Start := now.Add(-3 * time.Hour).Truncate(time.Minute)
		s2End := s2Start.Add(time.Hour)
		seedSchedule(t, st, testBase, [8]seedSlot{
			{},
			{en: 1, start: packHHMM(s2Start), end: packHHMM(s2End), wm: 7, maxMin: 55<<8 | 10, fdSoc: 40, fdPwr: 2500},
			{},
			{},
			{},
			{},
			{},
			{},
		})
		slot2 = [7]int{1, packHHMM(s2Start), packHHMM(s2End), 7, 55<<8 | 10, 40, 2500}
		wins := []Window{{
			Name: "night", Slot: 1, Start: packHHMM(start), End: packHHMM(end),
			FdSoc: 90, FdPwr: 9500,
		}}

		f = newTestInv(t)
		gw := gateway.New(f, time.Now)
		f.setGw(gw)
		gw.SetFlow(connTest)

		sched, err := NewScheduler(st, gw, time.Now, wins)
		if err != nil {
			t.Fatalf("NewScheduler: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		runDone := make(chan struct{})
		go func() { sched.Run(ctx); close(runDone) }()

		// Open: advance to the window start, then block until the open
		// push reaches its terminal state at the gateway.
		sleepTo(t, start)
		<-f.acked

		writes := schedWrites(t, f)
		if len(writes) != 1 {
			t.Fatalf("pushes after open: got %d, want 1", len(writes))
		}
		// min SoC on grid 10: the installer's floor (the cloud's value)
		wantSlot1 := [7]int{1, packHHMM(start), packHHMM(end), 6, 100<<8 | 10, 90, 9500}
		assertPushTable(t, writes[0], wantSlot1, slot2)
		if got := masterWrites(t, f); len(got) != 1 || got[0] != 1 {
			t.Fatalf("open master write: got %v, want [1]", got)
		}

		// Close: advance to the window end; the close push must zero the
		// target slot and keep the preserved one.
		sleepTo(t, end)
		<-f.acked
		writes = schedWrites(t, f)
		if len(writes) != 2 {
			t.Fatalf("pushes after close: got %d, want 2", len(writes))
		}
		assertPushTable(t, writes[1], unusedGroup, slot2)
		if got := masterWrites(t, f); len(got) != 2 || got[1] != 1 {
			t.Fatalf("close master write: got %v, want slot 2 still enabled", got)
		}

		// Idempotence: later ticks must not re-push.
		sleepTo(t, end.Add(90*time.Second))
		if got := schedWrites(t, f); len(got) != 2 {
			t.Fatalf("idempotence: got %d pushes, want 2", len(got))
		}

		// Drain the bubble: stop the scheduler (its Run goroutine),
		// wait for it to have fully exited, then release the fake's
		// worker (synctest requires every bubble goroutine to have
		// exited before the body returns).
		cancel()
		<-runDone
		close(f.ackCh)
	})

	// Origin: both pushes went through the gateway tagged with the
	// window name (the audit trail carries no origin of its own).
	wantOrigins := []string{"scheduler:night", "scheduler:night"}
	if !slices.Equal(f.origins, wantOrigins) {
		t.Fatalf("push origins: got %v, want %v", f.origins, wantOrigins)
	}
	assertScheduleAuditRows(t, st, []string{"opened night: acked", "closed night: acked"})
}

// assertScheduleAuditRows checks the schedule audit rows (kinds and
// details, in order) against wantDetails.
func assertScheduleAuditRows(t *testing.T, st *store.Store, wantDetails []string) {
	t.Helper()
	rows := auditRows(t, st, audKindSchedule)
	if len(rows) != len(wantDetails) {
		t.Fatalf("schedule audit rows: got %d, want %d: %v", len(rows), len(wantDetails), rows)
	}
	for i, row := range rows {
		if row.Kind != audKindSchedule || row.Detail != wantDetails[i] {
			t.Fatalf("audit row %d: got kind %q detail %q, want kind %q detail %q",
				i, row.Kind, row.Detail, audKindSchedule, wantDetails[i])
		}
	}
}

// TestSchedulerErrInFlightRetry: with another command in flight, the
// open attempt fails at Submit (ErrInFlight) — no push, no state
// change, an audit row per skipped tick — and the next tick after the
// held command clears retries and lands exactly once.
func TestSchedulerErrInFlightRetry(t *testing.T) {
	st := newTestStore(t)

	synctest.Test(t, func(t *testing.T) {
		now := time.Now()
		start := now.Add(20 * time.Minute).Truncate(time.Minute)
		end := now.Add(50 * time.Minute).Truncate(time.Minute)
		wins := []Window{{Name: "retry", Slot: 1, Start: packHHMM(start), End: packHHMM(end), FdSoc: 90}}

		f := newTestInv(t)
		// The held command spans ~20 min of virtual time — far past the
		// default in-flight bound — so this test opts into a long one.
		gw := gateway.New(f, time.Now, gateway.WithInFlightTTL(time.Hour))
		f.setGw(gw)
		gw.SetFlow(connTest)

		sched, err := NewScheduler(st, gw, time.Now, wins)
		if err != nil {
			t.Fatalf("NewScheduler: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		runDone := make(chan struct{})
		go func() { sched.Run(ctx); close(runDone) }()

		// Hold a concurrent operator command in flight (auto-ack off).
		f.hold()
		if _, err := gw.Submit(ctx, gateway.Command{
			Kind:   gateway.KindWorkMode,
			Params: map[gateway.Param]int{gateway.ParamMode: 1},
			Origin: "api",
		}); err != nil {
			t.Fatalf("submit held command: %v", err)
		}

		// Ticks across the window start fail with ErrInFlight; release
		// after two tick periods so at least one failing tick is
		// guaranteed, then block until the held command AND the retried
		// open push both reach their terminal state.
		sleepTo(t, start.Add(35*time.Second))
		f.release()
		<-f.acked
		<-f.acked

		if got := schedWrites(t, f); len(got) != 1 {
			t.Fatalf("pushes: got %d, want 1 (the retried open)", len(got))
		}
		if err := f.ackErr; err != nil {
			t.Fatalf("gateway ack: %v", err)
		}

		cancel()
		<-runDone
		close(f.ackCh)
	})

	rows := auditRows(t, st, audKindSchedule)
	fails, ok := 0, false
	for _, r := range rows {
		switch {
		case strings.Contains(r.Detail, "in flight"):
			fails++
		case r.Detail == "opened retry: acked":
			ok = true
		}
	}
	if fails < 1 {
		t.Fatalf("want at least one ErrInFlight audit row, got %v", rows)
	}
	if !ok {
		t.Fatalf("want the retried open acked row, got %v", rows)
	}
}
