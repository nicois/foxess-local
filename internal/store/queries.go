package store

import (
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/nicois/prox/pkg/capture"

	"foxesslocal/internal/frame"
)

// Schedule and scan constants (profile §5; mon150 slot semantics).
const (
	schedFn10      = 0x10
	schedAddr48010 = 48010
	schedRegs      = 80
	groupRegs      = 10
	regBytes       = 2
	highByteShift  = 8
	lowByteMask    = 0xFF
	classSnap61    = frame.KindSnap61
	classMbWrite   = frame.KindMbWrite
	fn06Test       = 0x06
	fn10HdrLen     = 7
	fn06PDUlen     = 6
)

// Slot register offsets inside one 48010 time group (mon150
// printSlotTable: en, start, end, workmode, maxsoc:minsoc, fdsoc, fdpwr,
// res x3).
const (
	slotEnOff    = 0
	slotStartOff = 1
	slotEndOff   = 2
	slotWMOff    = 3
	slotSocOff   = 4
	slotFDSocOff = 5
	slotFDPwrOff = 6
	// slotAfterOff: the action after a force window's cut-off SoC
	// (1 standby, 3 self-use — profile §5, 2026-11-18).
	slotAfterOff  = 8
	schedFetchCap = 64
)

// Telemetry is one 150 B tele frame projected to a row.
type Telemetry struct {
	ConnID      string
	TS          time.Time
	Sol         int32
	Grid        int32
	Haus        int32
	Acport      int32
	Batt        int32
	Soc         int
	ChargeState int
	IDGrid      int
	IDAc        int
	HausDup     int
}

// AuditEvent is one audit_events row.
type AuditEvent struct {
	TS     time.Time
	Kind   string
	Detail string
}

// Slot is one of the eight time groups of the 48010 schedule block.
type Slot struct {
	Enabled  bool
	WorkMode int
	StartH   int
	StartM   int
	EndH     int
	EndM     int
	MaxSoc   int
	MinSoc   int
	FDSoc    int
	FDPower  int
	// AfterCutoff is the raw +8 register: the action once a force
	// window reaches its cut-off SoC (1 standby, 3 self-use).
	AfterCutoff int
}

// State is the operator-facing snapshot that the /api/v1 state endpoint
// (task 7) consumes: the latest telemetry row, the latest snap61 battery
// snapshot, the latest fn10 schedule push to 48010, and staleness.
type State struct {
	// Latest telemetry row (zero fields when the store has no telemetry).
	TeleTS time.Time
	Sol    int32
	Grid   int32
	Haus   int32
	Acport int32
	Batt   int32
	Eps    int32 // EPS (backup / UPS) port output W
	Soc    int
	// ChargeState is the tele charge-state byte (02 idle / 04 charging).
	ChargeState int
	IDGrid      int
	IDAc        int
	HausDup     int
	// Temps are the latest sample's temperatures and BMS charge limit
	// (nil when it carried none; never mutated after it is stored).
	Temps *frame.Temps
	// InvState is the latest sample's inverter state (31027; nil when
	// it carried none).
	InvState *int
	// Latest snap61 battery snapshot (zero when absent).
	SnapTS time.Time
	// SocPct is the battery state of charge in percent.
	SocPct int
	// EnergyUnits is the raw snap61 1907 counter (unit undetermined —
	// usable energy is SoC × the configured capacity, computed in api).
	EnergyUnits uint16
	BusVoltage  []int // pack DC-bus registers 0c05-0c0d, 0.01 V units
	// CellMinDeciC / CellMaxDeciC: the BMS's lowest / highest cell
	// temperature (snap61 tag 1874, 0.1 °C); nil when not reported.
	CellMinDeciC, CellMaxDeciC *int
	// Latest fn10 schedule push to 48010 (absent until the first push).
	SchedTS  time.Time
	Schedule []Slot
	// Staleness is now minus the newest of the three data timestamps; 0
	// when the store is empty.
	Staleness time.Duration
}

// TelemetryRange returns telemetry rows with since <= ts < until
// (inclusive of since, exclusive of until), ordered by ts.
func (s *Store) TelemetryRange(since, until time.Time) ([]Telemetry, error) {
	s.catchUp()
	rows, err := s.rdb.Query(`
		SELECT conn_id, ts, sol, grid, haus, acport, batt, soc, charge_state,
			idnt_grid, idnt_ac, haus_dup
		FROM telemetry WHERE ts >= ? AND ts < ?
		ORDER BY ts, rowid`, encTS(since), encTS(until))
	if err != nil {
		return nil, err
	}
	out := []Telemetry{}
	for rows.Next() {
		var (
			m      Telemetry
			tsText string
		)
		if err := rows.Scan(&m.ConnID, &tsText, &m.Sol, &m.Grid, &m.Haus,
			&m.Acport, &m.Batt, &m.Soc, &m.ChargeState, &m.IDGrid, &m.IDAc,
			&m.HausDup); err != nil {
			return nil, closeRows(rows, err)
		}
		if m.TS, err = decTS(tsText); err != nil {
			return nil, closeRows(rows, err)
		}
		out = append(out, m)
	}
	return out, closeRows(rows, rows.Err())
}

// QueryAudit returns audit events with ts >= since (inclusive), filtered
// by kind when given, ordered by ts.
func (s *Store) QueryAudit(since time.Time, kinds ...string) ([]AuditEvent, error) {
	s.catchUp()
	q := "SELECT ts, kind, detail FROM audit_events WHERE ts >= ?"
	args := []any{encTS(since)}
	if len(kinds) > 0 {
		//nolint:gosec // G202: only a fixed prefix and placeholders are concatenated; kinds are bound args.
		q += " AND kind IN (" + placeholders(len(kinds)) + ")"
		args = append(args, values(kinds)...)
	}
	q += " ORDER BY ts, rowid"
	rows, err := s.rdb.Query(q, args...)
	if err != nil {
		return nil, err
	}
	out := []AuditEvent{}
	for rows.Next() {
		var (
			m      AuditEvent
			tsText string
		)
		if err := rows.Scan(&tsText, &m.Kind, &m.Detail); err != nil {
			return nil, closeRows(rows, err)
		}
		if m.TS, err = decTS(tsText); err != nil {
			return nil, closeRows(rows, err)
		}
		out = append(out, m)
	}
	return out, closeRows(rows, rows.Err())
}

// AuditFrames returns the frames with the given verdict and direction —
// the C-001 audit ("which s2c frames passed, and by which open state") — ordered by
// ts, conn, and seq.
func (s *Store) AuditFrames(decision Decision, dir capture.Dir) ([]FrameRecord, error) {
	s.catchUp()
	rows, err := s.rdb.Query(`
		SELECT conn_id, seq, dir, off, ts, class, dev, fn, plen, payload,
			decision, reason, origin, decoded
		FROM frames WHERE decision = ? AND dir = ?
		ORDER BY ts, conn_id, seq`, decision, dir.String())
	if err != nil {
		return nil, err
	}
	return scanFrames(rows)
}

// FramesBetween returns every frame with from <= ts < to, oldest first
// (the diagnostic bundle's frame log).
func (s *Store) FramesBetween(from, to time.Time) ([]FrameRecord, error) {
	s.catchUp()
	rows, err := s.rdb.Query(`
		SELECT conn_id, seq, dir, off, ts, class, dev, fn, plen, payload,
			decision, reason, origin, decoded
		FROM frames WHERE ts >= ? AND ts < ?
		ORDER BY ts, conn_id, seq`, encTS(from), encTS(to))
	if err != nil {
		return nil, err
	}
	return scanFrames(rows)
}

// scanFrames reads frame rows (the AuditFrames / FramesBetween columns).
func scanFrames(rows *sql.Rows) ([]FrameRecord, error) {
	out := []FrameRecord{}
	for rows.Next() {
		var (
			r          FrameRecord
			dirText    string
			tsText     string
			devText    string
			decodedStr string
		)
		if err := rows.Scan(&r.ConnID, &r.Seq, &dirText, &r.Off, &tsText,
			&r.Class, &devText, &r.Fn, &r.Plen, &r.Payload, &r.Decision,
			&r.Reason, &r.Origin, &decodedStr); err != nil {
			return nil, closeRows(rows, err)
		}
		r.Dir = parseDir(dirText)
		var err error
		if r.TS, err = decTS(tsText); err != nil {
			return nil, closeRows(rows, err)
		}
		if _, err := hex.Decode(r.Dev[:], []byte(devText)); err != nil {
			return nil, closeRows(rows, fmt.Errorf("store: dev %q: %w", devText, err))
		}
		if decodedStr != "" {
			r.Decoded = json.RawMessage(decodedStr)
		}
		out = append(out, r)
	}
	return out, closeRows(rows, rows.Err())
}

// ChunkPayloadCount is the number of chunk rows carrying captured
// payload bytes (the trace-contribution surface: 0 = nothing captured).
func (s *Store) ChunkPayloadCount() (int, error) {
	var n int
	if err := s.rdb.QueryRow(`SELECT COUNT(*) FROM chunks WHERE payload IS NOT NULL`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: %w", err)
	}
	return n, nil
}

// QueryState assembles the state snapshot from memory (view.go): latest
// telemetry, latest snap61, the 48010 schedule (written or read), and
// staleness against the injected clock. No query: /state is the hottest
// path, and everything in it is already known when it is recorded.
func (s *Store) QueryState() (State, error) {
	st := s.view.state()
	st.Staleness = s.staleness(st)
	return st, nil
}

// RegisterRead is the latest known value of one holding register (read
// by the settings poll or set by a write that reached the inverter); ok
// is false when the address is unknown.
func (s *Store) RegisterRead(addr int) (int, time.Time, bool, error) {
	s.view.mu.RLock()
	defer s.view.mu.RUnlock()
	r, ok := s.view.regs[addr]
	if !ok {
		return 0, time.Time{}, false, nil
	}
	return int(r.v), r.ts, true, nil
}

// RegistersRange returns the latest known value of every register in
// [lo, hi] (unknown addresses are absent).
func (s *Store) RegistersRange(lo, hi int) (map[int]uint16, error) {
	s.view.mu.RLock()
	defer s.view.mu.RUnlock()
	out := map[int]uint16{}
	for a, r := range s.view.regs {
		if a >= lo && a <= hi {
			out[a] = r.v
		}
	}
	return out, nil
}

// LatestPayload returns the newest frame payload of a class (the
// identity snapshots: snap01 device info, snap60 battery modules) — from
// memory once the class was seen or looked up.
func (s *Store) LatestPayload(class frame.Kind) ([]byte, bool, error) {
	s.view.mu.RLock()
	cp, ok := s.view.payloads[class]
	s.view.mu.RUnlock()
	if ok && cp.known {
		return cp.p, cp.p != nil, nil
	}
	p, ts, found, err := s.latestPayloadDB(class)
	if err != nil {
		return nil, false, err
	}
	s.view.mu.Lock()
	if cur, ok := s.view.payloads[class]; !ok || !cur.known {
		s.view.payloads[class] = classPayload{p: p, ts: ts, known: true}
	} else {
		p, found = cur.p, cur.p != nil // recorded meanwhile: newer
	}
	s.view.mu.Unlock()
	return p, found, nil
}

// FrameSet is one (direction, verdict) slice of the frame log.
type FrameSet struct {
	Dir      capture.Dir
	Decision Decision
}

// FramePage selects one page of the frame log, newest first: frames in
// any of Sets (none = an empty page), of any of Classes (nil = every
// class), with Since <= ts < Until and ts < Before (zero = unbounded),
// at most Limit.
type FramePage struct {
	Sets                 []FrameSet
	Classes              []frame.Kind
	Since, Until, Before time.Time
	Limit                int
}

// FramesPage returns one page of the frame log (the /audit and
// /messages lists). Each (dir, decision) slice — or, with a class
// filter, each class — is one index range read from its newest end and
// cut at Limit; the ranges merge newest first. Once a page's worth is
// in hand, later ranges stop at its oldest timestamp. The cost follows
// the page size, never the log size, for common and rare filters alike.
func (s *Store) FramesPage(p FramePage) ([]FrameRecord, error) {
	s.catchUp()
	if len(p.Sets) == 0 || p.Limit <= 0 || (p.Classes != nil && len(p.Classes) == 0) {
		return []FrameRecord{}, nil
	}
	var out []FrameRecord
	for _, r := range pageRanges(p) {
		floor := time.Time{}
		if len(out) >= p.Limit {
			floor = pageFloor(out, p.Limit)
		}
		recs, err := s.framesRange(r.index, r.cond, r.args, p, floor)
		if err != nil {
			return nil, err
		}
		out = append(out, recs...)
	}
	sortNewest(out)
	if len(out) > p.Limit {
		out = out[:p.Limit]
	}
	return out, nil
}

// indexRange is one index range of a page query (fixed SQL fragments
// and their bound args).
type indexRange struct {
	index, cond string
	args        []any
}

// pageRanges splits a page query into index ranges: one per (dir,
// decision) slice, or one per class when the classes are filtered.
func pageRanges(p FramePage) []indexRange {
	if p.Classes == nil {
		out := make([]indexRange, 0, len(p.Sets))
		for _, fs := range p.Sets {
			out = append(out, indexRange{
				"idx_frames_dir_dec_ts", "dir = ? AND decision = ?",
				[]any{fs.Dir.String(), fs.Decision},
			})
		}
		return out
	}
	setSQL, setArgs := setsCond(p.Sets)
	out := make([]indexRange, 0, len(p.Classes))
	for _, c := range p.Classes {
		out = append(out, indexRange{
			"idx_frames_class_ts", "class = ? AND (" + setSQL + ")",
			append([]any{c}, setArgs...),
		})
	}
	return out
}

// setsCond is the OR of (dir, decision) slices (fixed fragments and
// their bound args).
func setsCond(sets []FrameSet) (string, []any) {
	terms := make([]string, 0, len(sets))
	var args []any
	for _, fs := range sets {
		terms = append(terms, "(dir = ? AND decision = ?)")
		args = append(args, fs.Dir.String(), fs.Decision)
	}
	return strings.Join(terms, " OR "), args
}

// sortNewest orders frames newest first (seq breaks a timestamp tie).
func sortNewest(recs []FrameRecord) {
	sort.SliceStable(recs, func(i, j int) bool {
		if !recs[i].TS.Equal(recs[j].TS) {
			return recs[i].TS.After(recs[j].TS)
		}
		return recs[i].Seq > recs[j].Seq
	})
}

// pageFloor is the Limit-th newest timestamp of recs: no older frame can
// make the page.
func pageFloor(recs []FrameRecord, limit int) time.Time {
	ts := make([]time.Time, 0, len(recs))
	for i := range recs {
		ts = append(ts, recs[i].TS)
	}
	sort.Slice(ts, func(i, j int) bool { return ts[i].After(ts[j]) })
	return ts[limit-1]
}

// framesRange reads one index range newest first (at most p.Limit, not
// older than floor when set). index and cond are fixed fragments.
func (s *Store) framesRange(index, cond string, args []any, p FramePage, floor time.Time) ([]FrameRecord, error) {
	where, args := appendWindow([]string{cond}, args, p.Since, p.Until, p.Before)
	if !floor.IsZero() {
		where = append(where, "ts >= ?")
		args = append(args, encTS(floor))
	}
	args = append(args, p.Limit)
	//nolint:gosec // G202: only fixed fragments and placeholders are concatenated; every value is a bound arg.
	q := `SELECT conn_id, seq, dir, off, ts, class, dev, fn, plen, payload,
			decision, reason, origin, decoded
		FROM frames INDEXED BY ` + index + `
		WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY ts DESC LIMIT ?`
	rows, err := s.rdb.Query(q, args...)
	if err != nil {
		return nil, err
	}
	return scanFrames(rows)
}

// AuditQuery selects one page of audit events, newest first.
type AuditQuery struct {
	Kinds                []string // nil = every kind
	Since, Until, Before time.Time
	Limit                int
}

// AuditPage returns one page of audit events (newest first, at most
// Limit; the ts index bounds the walk).
func (s *Store) AuditPage(a AuditQuery) ([]AuditEvent, error) {
	s.catchUp()
	if a.Limit <= 0 {
		return []AuditEvent{}, nil
	}
	where := []string{"1 = 1"}
	var args []any
	if a.Kinds != nil {
		if len(a.Kinds) == 0 {
			return []AuditEvent{}, nil
		}
		where = append(where, "kind IN ("+placeholders(len(a.Kinds))+")")
		args = append(args, values(a.Kinds)...)
	}
	where, args = appendWindow(where, args, a.Since, a.Until, a.Before)
	args = append(args, a.Limit)
	//nolint:gosec // G202: only fixed fragments and placeholders are concatenated; every value is a bound arg.
	q := `SELECT ts, kind, detail FROM audit_events
		WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY ts DESC, rowid DESC LIMIT ?`
	rows, err := s.rdb.Query(q, args...)
	if err != nil {
		return nil, err
	}
	out := []AuditEvent{}
	for rows.Next() {
		var (
			m      AuditEvent
			tsText string
		)
		if err := rows.Scan(&tsText, &m.Kind, &m.Detail); err != nil {
			return nil, closeRows(rows, err)
		}
		if m.TS, err = decTS(tsText); err != nil {
			return nil, closeRows(rows, err)
		}
		out = append(out, m)
	}
	return out, closeRows(rows, rows.Err())
}

// appendWindow adds the page window terms (since inclusive; until and
// before exclusive; zero = unbounded).
func appendWindow(where []string, args []any, since, until, before time.Time) ([]string, []any) {
	if !since.IsZero() {
		where = append(where, "ts >= ?")
		args = append(args, encTS(since))
	}
	if !until.IsZero() {
		where = append(where, "ts < ?")
		args = append(args, encTS(until))
	}
	if !before.IsZero() {
		where = append(where, "ts < ?")
		args = append(args, encTS(before))
	}
	return where, args
}

// latestTele fills the telemetry fields from the newest telemetry row.
func (s *Store) latestTele(st *State) error {
	rows, err := s.db.Query(`
		SELECT ts, sol, grid, haus, acport, batt, soc, charge_state,
			idnt_grid, idnt_ac, haus_dup, eps
		FROM telemetry ORDER BY ts DESC, rowid DESC LIMIT 1`)
	if err != nil {
		return err
	}
	if !rows.Next() {
		return closeRows(rows, rows.Err())
	}
	var tsText string
	if err := rows.Scan(&tsText, &st.Sol, &st.Grid, &st.Haus, &st.Acport,
		&st.Batt, &st.Soc, &st.ChargeState, &st.IDGrid, &st.IDAc,
		&st.HausDup, &st.Eps); err != nil {
		return closeRows(rows, err)
	}
	return closeRows(rows, setTS(&st.TeleTS, tsText))
}

// latestSnap61 fills the battery-snapshot fields from the newest snap61
// frame (a malformed payload leaves the fields zero).
func (s *Store) latestSnap61(st *State) error {
	rows, err := s.db.Query(`
		SELECT ts, payload FROM frames WHERE class = ?
		ORDER BY ts DESC, rowid DESC LIMIT 1`, classSnap61)
	if err != nil {
		return err
	}
	if !rows.Next() {
		return closeRows(rows, rows.Err())
	}
	var (
		tsText  string
		payload []byte
	)
	if err := rows.Scan(&tsText, &payload); err != nil {
		return closeRows(rows, err)
	}
	snap, err := frame.ParseSnap61(payload)
	if err != nil {
		return closeRows(rows, nil)
	}
	st.SocPct = int(snap.SocPct)
	st.EnergyUnits = snap.EnergyUnits
	st.BusVoltage = make([]int, 0, len(snap.BusVoltage))
	for _, v := range snap.BusVoltage {
		st.BusVoltage = append(st.BusVoltage, int(v))
	}
	if err := setTS(&st.SnapTS, tsText); err != nil {
		return closeRows(rows, err)
	}
	return closeRows(rows, nil)
}

// decodeSlots decodes the 48010 x80 block into eight time groups per the
// mon150 printSlotTable layout.
func decodeSlots(data []byte) []Slot {
	slots := make([]Slot, 0, schedRegs/groupRegs)
	r := func(i int) int {
		return int(binary.BigEndian.Uint16(data[regBytes*i : regBytes*(i+1)]))
	}
	for k := range schedRegs / groupRegs {
		b := groupRegs * k
		soc := r(b + slotSocOff)
		slots = append(slots, Slot{
			Enabled:     r(b+slotEnOff) != 0,
			WorkMode:    r(b + slotWMOff),
			StartH:      r(b+slotStartOff) >> highByteShift,
			StartM:      r(b+slotStartOff) & lowByteMask,
			EndH:        r(b+slotEndOff) >> highByteShift,
			EndM:        r(b+slotEndOff) & lowByteMask,
			MaxSoc:      soc >> highByteShift,
			MinSoc:      soc & lowByteMask,
			FDSoc:       r(b + slotFDSocOff),
			FDPower:     r(b + slotFDPwrOff),
			AfterCutoff: r(b + slotAfterOff),
		})
	}
	return slots
}

// staleness is now minus the newest data timestamp; 0 when empty.
func (s *Store) staleness(st State) time.Duration {
	newest := st.TeleTS
	if st.SnapTS.After(newest) {
		newest = st.SnapTS
	}
	if st.SchedTS.After(newest) {
		newest = st.SchedTS
	}
	if newest.IsZero() {
		return 0
	}
	return s.now().Sub(newest)
}

// parseDir maps the stored direction text back to a capture.Dir.
func parseDir(s string) capture.Dir {
	if s == "s2c" {
		return capture.DirS2C
	}
	return capture.DirC2S
}

// placeholders builds the "?, ?, ?" IN-list for n arguments.
func placeholders(n int) string {
	marks := make([]string, 0, n)
	for range n {
		marks = append(marks, "?")
	}
	return strings.Join(marks, ", ")
}

// values lifts string arguments to any for query parameter lists.
func values(ss []string) []any {
	out := make([]any, 0, len(ss))
	for _, v := range ss {
		out = append(out, v)
	}
	return out
}

// closeRows closes rows and wraps a close error into err (preferring the
// earlier error when both are set).
func closeRows(rows *sql.Rows, err error) error {
	if cerr := rows.Close(); cerr != nil && err == nil {
		err = cerr
	}
	return err
}

// setTS decodes one timestamp column into dst.
func setTS(dst *time.Time, v string) error {
	t, err := decTS(v)
	if err != nil {
		return err
	}
	*dst = t
	return nil
}
