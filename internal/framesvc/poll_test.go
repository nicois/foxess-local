package framesvc

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"foxesslocal/internal/frame"
)

// fakePollDeps scripts the poller's four seams.
type fakePollDeps struct {
	mu       sync.Mutex
	ready    bool
	readErr  error
	reads    []int // block start addresses, in order
	recorded []frame.Tele
	events   []Event
	regs     []int // recorded register block start addresses
	failAddr int   // a read of this block fails (0: none)
}

func (f *fakePollDeps) poller() *poller {
	return &poller{
		target: func(time.Time) (string, bool) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return "c1", f.ready
		},
		read: func(_ context.Context, _ int, addr, count int) ([]uint16, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.reads = append(f.reads, addr)
			if f.readErr != nil {
				return nil, f.readErr
			}
			if addr == f.failAddr {
				return nil, errors.New("exception")
			}
			out := make([]uint16, count)
			if addr == frame.KHRealtimeAAddr {
				out[24] = 77 // SoC
			}
			return out, nil
		},
		record: func(_ string, _ time.Time, t frame.Tele) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.recorded = append(f.recorded, t)
		},
		recordRegs: func(_ time.Time, addr int, _ []uint16) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.regs = append(f.regs, addr)
		},
		observe: func(ev Event) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.events = append(f.events, ev)
		},
		clock: time.Now,
	}
}

// TestPollerCadence: every pollEvery, both blocks are read and one
// telemetry row + one telemetry event land.
func TestPollerCadence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakePollDeps{ready: true}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { f.poller().run(ctx); close(done) }()
		time.Sleep(3*pollEvery + pollEvery/2)
		synctest.Wait()
		cancel()
		<-done
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(f.recorded) != 3 || len(f.events) != 3 {
			t.Fatalf("after 3.5 periods: %d rows, %d events, want 3 each", len(f.recorded), len(f.events))
		}
		if f.recorded[0].Soc != 77 || f.events[0].Kind != EventTelemetry || f.events[0].ConnID != "c1" {
			t.Fatalf("row %+v event %+v", f.recorded[0], f.events[0])
		}
		want := []int{frame.KHRealtimeAAddr, frame.KHRealtimeBAddr}
		live := liveReads(f.reads)
		if len(live) != 6 || live[0] != want[0] || live[1] != want[1] {
			t.Fatalf("reads %v, want pairs of %v", live, want)
		}
	})
}

// TestPollerWaitsForTarget: no established session → no reads at all.
func TestPollerWaitsForTarget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakePollDeps{}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { f.poller().run(ctx); close(done) }()
		time.Sleep(5 * pollEvery)
		synctest.Wait()
		cancel()
		<-done
		if len(f.reads) != 0 || len(f.recorded) != 0 {
			t.Fatalf("polled without a target: reads %v rows %d", f.reads, len(f.recorded))
		}
	})
}

// TestPollerSkipsFailedRead: a failed read (busy gateway, timeout,
// exception) records nothing for that tick and the loop keeps going.
func TestPollerSkipsFailedRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakePollDeps{ready: true, readErr: errors.New("busy")}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { f.poller().run(ctx); close(done) }()
		time.Sleep(2*pollEvery + pollEvery/2)
		synctest.Wait()
		f.mu.Lock()
		f.readErr = nil
		f.mu.Unlock()
		time.Sleep(pollEvery)
		synctest.Wait()
		cancel()
		<-done
		if len(f.recorded) != 1 {
			t.Fatalf("rows %d, want 1 (two failed ticks skipped, then one good)", len(f.recorded))
		}
	})
}

// TestPollTargetSettles: the active flow becomes a poll target only
// pollSettle after its first keepalive (never mid-handshake).
func TestPollTargetSettles(t *testing.T) {
	t0 := time.Date(2026, 11, 16, 4, 0, 0, 0, time.UTC)
	s := &Service{flows: map[string]*flow{}}
	if _, ok := s.pollTarget(t0); ok {
		t.Fatal("target with no active flow")
	}
	fl := &flow{leg: stubLeg{}} // a locally served session
	s.flows["c1"] = fl
	s.active = "c1"
	if _, ok := s.pollTarget(t0); ok {
		t.Fatal("target before the first keepalive")
	}
	fl.firstKeepalive = t0
	if _, ok := s.pollTarget(t0.Add(pollSettle - time.Second)); ok {
		t.Fatal("target before pollSettle")
	}
	if id, ok := s.pollTarget(t0.Add(pollSettle)); !ok || id != "c1" {
		t.Fatalf("target at pollSettle: %q %v", id, ok)
	}
}

// settingsRegs filters the live-value blocks out of the recorded
// register blocks (the poller records both).
func settingsRegs(regs []int) []int {
	var out []int
	for _, a := range regs {
		if a != frame.KHRealtimeAAddr && a != frame.KHRealtimeBAddr {
			out = append(out, a)
		}
	}
	return out
}

// TestPollerRecordsLiveRegisters: every tick also records the raw live
// blocks, so the compatibility checks (PV V×A = W, grid V/Hz) can run
// on the measured values, not the derived telemetry row.
func TestPollerRecordsLiveRegisters(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakePollDeps{ready: true}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { f.poller().run(ctx); close(done) }()
		time.Sleep(2*pollEvery + pollEvery/2)
		synctest.Wait()
		cancel()
		<-done
		f.mu.Lock()
		defer f.mu.Unlock()
		if live := liveReads(f.regs); len(live) != 4 {
			t.Fatalf("recorded live blocks %v over 2 ticks, want A,B ×2", live)
		}
	})
}

// liveReads filters the live-value block reads out of a read log.
func liveReads(reads []int) []int {
	var out []int
	for _, a := range reads {
		if a == frame.KHRealtimeAAddr || a == frame.KHRealtimeBAddr {
			out = append(out, a)
		}
	}
	return out
}

// TestPollerReadsSettings: the settings blocks (work mode, the 41007
// readback with the limits and meter compensation, the scheduler) are
// read on the first poll of a session and then every settingsEvery —
// not on every 5 s tick.
func TestPollerReadsSettings(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakePollDeps{ready: true}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { f.poller().run(ctx); close(done) }()
		time.Sleep(3*pollEvery + pollEvery/2)
		synctest.Wait()
		f.mu.Lock()
		got := settingsRegs(f.regs)
		f.mu.Unlock()
		// identity first (model 30000 ×15, versions 30016 ×3 — D-020),
		// then the settings
		want := []int{30000, 30016, 40000, 41000, 41007, 41200, 48000, 48010, 57000, 57050, 57100}
		if len(got) != len(want) {
			t.Fatalf("after 3 ticks: recorded blocks %v, want one round %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("blocks %v, want %v", got, want)
			}
		}
		time.Sleep(settingsEvery)
		synctest.Wait()
		cancel()
		<-done
		if n := len(settingsRegs(f.regs)); n != 2*len(want) {
			t.Fatalf("after settingsEvery: %d blocks, want a second round (%d)", n, 2*len(want))
		}
	})
}

// TestPollerSettingsRetry: a failed settings block leaves the round
// due — it is retried on the next tick, not settingsEvery later.
func TestPollerSettingsRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakePollDeps{ready: true, failAddr: 48010}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { f.poller().run(ctx); close(done) }()
		time.Sleep(pollEvery + pollEvery/2)
		synctest.Wait()
		f.mu.Lock()
		f.failAddr = 0
		f.mu.Unlock()
		time.Sleep(pollEvery)
		synctest.Wait()
		cancel()
		<-done
		n48010 := 0
		for _, a := range f.regs {
			if a == 48010 {
				n48010++
			}
		}
		if n48010 != 1 || len(f.recorded) != 2 {
			t.Fatalf("48010 recorded %d times (want 1, on the retry tick); %d telemetry rows (want 2)", n48010, len(f.recorded))
		}
	})
}

// stubLeg marks a flow as locally served (ServeLocal) in pollTarget tests.
type stubLeg struct{}

func (stubLeg) RelayLeg() io.ReadWriteCloser { return nil }
func (stubLeg) Drain() io.ReadCloser         { return nil }

// TestPollTargetPollsMirror (D-024): a mirrored session (telemetry_only,
// full, a maintenance window) is polled like a local one — the inverter's
// answers are routed to whoever asked (route.go), so a cloud answer is
// never taken as ours and ours never reach the cloud. (It was skipped
// before the routing existed, which left telemetry_only without live
// data.)
func TestPollTargetPollsMirror(t *testing.T) {
	t0 := time.Date(2026, 11, 16, 4, 0, 0, 0, time.UTC)
	s := &Service{flows: map[string]*flow{}}
	s.flows["c1"] = &flow{firstKeepalive: t0} // no leg: mirrored
	s.active = "c1"
	if id, ok := s.pollTarget(t0.Add(time.Hour)); !ok || id != "c1" {
		t.Fatal("mirrored session not polled")
	}
}

// TestPollerIdentityBlockOptional: an inverter that answers the identity
// reads with an exception (another family's register map) still gets its
// settings read, and the failed identity block does not make the round
// retry every tick — it is tried again with the next round.
func TestPollerIdentityBlockOptional(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakePollDeps{ready: true, failAddr: 30000}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { f.poller().run(ctx); close(done) }()
		time.Sleep(3*pollEvery + pollEvery/2)
		synctest.Wait()
		cancel()
		<-done
		f.mu.Lock()
		defer f.mu.Unlock()
		want := []int{30016, 40000, 41000, 41007, 41200, 48000, 48010, 57000, 57050, 57100}
		if got := settingsRegs(f.regs); len(got) != len(want) {
			t.Fatalf("recorded blocks %v, want one round without 30000: %v", got, want)
		}
		n30000 := 0
		for _, a := range f.reads {
			if a == 30000 {
				n30000++
			}
		}
		if n30000 != 1 {
			t.Fatalf("30000 read %d times in 3 ticks, want 1 (no per-tick retry)", n30000)
		}
	})
}
