// Maintenance window tests (2026-11-13 redesign): the installer-gated,
// time-boxed pass-through that replaces the retired C-002 firmware hold.
// Time-driven paths (expiry) run under synctest virtual time (C-004);
// the state transitions themselves are plain virtual-clock tests.
package forward

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"foxesslocal/internal/policy"
	"foxesslocal/internal/store"
)

// emitRec records the host-sink emissions (the spec's system events).
type emitRec struct {
	mu     sync.Mutex
	events []string // "type: detail" lines, in order
}

func (r *emitRec) emit(typ, detail string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, typ+": "+detail)
}

func (r *emitRec) lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

func (r *emitRec) types() []string {
	var out []string
	for _, l := range r.lines() {
		out = append(out, strings.SplitN(l, ":", 2)[0])
	}
	return out
}

// newTestMaintenance wires the machine over a virtual clock and a
// variable-backed configured-mode reader.
func newTestMaintenance(t *testing.T, mode *policy.ForwardMode) (*Maintenance, *emitRec, *testClock, *store.Store) {
	t.Helper()
	clk := &testClock{now: testBase}
	rec := &emitRec{}
	st := newTestStore(t)
	m := NewMaintenance(st, clk.get, func() policy.ForwardMode { return *mode }, rec.emit)
	return m, rec, clk, st
}

// TestOpenDefaultDuration: an absent/zero duration opens the default
// 30-minute window; the view, the audit row, and the opened event all
// name the actor, the duration, and the overridden mode.
func TestOpenDefaultDuration(t *testing.T) {
	mode := policy.ModeTelemetryOnly
	m, rec, _, h := newTestMaintenance(t, &mode)

	exp, err := m.Open(0, "alice")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !m.Active() {
		t.Fatal("window not active after open")
	}
	if want := testBase.Add(DefaultMaintenanceDuration); !exp.Equal(want) {
		t.Fatalf("expires %v, want %v", exp, want)
	}
	v := m.View()
	if !v.Active || v.Rearms != 0 || v.Base != policy.ModeTelemetryOnly {
		t.Fatalf("view: %+v", v)
	}
	if got := v.Remaining; got != DefaultMaintenanceDuration {
		t.Fatalf("remaining %v, want %v", got, DefaultMaintenanceDuration)
	}

	rows := auditRows(t, h, audKindMaintenance)
	if len(rows) != 1 {
		t.Fatalf("audit rows: %v", rows)
	}
	d := rows[0].Detail
	for _, want := range []string{"opened", "30m0s", "alice", "telemetry_only"} {
		if !strings.Contains(d, want) {
			t.Fatalf("audit %q: missing %q", d, want)
		}
	}
	types := rec.types()
	if len(types) != 1 || types[0] != EventMaintenanceOpened {
		t.Fatalf("events: %v", rec.lines())
	}
}

// TestOpenRearm: opening while active resets the deadline (re-arm),
// keeps the original open time, counts the rearm, and emits
// maintenance-extended (not -opened) with an audit row per re-arm.
func TestOpenRearm(t *testing.T) {
	mode := policy.ModeFull
	m, rec, clk, h := newTestMaintenance(t, &mode)

	if _, err := m.Open(time.Hour, "alice"); err != nil {
		t.Fatalf("Open: %v", err)
	}
	clk.advance(10 * time.Minute)
	exp, err := m.Open(30*time.Minute, "bob")
	if err != nil {
		t.Fatalf("re-arm: %v", err)
	}
	if want := testBase.Add(10*time.Minute + 30*time.Minute); !exp.Equal(want) {
		t.Fatalf("expires %v, want %v", exp, want)
	}
	v := m.View()
	if !v.Active || v.Rearms != 1 || !v.Opens.Equal(testBase) {
		t.Fatalf("view: %+v", v)
	}
	types := rec.types()
	if len(types) != 2 || types[0] != EventMaintenanceOpened || types[1] != EventMaintenanceExtended {
		t.Fatalf("events: %v", rec.lines())
	}
	rows := auditRows(t, h, audKindMaintenance)
	if len(rows) != 2 {
		t.Fatalf("audit rows: %v", rows)
	}
	if !strings.Contains(rows[1].Detail, "re-armed") || !strings.Contains(rows[1].Detail, "bob") {
		t.Fatalf("re-arm audit: %q", rows[1].Detail)
	}
}

// TestOpenRejectsOverCap: the per-call cap is 4h — 4h is accepted
// (boundary), anything above is refused with no state change.
func TestOpenRejectsOverCap(t *testing.T) {
	mode := policy.ModeOff
	m, _, _, _ := newTestMaintenance(t, &mode)

	if _, err := m.Open(MaxMaintenanceDuration, "alice"); err != nil {
		t.Fatalf("4h must be accepted: %v", err)
	}
	if !m.Active() {
		t.Fatal("window not active after 4h open")
	}
	if _, err := m.Open(time.Hour, "bob"); err != nil {
		t.Fatalf("re-arm at the cap: %v", err)
	}
	// A fresh machine: 4h0m1s is refused, nothing becomes active.
	mode2 := policy.ModeTelemetryOnly
	m2, rec2, _, _ := newTestMaintenance(t, &mode2)
	if _, err := m2.Open(MaxMaintenanceDuration+time.Second, "alice"); !errors.Is(err, ErrDurationTooLong) {
		t.Fatalf("want ErrDurationTooLong, got %v", err)
	}
	if m2.Active() || len(rec2.lines()) != 0 {
		t.Fatalf("refused open must change no state: active=%v events=%v", m2.Active(), rec2.lines())
	}
}

// TestClose: closing an active window restores the configured mode
// (view inactive, audit + closed event); closing an inactive window is
// ErrNotActive (the API's 409).
func TestClose(t *testing.T) {
	mode := policy.ModeTelemetryOnly
	m, rec, _, h := newTestMaintenance(t, &mode)

	if err := m.Close("alice"); !errors.Is(err, ErrNotActive) {
		t.Fatalf("want ErrNotActive, got %v", err)
	}
	if _, err := m.Open(30*time.Minute, "alice"); err != nil {
		t.Fatalf("Open: %v", err)
	}
	// The configured mode may move while the window is open; the
	// restore report names the mode that was overridden at open.
	mode = policy.ModeFull
	if err := m.Close("alice"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if m.Active() {
		t.Fatal("still active after close")
	}
	if v := m.View(); v.Active || v.Base != "" {
		t.Fatalf("view after close: %+v", v)
	}
	types := rec.types()
	if len(types) != 2 || types[1] != EventMaintenanceClosed {
		t.Fatalf("events: %v", rec.lines())
	}
	rows := auditRows(t, h, audKindMaintenance)
	if len(rows) != 2 {
		t.Fatalf("audit rows: %v", rows)
	}
	if !strings.Contains(rows[1].Detail, "closed") || !strings.Contains(rows[1].Detail, "telemetry_only") {
		t.Fatalf("close audit: %q", rows[1].Detail)
	}
}

// assertEmit checks the emitted event types (in order).
func assertEmit(t *testing.T, rec *emitRec, want ...string) {
	t.Helper()
	got := rec.types()
	if len(got) != len(want) {
		t.Fatalf("events: want %v, got %v", want, rec.lines())
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("events: want %v, got %v", want, rec.lines())
		}
	}
}

// TestExpiryFiresEventAndRestores: the Run loop (synctest virtual time)
// expires the window at its deadline — maintenance-expired is emitted,
// the audit row names the restored mode, and the view goes inactive.
// A re-armed deadline is the one that expires (the original one is
// superseded).
func TestExpiryFiresEventAndRestores(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mode := policy.ModeTelemetryOnly
		st := newTestStore(t)
		rec := &emitRec{}
		m := NewMaintenance(st, time.Now, func() policy.ForwardMode { return mode }, rec.emit)

		if _, err := m.Open(2*time.Second, "alice"); err != nil {
			t.Fatalf("Open: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		runDone := make(chan struct{})
		go func() { m.Run(ctx); close(runDone) }()
		defer func() {
			cancel()
			<-runDone
		}()

		time.Sleep(time.Second) // well inside the window
		if !m.Active() {
			t.Fatal("window expired early")
		}
		assertEmit(t, rec, EventMaintenanceOpened)

		time.Sleep(3 * time.Second) // past the 2s deadline (the tick fires)
		if m.Active() {
			t.Fatal("window still active past its deadline")
		}
		assertEmit(t, rec, EventMaintenanceOpened, EventMaintenanceExpired)
		rows := auditRows(t, st, audKindMaintenance)
		if len(rows) != 2 {
			t.Fatalf("audit rows: %v", rows)
		}
		if !strings.Contains(rows[1].Detail, "expired") || !strings.Contains(rows[1].Detail, "telemetry_only") {
			t.Fatalf("expiry audit: %q", rows[1].Detail)
		}
	})
}

// TestExpiryHonoursRearm: re-arming moves the deadline — the original
// expiry does not fire until the re-armed deadline passes.
func TestExpiryHonoursRearm(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mode := policy.ModeOff
		st := newTestStore(t)
		rec := &emitRec{}
		m := NewMaintenance(st, time.Now, func() policy.ForwardMode { return mode }, rec.emit)

		if _, err := m.Open(2*time.Second, "alice"); err != nil {
			t.Fatalf("Open: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		runDone := make(chan struct{})
		go func() { m.Run(ctx); close(runDone) }()
		defer func() {
			cancel()
			<-runDone
		}()

		time.Sleep(1500 * time.Millisecond)
		if _, err := m.Open(4*time.Second, "bob"); err != nil {
			t.Fatalf("re-arm: %v", err)
		}
		time.Sleep(1500 * time.Millisecond) // 3s: past the original 2s deadline, inside the re-armed 5s
		if !m.Active() {
			t.Fatal("window expired at the superseded deadline")
		}
		time.Sleep(3500 * time.Millisecond) // 6.5s: past the 5.5s deadline
		// (the 6 s tick has run — checking at exactly 6 s would race
		// the tick and the test goroutine at the same virtual instant)
		if m.Active() {
			t.Fatal("window still active past the re-armed deadline")
		}
		types := rec.types()
		if len(types) != 3 || types[2] != EventMaintenanceExpired {
			t.Fatalf("events: %v", rec.lines())
		}
	})
}
