package gateway

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// spyInjector records every injected frame.
type spyInjector struct {
	mu    sync.Mutex
	calls []injectedCall
	err   error
}

type injectedCall struct {
	connID string
	raw    []byte
}

func (s *spyInjector) InjectS2C(connID string, raw []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.calls = append(s.calls, injectedCall{connID, append([]byte(nil), raw...)})
	return nil
}

func (s *spyInjector) injected() []injectedCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]injectedCall, len(s.calls))
	copy(out, s.calls)
	return out
}

// vclock is the injected virtual clock (C-004: no wall-clock in tests).
type vclock struct{ now time.Time }

func (c *vclock) get() time.Time          { return c.now }
func (c *vclock) advance(d time.Duration) { c.now = c.now.Add(d) }

func newTestGW(t *testing.T, clk *vclock) (*Gateway, *spyInjector) {
	t.Helper()
	inj := &spyInjector{}
	return New(inj, clk.get), inj
}

func pendingStatus(gw *Gateway, id string) (Status, bool) {
	for _, p := range gw.Pending() {
		if p.ID == id {
			return p.Status, true
		}
	}
	return "", false
}

// ---------- validation: unknown kind / unknown param ----------

func TestValidateUnknownKind(t *testing.T) {
	clk := &vclock{now: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	gw, _ := newTestGW(t, clk)
	if _, err := gw.Submit(context.Background(), Command{Kind: "metersoc", Params: map[Param]int{}}); !errors.Is(err, ErrUnknownKind) {
		t.Errorf("metersoc kind: want ErrUnknownKind, got %v", err)
	}
	if _, err := gw.Submit(context.Background(), Command{Kind: "minsoc"}); !errors.Is(err, ErrUnknownKind) {
		t.Errorf("minsoc kind: want ErrUnknownKind, got %v", err)
	}
}

func TestValidateUnknownParam(t *testing.T) {
	clk := &vclock{now: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	gw, _ := newTestGW(t, clk)
	if _, err := gw.Submit(context.Background(), Command{Kind: KindWorkMode, Params: map[Param]int{pMode: 0, {"extra"}: 1}}); !errors.Is(err, ErrUnknownParam) {
		t.Errorf("unknown param: want ErrUnknownParam, got %v", err)
	}
}

// ---------- clamp boundaries per kind ----------

func TestValidateLimitsBounds(t *testing.T) {
	clk := &vclock{now: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	gw, inj := newTestGW(t, clk)
	gw.SetFlow("c1")

	cases := []struct {
		name  string
		param Param
		val   int
		err   error
	}{
		{"import below", pImport, -1, ErrOutOfRange},
		{"import at 0", pImport, 0, nil},
		{"import mid", pImport, 15000, nil},
		{"import at 30000", pImport, 30000, nil},
		{"import above", pImport, 30001, ErrOutOfRange},
		{"export below", pExport, -1, ErrOutOfRange},
		{"export at 0", pExport, 0, nil},
		{"export at 30000", pExport, 30000, nil},
		{"export above", pExport, 30001, ErrOutOfRange},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := len(inj.injected())
			id, err := gw.Submit(context.Background(), Command{Kind: KindLimits, Params: map[Param]int{tc.param: tc.val}})
			if tc.err == nil {
				if err != nil {
					t.Fatalf("want ok, got %v", err)
				}
				if got := len(inj.injected()); got != n+1 {
					t.Errorf("injected %d frames, want 1 (n=%d)", got-n, n)
				}
				// complete it so the next submit is not rejected as in-flight
				completeLimits(t, gw, inj, id, n)
			} else if !errors.Is(err, tc.err) {
				t.Fatalf("want %v, got %v", tc.err, err)
			}
		})
	}
	if got := len(inj.injected()); got != 5 {
		t.Errorf("total injected frames: got %d, want 5 (one per valid submit)", got)
	}
}

// completeLimits acks the command of the nth injected frame (fn10 ×2 shape).
func completeLimits(t *testing.T, gw *Gateway, inj *spyInjector, id string, n int) {
	t.Helper()
	p := pdu(t, inj.injected()[n].raw)
	aack := writeAckPDU(int(p[2])<<8|int(p[3]), int(p[4])<<8|int(p[5]))
	if aerr := ack(gw, id, aack); aerr != nil {
		t.Fatalf("ack: %v", aerr)
	}
}

func TestValidateLimitsNoField(t *testing.T) {
	clk := &vclock{now: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	gw, _ := newTestGW(t, clk)
	if _, err := gw.Submit(context.Background(), Command{Kind: KindLimits, Params: map[Param]int{}}); !errors.Is(err, ErrNoFields) {
		t.Errorf("want ErrNoFields, got %v", err)
	}
}

func TestValidateMeterCompensationBounds(t *testing.T) {
	clk := &vclock{now: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	gw, _ := newTestGW(t, clk)
	cases := []struct {
		val int
		err error
	}{
		{-501, ErrOutOfRange},
		{-500, nil},
		{-1, nil},
		{0, nil},
		{1, nil},
		{500, nil},
		{501, ErrOutOfRange},
	}
	for _, tc := range cases {
		_, err := gw.Submit(context.Background(), Command{Kind: KindMeterCompensation, Params: map[Param]int{pValue: tc.val}})
		if tc.err == nil && err != nil {
			t.Errorf("value %d: want ok, got %v", tc.val, err)
		}
		if tc.err != nil && !errors.Is(err, tc.err) {
			t.Errorf("value %d: want %v, got %v", tc.val, tc.err, err)
		}
	}
	if _, err := gw.Submit(context.Background(), Command{Kind: KindMeterCompensation, Params: map[Param]int{}}); !errors.Is(err, ErrMissingParam) {
		t.Errorf("missing value: want ErrMissingParam, got %v", err)
	}
}

func TestValidateWorkMode(t *testing.T) {
	clk := &vclock{now: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	gw, _ := newTestGW(t, clk)
	for _, val := range []int{0, 1} {
		if _, err := gw.Submit(context.Background(), Command{Kind: KindWorkMode, Params: map[Param]int{pMode: val}}); err != nil {
			t.Errorf("mode %d: want ok, got %v", val, err)
		}
	}
	for _, val := range []int{-1, 2, 3} {
		if _, err := gw.Submit(context.Background(), Command{Kind: KindWorkMode, Params: map[Param]int{pMode: val}}); !errors.Is(err, ErrOutOfRange) {
			t.Errorf("mode %d: want ErrOutOfRange, got %v", val, err)
		}
	}
	if _, err := gw.Submit(context.Background(), Command{Kind: KindWorkMode, Params: map[Param]int{}}); !errors.Is(err, ErrMissingParam) {
		t.Errorf("missing mode: want ErrMissingParam, got %v", err)
	}
}

// ---------- schedule validation ----------

func TestValidateScheduleSoCBounds(t *testing.T) {
	clk := &vclock{now: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	gw, _ := newTestGW(t, clk)
	for _, field := range []Param{SlotParam(1, SlotMaxSoc), SlotParam(1, SlotMinSoc), SlotParam(1, SlotFdSoc)} {
		for _, tc := range []struct {
			val int
			err error
		}{
			// slot 1 is enabled: the installer's 10..100 (schema dump 2026-11-16)
			{-1, ErrOutOfRange}, {9, ErrOutOfRange}, {10, nil}, {50, nil}, {100, nil}, {101, ErrOutOfRange},
		} {
			c := baseSchedule()
			c.Params[field] = tc.val
			_, err := gw.Submit(context.Background(), c)
			if tc.err == nil && err != nil {
				t.Errorf("%s=%d: want ok, got %v", field, tc.val, err)
			}
			if tc.err != nil && !errors.Is(err, tc.err) {
				t.Errorf("%s=%d: want %v, got %v", field, tc.val, tc.err, err)
			}
		}
	}
}

func TestValidateScheduleSlotWorkMode(t *testing.T) {
	clk := &vclock{now: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	gw, _ := newTestGW(t, clk)
	for _, val := range []int{1, 2, 6, 7} {
		c := baseSchedule()
		setSlot(&c, 4, sEn, 1)
		setSlot(&c, 4, sStart, 0x0000)
		setSlot(&c, 4, sEnd, 0x0100)
		setSlot(&c, 4, sWorkMode, val)
		_, err := gw.Submit(context.Background(), c)
		if err != nil {
			t.Errorf("slot workmode %d (enabled): want ok, got %v", val, err)
		}
	}
	for _, val := range []int{0, 8} {
		c := baseSchedule()
		setSlot(&c, 4, sEn, 1)
		setSlot(&c, 4, sStart, 0x0000)
		setSlot(&c, 4, sEnd, 0x0100)
		setSlot(&c, 4, sWorkMode, val)
		_, err := gw.Submit(context.Background(), c)
		if !errors.Is(err, ErrOutOfRange) {
			t.Errorf("slot workmode %d (enabled): want ErrOutOfRange, got %v", val, err)
		}
	}
	// disabled slots skip the window/workmode checks (the inverter holds
	// non-zero fields on disabled groups — capture g4–g8); zeroed end==start
	// is fine while disabled
	c := baseSchedule()
	setSlot(&c, 5, sEn, 0)
	setSlot(&c, 5, sWorkMode, 1)
	setSlot(&c, 5, sStart, 0x0B00)
	setSlot(&c, 5, sEnd, 0x0B00)
	if _, err := gw.Submit(context.Background(), c); err != nil {
		t.Errorf("disabled slot with non-zero fields: want ok, got %v", err)
	}
}

func TestValidateScheduleHHMM(t *testing.T) {
	clk := &vclock{now: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	gw, _ := newTestGW(t, clk)
	// slot-4-only table: 23:59 is the valid HHMM bound; hour 24 / minute 117 are not
	ok := Command{Kind: KindSchedule, Params: map[Param]int{pEnabled: 1}}
	setSlot(&ok, 4, sEn, 1)
	setSlot(&ok, 4, sStart, 0x0000)
	setSlot(&ok, 4, sEnd, 0x173B)
	setSlot(&ok, 4, sWorkMode, 1)
	setSlot(&ok, 4, sMaxSoc, 100)
	setSlot(&ok, 4, sMinSoc, 10)
	setSlot(&ok, 4, sFdSoc, 10)
	if _, err := gw.Submit(context.Background(), ok); err != nil {
		t.Errorf("23:59: want ok, got %v", err)
	}
	for _, bad := range []int{0x1800, 0x0B75} { // 24:00, 11:117
		c := ok
		c.Params = map[Param]int{pEnabled: 1}
		setSlot(&c, 4, sEn, 1)
		setSlot(&c, 4, sStart, 0x0000)
		setSlot(&c, 4, sEnd, bad)
		setSlot(&c, 4, sWorkMode, 1)
		if _, err := gw.Submit(context.Background(), c); !errors.Is(err, ErrOutOfRange) {
			t.Errorf("hhmm %#04x: want ErrOutOfRange, got %v", bad, err)
		}
	}
}

func TestValidateScheduleEnabled(t *testing.T) {
	clk := &vclock{now: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	gw, _ := newTestGW(t, clk)
	c := baseSchedule()
	delete(c.Params, pEnabled)
	if _, err := gw.Submit(context.Background(), c); !errors.Is(err, ErrMissingParam) {
		t.Errorf("missing enabled: want ErrMissingParam, got %v", err)
	}
	c = baseSchedule()
	c.Params[pEnabled] = 2
	if _, err := gw.Submit(context.Background(), c); !errors.Is(err, ErrOutOfRange) {
		t.Errorf("enabled=2: want ErrOutOfRange, got %v", err)
	}
}

func TestValidateScheduleOverlap(t *testing.T) {
	clk := &vclock{now: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	gw, _ := newTestGW(t, clk)
	// two enabled overlapping windows (slot 1 11:00–13:00, slot 4 12:00–14:00)
	c := baseSchedule()
	setSlot(&c, 1, sStart, 0x0B00)
	setSlot(&c, 1, sEnd, 0x0D00)
	setSlot(&c, 4, sEn, 1)
	setSlot(&c, 4, sStart, 0x0C00)
	setSlot(&c, 4, sEnd, 0x0E00)
	setSlot(&c, 4, sWorkMode, 1)
	if _, err := gw.Submit(context.Background(), c); !errors.Is(err, ErrScheduleOverlap) {
		t.Errorf("overlap: want ErrScheduleOverlap, got %v", err)
	}
	// adjacent (half-open: end == next start) is fine
	c = baseSchedule()
	setSlot(&c, 1, sStart, 0x0B00)
	setSlot(&c, 1, sEnd, 0x0C00)
	setSlot(&c, 4, sEn, 1)
	setSlot(&c, 4, sStart, 0x0C00)
	setSlot(&c, 4, sEnd, 0x0E00)
	setSlot(&c, 4, sWorkMode, 1)
	if _, err := gw.Submit(context.Background(), c); err != nil {
		t.Errorf("adjacent: want ok, got %v", err)
	}
	// a disabled slot with an overlapping window is ignored
	c = baseSchedule()
	setSlot(&c, 4, sEn, 0)
	setSlot(&c, 4, sStart, 0x0B00)
	setSlot(&c, 4, sEnd, 0x0D00)
	setSlot(&c, 1, sStart, 0x0B00)
	setSlot(&c, 1, sEnd, 0x0D00)
	if _, err := gw.Submit(context.Background(), c); err != nil {
		t.Errorf("disabled overlap: want ok, got %v", err)
	}
}

func TestValidateScheduleWindow(t *testing.T) {
	clk := &vclock{now: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	gw, _ := newTestGW(t, clk)
	for _, tc := range []struct{ start, end int }{
		{0x0B00, 0x0B00}, // end == start
		{0x0D00, 0x0B00}, // end < start
	} {
		c := baseSchedule()
		setSlot(&c, 1, sStart, tc.start)
		setSlot(&c, 1, sEnd, tc.end)
		if _, err := gw.Submit(context.Background(), c); !errors.Is(err, ErrScheduleWindow) {
			t.Errorf("end<=start (%#04x->%#04x): want ErrScheduleWindow, got %v", tc.start, tc.end, err)
		}
	}
	// disabled slots are ignored by the window check (zeroed window is fine)
	if _, err := gw.Submit(context.Background(), baseSchedule()); err != nil {
		t.Errorf("base schedule (disabled slots zeroed): want ok, got %v", err)
	}
}

// ---------- C-003: invalid commands never reach the wire ----------

func TestSubmitInvalidInjectsNothing(t *testing.T) {
	clk := &vclock{now: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	gw, inj := newTestGW(t, clk)
	gw.SetFlow("c1")
	cases := []Command{
		{Kind: KindLimits, Params: map[Param]int{pImport: 30001}},
		{Kind: KindMeterCompensation, Params: map[Param]int{pValue: -501}},
		{Kind: KindWorkMode, Params: map[Param]int{pMode: 2}},
		{Kind: "bogus"},
	}
	for _, c := range cases {
		if _, err := gw.Submit(context.Background(), c); err == nil {
			t.Errorf("%v: want error, got nil", c.Kind)
		}
	}
	if got := len(inj.injected()); got != 0 {
		t.Errorf("injected %d frames for invalid commands, want 0", got)
	}
	if got := len(gw.Pending()); got != 0 {
		t.Errorf("Pending() has %d entries for invalid submits, want 0", got)
	}
}

// ---------- no-flow queue + TTL (injected clock, no sleeps) ----------

func TestNoFlowQueueAndTTLExpiry(t *testing.T) {
	clk := &vclock{now: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	gw, inj := newTestGW(t, clk)

	id, err := gw.Submit(context.Background(), Command{Kind: KindWorkMode, Params: map[Param]int{pMode: 0}})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if st, ok := pendingStatus(gw, id); !ok || st != StatusQueued {
		t.Fatalf("status = %q (ok=%v), want queued", st, ok)
	}
	if got := len(inj.injected()); got != 0 {
		t.Fatalf("injected %d frames with no flow, want 0", got)
	}

	clk.advance(4 * time.Minute)
	if st, _ := pendingStatus(gw, id); st != StatusQueued {
		t.Fatalf("at 4 min: status = %q, want queued", st)
	}
	clk.advance(2 * time.Minute) // 6 min > default 5 min TTL
	if st, _ := pendingStatus(gw, id); st != StatusExpired {
		t.Fatalf("at 6 min: status = %q, want expired", st)
	}

	// the expired command must NOT be submitted when the flow appears
	gw.SetFlow("c1")
	if got := len(inj.injected()); got != 0 {
		t.Errorf("injected %d frames after flow appeared, want 0 (expired)", got)
	}
	if st, _ := pendingStatus(gw, id); st != StatusExpired {
		t.Errorf("after flow: status = %q, want expired", st)
	}
}

func TestFlowAppearanceSubmitsQueued(t *testing.T) {
	clk := &vclock{now: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	gw, inj := newTestGW(t, clk)

	id, err := gw.Submit(context.Background(), Command{Kind: KindWorkMode, Params: map[Param]int{pMode: 1}})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	clk.advance(time.Minute)
	gw.SetFlow("c1")

	if st, _ := pendingStatus(gw, id); st != StatusInFlight {
		t.Fatalf("after SetFlow: status = %q, want in-flight", st)
	}
	calls := inj.injected()
	if len(calls) != 1 {
		t.Fatalf("injected %d frames, want 1", len(calls))
	}
	if calls[0].connID != "c1" {
		t.Errorf("connID = %q, want c1", calls[0].connID)
	}
	want := "7f7f120000001000060106a02800017be0f7f7"
	if hex(calls[0].raw) != want {
		t.Errorf("frame = %s, want %s", hex(calls[0].raw), want)
	}
	// complete it
	if aerr := ack(gw, id, []byte{0x01, 0x06, 0xa0, 0x28, 0x00, 0x01}); aerr != nil {
		t.Fatalf("ack: %v", aerr)
	}
	if st, ok := pendingStatus(gw, id); ok {
		t.Errorf("after ack: still pending with status %q", st)
	}
}

func TestQueuedCommandFIFOOnFlowAppearance(t *testing.T) {
	clk := &vclock{now: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	gw, inj := newTestGW(t, clk)

	id1, _ := gw.Submit(context.Background(), Command{Kind: KindWorkMode, Params: map[Param]int{pMode: 0}})
	clk.advance(10 * time.Second)
	id2, _ := gw.Submit(context.Background(), Command{Kind: KindMeterCompensation, Params: map[Param]int{pValue: -51}})
	gw.SetFlow("c1")

	// only the first command may be in flight; the second stays queued
	if st, _ := pendingStatus(gw, id1); st != StatusInFlight {
		t.Errorf("id1 status = %q, want in-flight", st)
	}
	if st, _ := pendingStatus(gw, id2); st != StatusQueued {
		t.Errorf("id2 status = %q, want queued", st)
	}
	if got := len(inj.injected()); got != 1 {
		t.Fatalf("injected %d frames, want 1", got)
	}
	// ack the first; the second must then be submitted
	if aerr := ack(gw, id1, []byte{0x01, 0x06, 0xa0, 0x28, 0x00, 0x00}); aerr != nil {
		t.Fatalf("ack id1: %v", aerr)
	}
	if st, _ := pendingStatus(gw, id2); st != StatusInFlight {
		t.Errorf("after id1 ack: id2 status = %q, want in-flight", st)
	}
	if got := len(inj.injected()); got != 2 {
		t.Fatalf("injected %d frames, want 2", got)
	}
}

func TestSetTTLConfigurable(t *testing.T) {
	clk := &vclock{now: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	gw, _ := newTestGW(t, clk)
	gw.SetTTL(90 * time.Second)

	id, _ := gw.Submit(context.Background(), Command{Kind: KindWorkMode, Params: map[Param]int{pMode: 0}})
	clk.advance(89 * time.Second)
	if st, _ := pendingStatus(gw, id); st != StatusQueued {
		t.Fatalf("at 89 s: status = %q, want queued", st)
	}
	clk.advance(2 * time.Second)
	if st, _ := pendingStatus(gw, id); st != StatusExpired {
		t.Fatalf("at 91 s: status = %q, want expired", st)
	}
}

// ---------- in-flight exclusivity ----------

func TestSubmitInFlightRejects(t *testing.T) {
	clk := &vclock{now: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	gw, _ := newTestGW(t, clk)
	gw.SetFlow("c1")

	id, err := gw.Submit(context.Background(), Command{Kind: KindWorkMode, Params: map[Param]int{pMode: 1}})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, err := gw.Submit(context.Background(), Command{Kind: KindWorkMode, Params: map[Param]int{pMode: 0}}); !errors.Is(err, ErrInFlight) {
		t.Errorf("second submit: want ErrInFlight, got %v", err)
	}
	if aerr := ack(gw, id, []byte{0x01, 0x06, 0xa0, 0x28, 0x00, 0x01}); aerr != nil {
		t.Fatalf("ack: %v", aerr)
	}
	// now the slot is free again
	if _, err := gw.Submit(context.Background(), Command{Kind: KindWorkMode, Params: map[Param]int{pMode: 0}}); err != nil {
		t.Errorf("submit after ack: %v", err)
	}
}

func TestFlowLossFailsInFlight(t *testing.T) {
	clk := &vclock{now: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	gw, _ := newTestGW(t, clk)
	gw.SetFlow("c1")

	id, err := gw.Submit(context.Background(), Command{Kind: KindWorkMode, Params: map[Param]int{pMode: 1}})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	gw.SetFlow("")
	if st, ok := pendingStatus(gw, id); !ok || st != StatusFailed {
		t.Errorf("after flow loss: status = %q (ok=%v), want failed", st, ok)
	}
}
