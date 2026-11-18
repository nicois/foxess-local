package forward

import (
	"io"
	"path/filepath"
	"testing"
	"time"

	"foxesslocal/internal/framesvc"
	"foxesslocal/internal/policy"
	"foxesslocal/internal/store"
)

var testBase = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

// testClock is the injected clock (C-004: time-driven logic runs on
// virtual time, never wall-clock sleeps).
type testClock struct{ now time.Time }

func (c *testClock) get() time.Time          { return c.now }
func (c *testClock) advance(d time.Duration) { c.now = c.now.Add(d) }

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "forward.db"), "dev")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// auditRows flushes the store and returns the audit rows of one kind.
// Store reads run in the test's outer scope (the flusher's channels are
// real-time owned; bubble-safe handshake).
func auditRows(t *testing.T, st *store.Store, kind string) []store.AuditEvent {
	t.Helper()
	st.Flush()
	rows, err := st.QueryAudit(time.Time{}, kind)
	if err != nil {
		t.Fatalf("QueryAudit(%s): %v", kind, err)
	}
	return rows
}

// TestNewDefaultsToTelemetryOnly: an empty initial mode defaults to
// telemetry_only (the cutover-safe default; the host constructs the
// machine without a default).
func TestNewDefaultsToTelemetryOnly(t *testing.T) {
	st := newTestStore(t)
	clk := &testClock{now: testBase}
	m := New(st, clk.get, "")
	got, ts, err := m.Get()
	if err != nil || got != policy.ModeTelemetryOnly || !ts.Equal(testBase) {
		t.Fatalf("Get: got %v %v (err %v), want telemetry_only @ base", got, ts, err)
	}
}

// TestSetUnknownModeRefused: Set of an unknown mode errors and changes
// neither the state nor the audit trail.
func TestSetUnknownModeRefused(t *testing.T) {
	st := newTestStore(t)
	clk := &testClock{now: testBase}
	m := New(st, clk.get, policy.ModeFull)

	if err := m.Set(policy.ForwardMode("bogus"), "alice"); err == nil {
		t.Fatal("Set(unknown): want error, got nil")
	}
	got, ts, err := m.Get()
	if err != nil || got != policy.ModeFull || !ts.Equal(testBase) {
		t.Fatalf("Get after refused Set: got %v %v (err %v), want full @ base", got, ts, err)
	}
	if rows := auditRows(t, st, audKindForwardMode); len(rows) != 0 {
		t.Fatalf("refused Set wrote %d audit rows, want 0", len(rows))
	}
}

// TestSetValidWritesAuditRow: a valid Set applies atomically and records
// the exact ConfigForwardMode audit shape (the framesvc audit rows are
// the log surface).
func TestSetValidWritesAuditRow(t *testing.T) {
	st := newTestStore(t)
	clk := &testClock{now: testBase}
	m := New(st, clk.get, policy.ModeFull)

	clk.advance(time.Minute)
	if err := m.Set(policy.ModeOff, "alice"); err != nil {
		t.Fatalf("Set(off): %v", err)
	}
	got, ts, err := m.Get()
	if err != nil || got != policy.ModeOff || !ts.Equal(testBase.Add(time.Minute)) {
		t.Fatalf("Get: got %v %v (err %v), want off @ base+1m", got, ts, err)
	}
	rows := auditRows(t, st, audKindForwardMode)
	if len(rows) != 1 {
		t.Fatalf("audit rows: got %d, want 1", len(rows))
	}
	if rows[0].Kind != audKindForwardMode || rows[0].Detail != "→ off by alice" {
		t.Fatalf("audit row: got %q %q, want kind %q detail %q",
			rows[0].Kind, rows[0].Detail, audKindForwardMode, "→ off by alice")
	}
	if !rows[0].TS.Equal(testBase.Add(time.Minute)) {
		t.Fatalf("audit ts: got %v, want base+1m", rows[0].TS)
	}
}

// TestDecidePerMode: each mode maps to its serve decision (off →
// ServeLocal with the in-memory leg, the other two → ServeMirror with no
// leg), in the exact reason strings the audit rows carry.
func TestDecidePerMode(t *testing.T) {
	st := newTestStore(t)
	clk := &testClock{now: testBase}
	m := New(st, clk.get, policy.ModeFull)

	for _, tc := range []struct {
		mode   policy.ForwardMode
		want   framesvc.ServeMode
		leg    bool
		reason string
	}{
		{policy.ModeFull, framesvc.ServeMirror, false, "forward-mode full: mirror"},
		{policy.ModeTelemetryOnly, framesvc.ServeMirror, false, "forward-mode telemetry_only: mirror"},
		{policy.ModeOff, framesvc.ServeLocal, true, "forward-mode off: local"},
	} {
		if err := m.Set(tc.mode, "t"); err != nil {
			t.Fatalf("Set(%s): %v", tc.mode, err)
		}
		got, leg, reason, err := m.Decide(nil)
		if err != nil {
			t.Fatalf("Decide(%s): %v", tc.mode, err)
		}
		if got != tc.want || (leg != nil) != tc.leg || reason != tc.reason {
			t.Fatalf("Decide(%s): got %v leg=%v reason=%q, want %v leg=%v reason=%q",
				tc.mode, got, leg != nil, reason, tc.want, tc.leg, tc.reason)
		}
		if leg != nil {
			if err := leg.Drain().Close(); err != nil {
				t.Fatalf("leg.Drain().Close: %v", err)
			}
		}
	}
}

// TestLegRoundTrip: the off-mode leg round-trips c2s (the relay writes
// through RelayLeg, framesvc drains through Drain) and closes cleanly —
// the drain reader exits on close, further operations fail, and the
// close is idempotent across the two views (the relay closes the relay
// leg, framesvc closes the drain leg — the same close runs once), the
// contract framesvc's close-with-connection depends on.
func TestLegRoundTrip(t *testing.T) {
	st := newTestStore(t)
	clk := &testClock{now: testBase}
	m := New(st, clk.get, policy.ModeOff)

	mode, lc, _, err := m.Decide(nil)
	if err != nil || mode != framesvc.ServeLocal {
		t.Fatalf("Decide: got %v (err %v)", mode, err)
	}
	relay := lc.RelayLeg()
	drain := lc.Drain()

	// c2s: the relay writes client→upstream bytes through RelayLeg;
	// framesvc's drain goroutine reads them through Drain.
	go func() { _, _ = relay.Write([]byte("c2s-bytes")) }()
	got := make([]byte, 9)
	if _, err := io.ReadFull(drain, got); err != nil {
		t.Fatalf("drain.Read(c2s): %v", err)
	}
	if string(got) != "c2s-bytes" {
		t.Fatalf("drain.Read(c2s): got %q", got)
	}

	// The drain-style reader (framesvc's leg goroutine) exits on close.
	done := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, drain); close(done) }()
	if err := drain.Close(); err != nil {
		t.Fatalf("drain.Close: %v", err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("drain reader did not exit after Close")
	}
	if _, err := drain.Read(make([]byte, 1)); err == nil {
		t.Error("drain.Read after Close: want error")
	}
	if _, err := relay.Write([]byte("x")); err == nil {
		t.Error("relay.Write after Close: want error")
	}
	// Idempotent across the two views: the relay closes its view after
	// framesvc already closed the drain view — the close ran once, the
	// second view's close is a no-op.
	if err := relay.Close(); err != nil {
		t.Fatalf("relay.Close after drain.Close: %v", err)
	}
	if err := drain.Close(); err != nil {
		t.Fatalf("drain.Close twice: %v", err)
	}
}

// TestSetChangesNewConnectionDecision: a Set flips the decision for
// connections opened afterward (the established-flow half — that the
// flip does NOT re-serve a live flow — is proven against the real
// framesvc.Service in TestR3EstablishmentTimeDecision).
func TestSetChangesNewConnectionDecision(t *testing.T) {
	st := newTestStore(t)
	clk := &testClock{now: testBase}
	m := New(st, clk.get, policy.ModeOff)

	if mode, _, _, err := m.Decide(nil); err != nil || mode != framesvc.ServeLocal {
		t.Fatalf("off: got %v (err %v)", mode, err)
	}
	if err := m.Set(policy.ModeFull, "t"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if mode, leg, _, err := m.Decide(nil); err != nil || mode != framesvc.ServeMirror || leg != nil {
		t.Fatalf("full: got %v leg=%v (err %v)", mode, leg != nil, err)
	}
}

// TestStateOnChange (F12): every applied change reaches the change hook
// (the host persists it, so a restart keeps the mode); a refused one
// does not.
func TestStateOnChange(t *testing.T) {
	clk := &testClock{now: time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)}
	s := New(newTestStore(t), clk.get, policy.ModeOff)
	var got []string
	s.OnChange(func(m policy.ForwardMode, acting string) error {
		got = append(got, string(m)+" by "+acting)
		return nil
	})
	if err := s.Set(policy.ModeFull, "installer"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set("turbo", "installer"); err == nil {
		t.Fatal("unknown mode accepted")
	}
	if len(got) != 1 || got[0] != "full by installer" {
		t.Fatalf("hook calls %v", got)
	}
}
