package store

import (
	"encoding/binary"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/nicois/prox/pkg/capture"
)

// testSlot describes one of the eight 48010 time groups for slotData.
type testSlot struct {
	en, start, end, wm, maxMin, fdSoc, fdPwr int
}

// slotData packs eight groups into the 48010 x80 register block per the
// mon150 printSlotTable layout.
func slotData(groups [8]testSlot) []byte {
	p := make([]byte, schedRegs*regBytes)
	put := func(i, v int) {
		//nolint:gosec // G115: register values are 16-bit by protocol.
		binary.BigEndian.PutUint16(p[regBytes*i:], uint16(v))
	}
	for k, g := range groups {
		b := groupRegs * k
		put(b+slotEnOff, g.en)
		put(b+slotStartOff, g.start)
		put(b+slotEndOff, g.end)
		put(b+slotWMOff, g.wm)
		put(b+slotSocOff, g.maxMin)
		put(b+slotFDSocOff, g.fdSoc)
		put(b+slotFDPwrOff, g.fdPwr)
	}
	return p
}

// mbWritePayload builds an fn10 write-multiple PDU (unit 01).
func mbWritePayload(addr, count int, data []byte) []byte {
	p := make([]byte, fn10HdrLen+len(data))
	p[0] = 1
	p[1] = schedFn10
	//nolint:gosec // G115: PDU fields are bounded protocol integers.
	binary.BigEndian.PutUint16(p[2:4], uint16(addr))
	//nolint:gosec // G115: PDU fields are bounded protocol integers.
	binary.BigEndian.PutUint16(p[4:6], uint16(count))
	//nolint:gosec // G115: PDU fields are bounded protocol integers.
	p[6] = byte(len(data))
	copy(p[7:], data)
	return p
}

// fn06Payload builds an fn06 single-register write PDU (unit 01).
func fn06Payload(addr, val int) []byte {
	p := make([]byte, fn06PDUlen)
	p[0] = 1
	p[1] = fn06Test
	//nolint:gosec // G115: PDU fields are bounded protocol integers.
	binary.BigEndian.PutUint16(p[2:4], uint16(addr))
	//nolint:gosec // G115: PDU fields are bounded protocol integers.
	binary.BigEndian.PutUint16(p[4:6], uint16(val))
	return p
}

// snap61Payload builds a 626 B snap61 payload with SN, SoC, stored energy,
// and module-table bus voltages at the profile §4 layout.
func snap61Payload(sn string, soc, energyUnits uint16, volts []uint16) []byte {
	const (
		tlvBase      = 40
		modBase      = 412
		tlvStride    = 12
		socTag       = 0x1905
		socTagIdx    = 16
		energyTag    = 0x1907
		energyTagIdx = 18
		voltValOff   = 6
		voltTagLo    = 0x0c05
	)
	p := make([]byte, snapLenTest)
	copy(p, sn)
	putTLV := func(base, idx int, tag uint16, off int, v uint16) {
		e := base + tlvStride*idx
		binary.BigEndian.PutUint16(p[e:e+2], tag)
		binary.LittleEndian.PutUint16(p[e+2+off:e+2+off+2], v)
	}
	putTLV(tlvBase, socTagIdx, socTag, voltValOff, soc)
	putTLV(tlvBase, energyTagIdx, energyTag, 0, energyUnits)
	for i, v := range volts {
		putTLV(modBase, i, voltTagLo+uint16(i), voltValOff, v)
	}
	return p
}

// TestQueryState pins the state snapshot: the latest telemetry row, the
// latest snap61 battery snapshot, the latest fn10 schedule push to 48010
// (a stale push and an fn06 write must not win), and staleness against
// the injected clock.
func TestQueryState(t *testing.T) {
	s := newTestStore(t)

	empty, err := s.QueryState()
	if err != nil {
		t.Fatalf("QueryState empty: %v", err)
	}
	if !reflect.DeepEqual(empty, State{}) {
		t.Fatalf("empty state: got %+v", empty)
	}

	seedStateFrames(t, s)
	s.now = func() time.Time { return testBase.Add(time.Hour) }
	st, err := s.QueryState()
	if err != nil {
		t.Fatalf("QueryState: %v", err)
	}

	checkStateFields(t, st)
	checkSchedule(t, st)

	const wantStale = time.Hour - 30*time.Second
	if st.Staleness != wantStale {
		t.Errorf("staleness: got %v, want %v", st.Staleness, wantStale)
	}
}

// checkStateFields asserts the tele, snap61, and schedule timestamps of
// the state snapshot.
func checkStateFields(t *testing.T, st State) {
	t.Helper()
	if !st.TeleTS.Equal(testBase.Add(10 * time.Second)) {
		t.Errorf("TeleTS: got %v", st.TeleTS)
	}
	if st.Sol != 1111 || st.Grid != -222 || st.Haus != 333 || st.Acport != 444 ||
		st.Batt != -55 || st.Soc != 77 || st.ChargeState != 4 {
		t.Errorf("tele fields: got %+v", st)
	}
	if !st.SnapTS.Equal(testBase.Add(20 * time.Second)) {
		t.Errorf("SnapTS: got %v", st.SnapTS)
	}
	if st.SocPct != 57 || st.EnergyUnits != stateEnergyUnits ||
		!slices.Equal(st.BusVoltage, []int{412, 413}) {
		t.Errorf("snap fields: got soc=%d units=%d volts=%v", st.SocPct, st.EnergyUnits, st.BusVoltage)
	}
	if !st.SchedTS.Equal(testBase.Add(30 * time.Second)) {
		t.Errorf("SchedTS: got %v", st.SchedTS)
	}
}

// State fixture values.
const (
	stateEnergyUnits = 984
	stateMaxMinA     = (80 << 8) | 10
	stateMaxMinC     = 100 << 8
	stateLimitAddr   = 41023
	stateLimitVal    = 4973
)

// seedStateFrames records the frames the state test asserts on: two
// teles (5 s and 10 s), a snap61 at 20 s, a stale forwarded 48010 push at
// 25 s, an fn06 limit write at 29 s, the current (injected) 48010 push at
// 30 s, and a newer push the policy dropped at 40 s (never applied).
func seedStateFrames(t *testing.T, s *Store) {
	t.Helper()
	oldest := frameRecord(testConnID, 1, capture.DirC2S, classTele,
		telePayload(1, 2, 3, 4, 5, 9, 2))
	oldest.TS = testBase.Add(5 * time.Second)
	latest := frameRecord(testConnID, 2, capture.DirC2S, classTele,
		telePayload(1111, -222, 333, 444, -55, 77, 4))
	latest.TS = testBase.Add(10 * time.Second)
	snap := frameRecord(testConnID, 3, capture.DirS2C, classSnap61,
		snap61Payload("INVSN12345678", 57, stateEnergyUnits, []uint16{412, 413}))
	snap.TS = testBase.Add(20 * time.Second)
	stale := frameRecord(testConnID, 4, capture.DirS2C, classMbWrite,
		mbWritePayload(schedAddr48010, schedRegs, slotData([8]testSlot{})))
	stale.TS = testBase.Add(25 * time.Second)
	stale.Decision = decisionForwarded
	limit := frameRecord(testConnID, 5, capture.DirS2C, classMbWrite,
		fn06Payload(stateLimitAddr, stateLimitVal))
	limit.TS = testBase.Add(29 * time.Second)
	limit.Decision = decisionForwarded
	wantSched := [8]testSlot{
		{en: 1, start: 6 << 8, end: 9 << 8, wm: 1, maxMin: stateMaxMinA, fdSoc: 30, fdPwr: 5000},
		{},
		{en: 1, start: 18 << 8, end: 20 << 8, wm: 7, maxMin: stateMaxMinC, fdSoc: 20, fdPwr: 100},
	}
	cur := frameRecord(testConnID, 6, capture.DirS2C, classMbWrite,
		mbWritePayload(schedAddr48010, schedRegs, slotData(wantSched)))
	cur.TS = testBase.Add(30 * time.Second)
	cur.Decision = decisionInjected
	dropped := frameRecord(testConnID, 7, capture.DirS2C, classMbWrite,
		mbWritePayload(schedAddr48010, schedRegs, slotData([8]testSlot{})))
	dropped.TS = testBase.Add(40 * time.Second)
	dropped.Decision = decisionDropped
	for _, r := range []FrameRecord{oldest, latest, snap, stale, limit, cur, dropped} {
		s.RecordFrame(r)
	}
	s.Flush()
}

// checkSchedule asserts the decoded 48010 schedule slots.
func checkSchedule(t *testing.T, st State) {
	t.Helper()
	if len(st.Schedule) != 8 {
		t.Fatalf("slots: got %d, want 8", len(st.Schedule))
	}
	wantA := Slot{
		Enabled: true, WorkMode: 1, StartH: 6, EndH: 9,
		MaxSoc: 80, MinSoc: 10, FDSoc: 30, FDPower: 5000,
	}
	if st.Schedule[0] != wantA {
		t.Errorf("slot 1: got %+v, want %+v", st.Schedule[0], wantA)
	}
	if st.Schedule[1].Enabled {
		t.Errorf("slot 2 must be off: got %+v", st.Schedule[1])
	}
	wantC := Slot{
		Enabled: true, WorkMode: 7, StartH: 18, EndH: 20,
		MaxSoc: 100, FDSoc: 20, FDPower: 100,
	}
	if st.Schedule[2] != wantC {
		t.Errorf("slot 3: got %+v, want %+v", st.Schedule[2], wantC)
	}
}
