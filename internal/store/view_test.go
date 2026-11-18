package store

import (
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/nicois/prox/pkg/capture"

	"foxesslocal/internal/frame"
)

// TestStateBeforeFlush: QueryState and RegisterRead answer from memory —
// what was just recorded is visible before the batch writer flushes (a
// config event built right after a write must carry the new value).
func TestStateBeforeFlush(t *testing.T) {
	tick := make(chan time.Time) // never fires: nothing flushes on its own
	s, err := openStore(filepath.Join(t.TempDir(), "s.db"), tick, maxConns, fixedClock(testBase), testVersion)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	tele := frameRecord(testConnID, 1, capture.DirC2S, classTele, telePayload(1111, -222, 333, 444, -55, 77, 4))
	tele.TS = testBase.Add(10 * time.Second)
	s.RecordFrame(tele)
	s.RecordRegisters(testBase, 41000, []uint16{1})

	st, err := s.QueryState()
	if err != nil {
		t.Fatal(err)
	}
	if st.Sol != 1111 || !st.TeleTS.Equal(tele.TS) {
		t.Errorf("tele not visible before flush: %+v", st)
	}
	if v, _, ok, err := s.RegisterRead(41000); err != nil || !ok || v != 1 {
		t.Errorf("register not visible before flush: %d %v %v", v, ok, err)
	}
}

// TestWritesUpdateRegisters: a cloud write the policy forwarded and our
// own injected write update the registers (single and multiple); a
// dropped write never does.
func TestWritesUpdateRegisters(t *testing.T) {
	s := newTestStore(t)
	fwd := frameRecord(testConnID, 1, capture.DirS2C, classMbWrite, fn06Payload(41000, 1))
	fwd.Decision = decisionForwarded
	inj := frameRecord(testConnID, 2, capture.DirS2C, classMbWrite, mbWritePayload(41021, 2, []byte{0, 0, 0x30, 0xd4}))
	inj.Decision = decisionInjected
	inj.TS = testBase.Add(time.Second)
	drop := frameRecord(testConnID, 3, capture.DirS2C, classMbWrite, fn06Payload(41029, 7))
	drop.Decision = decisionDropped
	for _, r := range []FrameRecord{fwd, inj, drop} {
		s.RecordFrame(r)
	}
	want := map[int]int{41000: 1, 41021: 0, 41022: 12500}
	for addr, v := range want {
		got, _, ok, err := s.RegisterRead(addr)
		if err != nil || !ok || got != v {
			t.Errorf("@%d: %d ok=%v err=%v, want %d", addr, got, ok, err, v)
		}
	}
	if _, _, ok, _ := s.RegisterRead(41029); ok {
		t.Error("a dropped write updated the register")
	}
	s.Flush()
	if m, err := s.RegistersRange(41021, 41022); err != nil || m[41022] != 12500 {
		t.Errorf("RegistersRange: %v %v", m, err)
	}
}

// TestStateSurvivesReopen: the in-memory view is seeded from the
// database at open.
func TestStateSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.db")
	s, err := openStore(path, nil, maxConns, fixedClock(testBase), testVersion)
	if err != nil {
		t.Fatal(err)
	}
	seedStateFrames(t, s)
	before, err := s.QueryState()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := openStore(path, nil, maxConns, fixedClock(testBase), testVersion)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	after, err := s2.QueryState()
	if err != nil {
		t.Fatal(err)
	}
	checkStateFields(t, after)
	checkSchedule(t, after)
	if !before.TeleTS.Equal(after.TeleTS) || !before.SchedTS.Equal(after.SchedTS) {
		t.Errorf("reopen: before %+v after %+v", before, after)
	}
	if p, ok, err := s2.LatestPayload(classSnap61); err != nil || !ok || len(p) == 0 {
		t.Errorf("LatestPayload after reopen: %d bytes ok=%v err=%v", len(p), ok, err)
	}
}

// TestFramesPage: one page of the frame log, newest first, filtered by
// (dir, decision) sets, classes and the time window, capped by limit.
func TestFramesPage(t *testing.T) {
	s := newTestStore(t)
	p := telePayload(1, 2, 3, 4, 5, 6, 2)
	var recs []FrameRecord
	for i := range 10 {
		dir, class, decision := capture.DirS2C, classMbWrite, decisionForwarded
		if i%2 == 1 {
			dir, class, decision = capture.DirC2S, classTele, decisionForwarded
		}
		if i == 4 {
			decision = "dropped"
		}
		r := frameRecord(testConnID, i, dir, class, p)
		r.Decision = decision
		r.TS = testBase.Add(time.Duration(i) * time.Second)
		recs = append(recs, r)
	}
	for _, r := range recs {
		s.RecordFrame(r)
	}
	s.Flush()

	seqs := func(page FramePage) []int {
		t.Helper()
		got, err := s.FramesPage(page)
		if err != nil {
			t.Fatal(err)
		}
		out := make([]int, 0, len(got))
		for i := range got {
			out = append(out, got[i].Seq)
		}
		return out
	}
	s2cFwd := []FrameSet{{Dir: capture.DirS2C, Decision: decisionForwarded}}
	all := []FrameSet{
		{Dir: capture.DirS2C, Decision: decisionForwarded},
		{Dir: capture.DirS2C, Decision: decisionDropped},
		{Dir: capture.DirC2S, Decision: decisionForwarded},
	}
	cases := []struct {
		name string
		page FramePage
		want []int
	}{
		{"newest first, limit", FramePage{Sets: all, Limit: 3}, []int{9, 8, 7}},
		{"set filter", FramePage{Sets: s2cFwd, Limit: 10}, []int{8, 6, 2, 0}},
		{"class filter", FramePage{Sets: all, Classes: []frame.Kind{classTele}, Limit: 2}, []int{9, 7}},
		{"before cursor", FramePage{Sets: all, Before: testBase.Add(5 * time.Second), Limit: 2}, []int{4, 3}},
		{"since/until", FramePage{Sets: all, Since: testBase.Add(2 * time.Second), Until: testBase.Add(4 * time.Second), Limit: 10}, []int{3, 2}},
		{"no sets", FramePage{Limit: 10}, []int{}},
	}
	for _, c := range cases {
		if got := seqs(c.page); !slices.Equal(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// TestAuditPage: audit events of the given kinds, newest first, paged.
func TestAuditPage(t *testing.T) {
	s := newTestStore(t)
	for i := range 5 {
		kind := "command"
		if i == 2 {
			kind = "tls"
		}
		s.RecordAudit(testBase.Add(time.Duration(i)*time.Second), kind, "{}")
	}
	s.Flush()
	got, err := s.AuditPage(AuditQuery{Kinds: []string{"command"}, Before: testBase.Add(4 * time.Second), Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got[0].TS.Equal(testBase.Add(3*time.Second)) || !got[1].TS.Equal(testBase.Add(time.Second)) {
		t.Fatalf("AuditPage: %+v", got)
	}
}

// TestRegisterReadChanges: a register read that changes a known value
// (or learns a new one) is reported once, with the changed addresses;
// an unchanged read is silent, and so is a write that reached the
// inverter (its own config event already announces it).
func TestRegisterReadChanges(t *testing.T) {
	s := newTestStore(t)
	var got [][]int
	s.OnRegisterReadChange(func(addrs []int) { got = append(got, addrs) })

	s.RecordRegisters(testBase, 41000, []uint16{0, 7})
	s.RecordRegisters(testBase.Add(time.Minute), 41000, []uint16{0, 7}) // unchanged
	s.RecordRegisters(testBase.Add(2*time.Minute), 41000, []uint16{1, 7})
	w := frameRecord(testConnID, 1, capture.DirS2C, classMbWrite, fn06Payload(41001, 9))
	w.Decision = decisionInjected
	s.RecordFrame(w)
	want := [][]int{{41000, 41001}, {41000}}
	if len(got) != len(want) || !slices.Equal(got[0], want[0]) || !slices.Equal(got[1], want[1]) {
		t.Fatalf("changes %v, want %v", got, want)
	}
	if v, _, _, _ := s.RegisterRead(41001); v != 9 {
		t.Fatalf("the write did not update the register: %d", v)
	}
}

// TestReadsDoNotBlockWrites (audit R4): API reads use their own
// read-only connection, so an open read never holds up the write
// flusher (with one shared connection a slow history or message query
// stalled flushing, and a full write queue blocks the frame hook).
func TestReadsDoNotBlockWrites(t *testing.T) {
	s := newTestStore(t)
	s.RecordFrame(frameRecord(testConnID, 1, capture.DirC2S, classTele, telePayload(1, 2, 3, 4, 5, 6, 2)))
	s.Flush()
	rows, err := s.rdb.Query(`SELECT seq FROM frames`) //nolint:rowserrcheck // held open unread on purpose: a slow reader
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	s.RecordFrame(frameRecord(testConnID, 2, capture.DirC2S, classTele, telePayload(1, 2, 3, 4, 5, 6, 2)))
	done := make(chan struct{})
	go func() { s.Flush(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a held read blocked the write flush")
	}
	got, err := s.FramesPage(FramePage{Sets: []FrameSet{{Dir: capture.DirC2S, Decision: testDecision}}, Limit: 10})
	if err != nil || len(got) != 2 {
		t.Fatalf("FramesPage after the flush: %d rows, %v", len(got), err)
	}
}
