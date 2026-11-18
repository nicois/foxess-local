package store

import (
	"runtime"
	"testing"
	"time"

	"foxesslocal/internal/frame"
)

// queued waits until the flusher has taken every enqueued item off the
// channel (applied or held in its batch).
func queued(s *Store) {
	for len(s.batch) > 0 {
		runtime.Gosched()
	}
}

// TestWriteDelayHolds (owner, 2026-11-18: fragile storage): with a write
// delay, records wait in memory past the old 128-record trigger; a read
// of recent data writes them first, so nothing seems missing.
func TestWriteDelayHolds(t *testing.T) {
	s := newTestStore(t) // injected tick: never fires here
	s.SetWriteDelay(time.Minute)
	soc := byte(50)
	for i := range 500 {
		s.RecordTelemetry("c1", testBase.Add(time.Duration(i)*time.Second), frame.Tele{Soc: soc})
	}
	queued(s)
	if got := s.applied.Load(); got != 0 {
		t.Fatalf("%d records written before the delay", got)
	}
	rows, err := s.TelemetryRange(testBase, testBase.Add(time.Hour))
	if err != nil || len(rows) != 500 {
		t.Fatalf("a read saw %d rows (%v), want all 500 written first", len(rows), err)
	}
}

// TestWriteDelayAuditUrgent: an audit entry (a command, a setting
// change) is written at once, with everything waiting before it.
func TestWriteDelayAuditUrgent(t *testing.T) {
	s := newTestStore(t)
	s.SetWriteDelay(15 * time.Minute)
	soc := byte(50)
	s.RecordTelemetry("c1", testBase, frame.Tele{Soc: soc})
	s.RecordAudit(testBase, "command", "limits by test")
	for s.applied.Load() < 2 {
		runtime.Gosched()
	}
	if n := countRows(t, s, "audit_events"); n != 1 {
		t.Fatalf("audit rows %d", n)
	}
}

// TestWriteDelayMemoryCap: a long delay still writes once maxPending
// records wait (memory stays bounded on a small board).
func TestWriteDelayMemoryCap(t *testing.T) {
	s := newTestStore(t)
	s.SetWriteDelay(15 * time.Minute)
	soc := byte(50)
	for i := range maxPending {
		s.RecordTelemetry("c1", testBase.Add(time.Duration(i)*time.Millisecond), frame.Tele{Soc: soc})
	}
	for s.applied.Load() < maxPending {
		runtime.Gosched()
	}
}

// TestWriteDelayDefault: without a delay set, reads do not force writes
// (the 250 ms tick does), and the delay reads back with its floor.
func TestWriteDelayDefault(t *testing.T) {
	s := newTestStore(t)
	if d := s.WriteDelay(); d != flushInterval {
		t.Fatalf("default delay %v", d)
	}
	s.SetWriteDelay(0)
	if d := s.WriteDelay(); d != flushInterval {
		t.Fatalf("delay 0 reads back %v, want the %v floor", d, flushInterval)
	}
}
