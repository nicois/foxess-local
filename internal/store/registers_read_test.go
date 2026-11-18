package store

import (
	"testing"
	"time"

	"github.com/nicois/prox/pkg/capture"
)

// TestRecordRegistersLatest: a register block read lands per address;
// a later read of the same address replaces the earlier one.
func TestRecordRegistersLatest(t *testing.T) {
	s := newTestStore(t)
	t0 := time.Date(2026, 11, 16, 6, 0, 0, 0, time.UTC)
	s.RecordRegisters(t0, 41021, []uint16{12500, 48, 5000})
	s.RecordRegisters(t0.Add(time.Minute), 41023, []uint16{4000})
	s.Flush()
	cases := []struct {
		addr int
		want int
		ts   time.Time
	}{
		{41021, 12500, t0},
		{41022, 48, t0},
		{41023, 4000, t0.Add(time.Minute)},
	}
	for _, c := range cases {
		v, ts, ok, err := s.RegisterRead(c.addr)
		if err != nil || !ok || v != c.want || !ts.Equal(c.ts) {
			t.Errorf("@%d: %d %v %v %v, want %d at %v", c.addr, v, ts, ok, err, c.want, c.ts)
		}
	}
	if _, _, ok, err := s.RegisterRead(41029); ok || err != nil {
		t.Errorf("unread register: ok %v err %v", ok, err)
	}
}

// TestScheduleFromRegisterRead: a 48010 ×80 read is a schedule source
// like a confirmed write — the newer of the two wins.
func TestScheduleFromRegisterRead(t *testing.T) {
	s := newTestStore(t)
	t0 := time.Date(2026, 11, 16, 6, 0, 0, 0, time.UTC)
	block := make([]uint16, 80)
	block[0] = 1      // group 1 enabled
	block[1] = 0x0B02 // 11:02
	block[2] = 0x0D3A // 13:58
	block[3] = 6      // force charge
	block[4] = 0x640A // max 100 : min 10
	block[5] = 99
	block[6] = 9948
	s.RecordRegisters(t0, 48010, block)
	s.Flush()
	st, err := s.QueryState()
	if err != nil {
		t.Fatal(err)
	}
	if !st.SchedTS.Equal(t0) || len(st.Schedule) != 8 {
		t.Fatalf("SchedTS %v, %d groups", st.SchedTS, len(st.Schedule))
	}
	g := st.Schedule[0]
	if !g.Enabled || g.StartH != 11 || g.StartM != 2 || g.EndH != 13 || g.EndM != 58 ||
		g.WorkMode != 6 || g.MaxSoc != 100 || g.MinSoc != 10 || g.FDSoc != 99 || g.FDPower != 9948 {
		t.Fatalf("group 1: %+v", g)
	}

	// a partial block (fewer than 80 registers read) is not a schedule
	s2 := newTestStore(t)
	s2.RecordRegisters(t0, 48010, block[:40])
	s2.Flush()
	if st, _ := s2.QueryState(); len(st.Schedule) != 0 {
		t.Fatalf("partial block decoded: %+v", st.Schedule)
	}
}

// TestLatestPayload: the newest frame of a class wins; an absent class
// is (nil, false).
func TestLatestPayload(t *testing.T) {
	s := newTestStore(t)
	t0 := time.Date(2026, 11, 16, 6, 0, 0, 0, time.UTC)
	for i, p := range [][]byte{{1}, {2}} {
		s.RecordFrame(FrameRecord{
			ConnID: "c1", Dir: capture.DirC2S, TS: t0.Add(time.Duration(i) * time.Minute),
			Class: "snap01", Plen: 1, Payload: p, Decision: decisionForwardedTest,
		})
	}
	s.Flush()
	p, ok, err := s.LatestPayload("snap01")
	if err != nil || !ok || len(p) != 1 || p[0] != 2 {
		t.Fatalf("latest snap01: %v %v %v, want [2]", p, ok, err)
	}
	if p, ok, err := s.LatestPayload("snap60"); ok || p != nil || err != nil {
		t.Fatalf("absent class: %v %v %v", p, ok, err)
	}
}

// TestRegistersRange: one query returns every read register in
// [lo, hi]; unread addresses are absent.
func TestRegistersRange(t *testing.T) {
	s := newTestStore(t)
	t0 := time.Date(2026, 11, 16, 6, 0, 0, 0, time.UTC)
	s.RecordRegisters(t0, 41007, []uint16{450, 430})
	s.RecordRegisters(t0, 41029, []uint16{0xFFCD})
	s.RecordRegisters(t0, 48000, []uint16{1})
	s.Flush()
	got, err := s.RegistersRange(41000, 41046)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[41007] != 450 || got[41008] != 430 || got[41029] != 0xFFCD {
		t.Fatalf("range: %v", got)
	}
}

// TestFramesBetween: frames with from <= ts < to, oldest first.
func TestFramesBetween(t *testing.T) {
	s := newTestStore(t)
	t0 := time.Date(2026, 11, 16, 6, 0, 0, 0, time.UTC)
	for i := range 3 {
		s.RecordFrame(FrameRecord{
			ConnID: "c1", Seq: i, Dir: capture.DirC2S, TS: t0.Add(time.Duration(i) * time.Hour),
			Class: "keepalive", Plen: 1, Payload: []byte{byte(i)}, Decision: decisionForwardedTest,
		})
	}
	s.Flush()
	got, err := s.FramesBetween(t0.Add(30*time.Minute), t0.Add(3*time.Hour))
	if err != nil || len(got) != 2 || got[0].Payload[0] != 1 || got[1].Payload[0] != 2 {
		t.Fatalf("frames %+v err %v", got, err)
	}
}

// decisionForwardedTest is the frames decision used by these tests.
const decisionForwardedTest = "forwarded"
