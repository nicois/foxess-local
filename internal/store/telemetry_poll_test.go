package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/nicois/prox/pkg/capture"

	"foxesslocal/internal/frame"
)

// TestRecordTelemetry: the live-value poller writes a telemetry row
// directly (no 150 B frame behind it); /state's source query sees it
// exactly like a frame-projected row.
func TestRecordTelemetry(t *testing.T) {
	s := newTestStore(t)
	ts := time.Date(2026, 11, 16, 3, 52, 57, 0, time.UTC)
	s.RecordTelemetry("c1", ts, frame.Tele{Sol: 4726, Grid: -76, Haus: 4326, Acport: 4398, Batt: 28, Soc: 100})
	s.Flush()
	st, err := s.QueryState()
	if err != nil {
		t.Fatal(err)
	}
	if !st.TeleTS.Equal(ts) {
		t.Fatalf("TeleTS %v, want %v", st.TeleTS, ts)
	}
	if st.Sol != 4726 || st.Grid != -76 || st.Haus != 4326 || st.Acport != 4398 || st.Batt != 28 || st.Soc != 100 {
		t.Fatalf("state: %+v", st)
	}
}

// TestTelemetryEPS: the EPS port power is stored with each sample, shown
// in the state and averaged over a window.
func TestTelemetryEPS(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.db")
	s, err := openStore(path, nil, maxConns, fixedClock(testBase), testVersion)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		s.RecordTelemetry(testConnID, testBase.Add(time.Duration(i)*5*time.Second), frame.Tele{Sol: 1000, Eps: int32(300 + 100*i)})
	}
	st, err := s.QueryState()
	if err != nil {
		t.Fatal(err)
	}
	if st.Eps != 500 {
		t.Fatalf("state eps %d, want 500", st.Eps)
	}
	s.Flush()
	a, ok, err := s.WindowAverage(testBase, testBase.Add(time.Minute))
	if err != nil || !ok || a.EpsAvg != 400 {
		t.Fatalf("average eps %d ok=%v err=%v, want 400", a.EpsAvg, ok, err)
	}
	_ = s.Close()
	s2, err := openStore(path, nil, maxConns, fixedClock(testBase), testVersion)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close() }()
	if st, _ := s2.QueryState(); st.Eps != 500 {
		t.Fatalf("eps after reopen %d, want 500", st.Eps)
	}
}

// TestPollOutranksTeleFrame: while the register poll supplies telemetry
// (a sample within pollPrecedence), a 150 B tele frame — the cloud's
// model, no EPS field — is logged as a frame but neither replaces the
// live values nor becomes a telemetry row; with the poll stale it does.
func TestPollOutranksTeleFrame(t *testing.T) {
	s := newTestStore(t)
	s.RecordTelemetry(testConnID, testBase, frame.Tele{Sol: 1000, Haus: 4600, Eps: 3800})
	f := frameRecord(testConnID, 1, capture.DirC2S, classTele, telePayload(1000, 10800, 4600, -6200, -7900, 33, 4))
	f.TS = testBase.Add(2500 * time.Millisecond)
	s.RecordFrame(f)
	if !s.PollFresh(f.TS) {
		t.Fatal("PollFresh: false 2.5 s after a poll")
	}
	st, _ := s.QueryState()
	if st.Eps != 3800 || !st.TeleTS.Equal(testBase) {
		t.Fatalf("live values replaced by the frame: eps %d ts %v", st.Eps, st.TeleTS)
	}
	late := frameRecord(testConnID, 2, capture.DirC2S, classTele, telePayload(1000, 10800, 4600, -6200, -7900, 33, 4))
	late.TS = testBase.Add(time.Minute) // the poll went quiet
	s.RecordFrame(late)
	if st, _ := s.QueryState(); !st.TeleTS.Equal(late.TS) {
		t.Fatalf("a frame with the poll stale was not used: %v", st.TeleTS)
	}
	s.Flush()
	if n := countRows(t, s, "telemetry"); n != 2 {
		t.Fatalf("telemetry rows %d, want 2 (the poll + the late frame)", n)
	}
	if n := countRows(t, s, tableFrames); n != 2 {
		t.Fatalf("frame rows %d, want both frames logged", n)
	}
}
