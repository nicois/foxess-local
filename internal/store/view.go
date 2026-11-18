package store

import (
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/nicois/prox/pkg/capture"

	"foxesslocal/internal/frame"
)

// Frame verdicts whose Modbus writes reached the inverter: a cloud write
// the policy forwarded, or our own injected write. A dropped write
// changes nothing.
const (
	decisionForwarded = DecisionForwarded
	decisionInjected  = DecisionInjected
	decisionDropped   = DecisionDropped
)

// fnWriteOne is the Modbus write-single-register function.
const fnWriteOne = 0x06

// view is the newest-values view behind QueryState, RegisterRead,
// RegistersRange and LatestPayload. The Record* calls update it
// synchronously, so a read right after a record sees the value before
// the batch writer flushes; it is seeded from the database at open. The
// database stays the durable copy; the view only spares the readers a
// query (on a small device /state is the hottest path).
type view struct {
	mu   sync.RWMutex
	tele State // TeleTS + the telemetry fields
	snap State // SnapTS + the snap61 fields
	regs map[int]regVal
	// payloads: the newest payload per frame class, filled on record and
	// on a read miss (known=true once the class was looked up).
	payloads map[frame.Kind]classPayload
}

type regVal struct {
	v  uint16
	ts time.Time
}

type classPayload struct {
	p     []byte
	ts    time.Time
	known bool
}

func newView() *view {
	return &view{regs: map[int]regVal{}, payloads: map[frame.Kind]classPayload{}}
}

// setTele records one telemetry sample (the newest by ts wins; a tie
// goes to the later record, as the rowid order did).
func (v *view) setTele(ts time.Time, t frame.Tele) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if ts.Before(v.tele.TeleTS) {
		return
	}
	v.tele = State{
		TeleTS: ts, Sol: t.Sol, Grid: t.Grid, Haus: t.Haus, Acport: t.Acport,
		Batt: t.Batt, Eps: t.Eps, Soc: int(t.Soc), ChargeState: int(t.ChargeState),
		IDGrid: t.IDGrid, IDAc: t.IDAcport, HausDup: t.HausDup, Temps: t.Temps, InvState: t.InvState,
	}
}

// setSnap records one parsed snap61 (newest by ts).
func (v *view) setSnap(ts time.Time, snap frame.Snap61) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if ts.Before(v.snap.SnapTS) {
		return
	}
	st := State{SnapTS: ts, SocPct: int(snap.SocPct), EnergyUnits: snap.EnergyUnits}
	if snap.HasCellTemps {
		st.CellMinDeciC, st.CellMaxDeciC = &snap.CellMinDeciC, &snap.CellMaxDeciC
	}
	st.BusVoltage = make([]int, 0, len(snap.BusVoltage))
	for _, b := range snap.BusVoltage {
		st.BusVoltage = append(st.BusVoltage, int(b))
	}
	v.snap = st
}

// cellTemps returns the newest snapshot's cell temperatures when it is at
// most maxAge older than ts.
func (v *view) cellTemps(ts time.Time, maxAge time.Duration) (lo, hi *int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.snap.CellMinDeciC == nil || ts.Sub(v.snap.SnapTS) > maxAge || v.snap.SnapTS.Sub(ts) > maxAge {
		return nil, nil
	}
	return v.snap.CellMinDeciC, v.snap.CellMaxDeciC
}

// setRegs records a register block (each address's latest value; the
// last record wins, like the table's upsert) and returns the addresses
// whose value changed or was unknown.
func (v *view) setRegs(ts time.Time, addr int, vals []uint16) []int {
	v.mu.Lock()
	defer v.mu.Unlock()
	var changed []int
	for i, x := range vals {
		if old, ok := v.regs[addr+i]; !ok || old.v != x {
			changed = append(changed, addr+i)
		}
		v.regs[addr+i] = regVal{v: x, ts: ts}
	}
	return changed
}

// setPayload records the newest payload of a frame class.
func (v *view) setPayload(class frame.Kind, ts time.Time, p []byte) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if cur, ok := v.payloads[class]; ok && cur.known && ts.Before(cur.ts) {
		return
	}
	v.payloads[class] = classPayload{p: append([]byte(nil), p...), ts: ts, known: true}
}

// state assembles the snapshot (staleness is the caller's).
func (v *view) state() State {
	v.mu.RLock()
	defer v.mu.RUnlock()
	st := v.tele
	st.SnapTS, st.SocPct, st.EnergyUnits = v.snap.SnapTS, v.snap.SocPct, v.snap.EnergyUnits
	st.CellMinDeciC, st.CellMaxDeciC = v.snap.CellMinDeciC, v.snap.CellMaxDeciC
	st.BusVoltage = slices.Clone(v.snap.BusVoltage)
	v.scheduleLocked(&st)
	return st
}

// scheduleLocked decodes the 48010 ×80 time-group table from the
// registers (written or read) when all 80 are known; its timestamp is
// the oldest of the 80 (a block mixed from two sources dates from the
// earlier one).
func (v *view) scheduleLocked(st *State) {
	data := make([]byte, 0, schedRegs*regBytes)
	var oldest time.Time
	for a := schedAddr48010; a < schedAddr48010+schedRegs; a++ {
		r, ok := v.regs[a]
		if !ok {
			return
		}
		if oldest.IsZero() || r.ts.Before(oldest) {
			oldest = r.ts
		}
		data = binary.BigEndian.AppendUint16(data, r.v)
	}
	st.Schedule = decodeSlots(data)
	st.SchedTS = oldest
}

// observeFrame applies one recorded frame to the view: telemetry,
// snap61, the class's newest payload, and the registers a write that
// reached the inverter changed (returned for persisting).
func (v *view) observeFrame(r *FrameRecord, noTele bool) (addr int, vals []uint16, wrote bool) {
	v.setPayload(r.Class, r.TS, r.Payload)
	// frame.Kind is open-ended (SnapKind): the known classes, then the default
	//exhaustive:ignore
	switch r.Class {
	case classTele:
		if t, err := frame.ParseTele(r.Payload); err == nil && !noTele {
			v.setTele(r.TS, t)
		}
	case classSnap61:
		if snap, err := frame.ParseSnap61(r.Payload); err == nil {
			v.setSnap(r.TS, snap)
		}
	case classMbWrite:
		if r.Dir != capture.DirS2C || (r.Decision != decisionForwarded && r.Decision != decisionInjected) {
			return 0, nil, false
		}
		addr, vals, ok := writtenRegisters(r.Payload)
		if !ok {
			return 0, nil, false
		}
		return addr, vals, true
	}
	return 0, nil, false
}

// writtenRegisters extracts the registers a Modbus write PDU sets (fn06
// one register, fn10 a block).
func writtenRegisters(p []byte) (int, []uint16, bool) {
	mb, err := frame.ParseModbus(p)
	if err != nil {
		return 0, nil, false
	}
	switch mb.Fn {
	case fnWriteOne:
		return mb.Addr, []uint16{uint16(mb.Value)}, true //nolint:gosec // G115: a 16-bit register value.
	case schedFn10:
		if mb.Count == 0 || len(mb.Data) != mb.Count*regBytes {
			return 0, nil, false
		}
		vals := make([]uint16, mb.Count)
		for i := range vals {
			vals[i] = binary.BigEndian.Uint16(mb.Data[regBytes*i:])
		}
		return mb.Addr, vals, true
	}
	return 0, nil, false
}

// seed loads the view from the database (open): every register, the
// newest telemetry row and the newest valid snap61.
func (s *Store) seedView() error {
	var st State
	if err := s.latestTele(&st); err != nil {
		return fmt.Errorf("seed telemetry: %w", err)
	}
	s.view.tele = st
	var sn State
	if err := s.latestSnap61(&sn); err != nil {
		return fmt.Errorf("seed snap61: %w", err)
	}
	s.view.snap = sn
	rows, err := s.db.Query(`SELECT addr, value, ts FROM registers`)
	if err != nil {
		return fmt.Errorf("seed registers: %w", err)
	}
	for rows.Next() {
		var (
			addr, v int
			tsText  string
		)
		if err := rows.Scan(&addr, &v, &tsText); err != nil {
			return closeRows(rows, err)
		}
		ts, err := decTS(tsText)
		if err != nil {
			return closeRows(rows, err)
		}
		s.view.regs[addr] = regVal{v: uint16(v), ts: ts} //nolint:gosec // G115: registers are stored as 16-bit values.
	}
	return closeRows(rows, rows.Err())
}

// latestPayloadDB is LatestPayload's read-miss path: the newest frame of
// a class, walking the ts index from the newest end.
func (s *Store) latestPayloadDB(class frame.Kind) ([]byte, time.Time, bool, error) {
	var (
		p      []byte
		tsText string
	)
	err := s.rdb.QueryRow(`
		SELECT payload, ts FROM frames INDEXED BY idx_frames_class_ts WHERE class = ?
		ORDER BY ts DESC LIMIT 1`, class).Scan(&p, &tsText)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, time.Time{}, false, nil
	}
	if err != nil {
		return nil, time.Time{}, false, err
	}
	ts, err := decTS(tsText)
	if err != nil {
		return nil, time.Time{}, false, err
	}
	return p, ts, true, nil
}
