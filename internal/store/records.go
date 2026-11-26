package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"runtime"
	"time"

	"github.com/nicois/prox/pkg/capture"

	"foxesslocal/internal/frame"
)

// Batch-writer tuning. flushInterval is the production tick and the
// shortest write delay; tests inject their own tick channel so no
// wall-clock is needed to drive a flush.
//
// The write delay (SetWriteDelay; owner, 2026-11-18) lets records wait in
// memory between writes: fewer, larger transactions wear an SD card or
// eMMC far less, at the cost of losing what waits at a power cut. Audit
// entries (commands, setting changes) are written at once, with
// everything before them; reads of recent data write what waits first;
// a clean Close writes everything. maxPending bounds the memory.
const (
	flushInterval = 250 * time.Millisecond
	maxPending    = 10_000
	classTele     = frame.KindTele
)

// Decision is a frame's verdict (the frames.decision column).
type Decision string

// The verdicts.
const (
	// DecisionForwarded: the frame went on to its destination.
	DecisionForwarded Decision = "forwarded"
	// DecisionDropped: the policy kept it back (logged).
	DecisionDropped Decision = "dropped"
	// DecisionInjected: this app wrote it to the inverter.
	DecisionInjected Decision = "injected"
	// DecisionResidue: the unframed bytes at a stream's end (class
	// frame.KindResidue), kept for the raw log.
	DecisionResidue Decision = "residue"
)

// FrameRecord is one decoded frame plus its verdict: the raw-byte audit
// row. Payload is stored verbatim; Decoded carries the class-specific
// fields as JSON.
type FrameRecord struct {
	ConnID   string
	Seq      int
	Dir      capture.Dir
	Off      int64
	TS       time.Time
	Class    frame.Kind
	Dev      [4]byte
	Fn       byte
	Plen     int
	Payload  []byte // verbatim
	Decision Decision
	Reason   string
	Origin   string
	Decoded  json.RawMessage
}

// writeItem is one queued write. The flusher applies a batch of items in
// enqueue order inside a single transaction.
type writeItem any

// connItem persists one connections row.
type connItem struct {
	info    capture.CloseInfo
	evicted int
}

// chunkItem persists one chunks row (seq assigned by RecordChunk).
type chunkItem struct {
	connID string
	seq    int
	c      capture.Chunk
}

// frameItem persists one frames row plus the telemetry projection for
// tele-class frames.
type frameItem struct {
	r FrameRecord
	// noTele: a tele frame superseded by the register poll — logged as
	// a frame, not projected into a telemetry row (PollFresh)
	noTele bool
}

// auditItem persists one audit_events row (close causes, watermarks,
// serve decisions, TLS results — the conn-less audit trail).
type auditItem struct {
	ts     time.Time
	kind   string
	detail string
}

// teleItem persists one telemetry row with no frame behind it (the
// live-value poller).
type teleItem struct {
	connID string
	ts     time.Time
	tele   frame.Tele
}

// socEstItem sets the SoC estimate (0.01 %) of a stored telemetry row
// (the row is queued before it, on the same ordered queue).
type socEstItem struct {
	connID string
	ts     time.Time
	centi  int
}

// RecordSocEstimate enqueues the SoC estimate, in 0.01 %, computed for
// the telemetry sample of connID at ts (D-028).
func (s *Store) RecordSocEstimate(connID string, ts time.Time, centiPct int) {
	s.checkOpen("RecordSocEstimate")
	s.enqueue(socEstItem{connID: connID, ts: ts, centi: centiPct})
}

// regItem persists one register block read (one registers row per
// address).
type regItem struct {
	ts   time.Time
	addr int
	vals []uint16
}

// RecordRegisters enqueues one holding-register block read starting at
// addr: each address's latest value (the settings poll).
//
// A read that changes a known value (or learns a new one) is reported to
// the OnRegisterReadChange hook: a setting changed outside this app (the
// inverter's own panel) reaches the open web UIs.
func (s *Store) RecordRegisters(ts time.Time, addr int, vals []uint16) {
	s.checkOpen("RecordRegisters")
	changed := s.recordRegs(ts, addr, vals)
	if hook := s.readChange.Load(); hook != nil && len(changed) > 0 {
		(*hook)(changed)
	}
}

// recordRegs updates the view and persists a register block; it returns
// the changed addresses.
func (s *Store) recordRegs(ts time.Time, addr int, vals []uint16) []int {
	changed := s.view.setRegs(ts, addr, vals)
	s.enqueue(regItem{ts: ts, addr: addr, vals: append([]uint16(nil), vals...)})
	return changed
}

// pollPrecedence is how long a register-poll sample outranks the 150 B
// tele frame: the poll is measured and carries the EPS port; the frame
// is the cloud's model without it (the cloud's serial query switches it
// on for five minutes — interleaved, the two made the live values flip).
const pollPrecedence = 15 * time.Second

// PollFresh reports whether the register poll supplied telemetry within
// pollPrecedence of at (a 150 B tele frame then only gets logged).
func (s *Store) PollFresh(at time.Time) bool {
	last := s.lastPoll.Load()
	return last != 0 && at.Sub(time.Unix(0, last)) < pollPrecedence
}

// OnRegisterReadChange installs the hook RecordRegisters calls with the
// addresses a read changed (on the caller's goroutine: it must be cheap).
// Writes recorded from frames do not call it — their own config event
// announces them.
func (s *Store) OnRegisterReadChange(f func(addrs []int)) {
	s.readChange.Store(&f)
}

// CellTempMaxAge bounds how long a snap61's cell temperatures (pushed
// about every 5 minutes) stand for the live samples after it.
const CellTempMaxAge = 15 * time.Minute

// RecordTelemetry enqueues one telemetry row from a non-frame source —
// the live-value poller's decoded Modbus reads (the same row shape a
// 150 B tele frame projects to).
func (s *Store) RecordTelemetry(connID string, ts time.Time, tele frame.Tele) {
	s.checkOpen("RecordTelemetry")
	if tele.Temps != nil { // the snapshot's cell temperatures, while fresh
		t := *tele.Temps
		t.CellMinDeciC, t.CellMaxDeciC = s.view.cellTemps(ts, CellTempMaxAge)
		tele.Temps = &t
	}
	s.lastPoll.Store(ts.UnixNano())
	s.view.setTele(ts, tele)
	s.notePresent(tele)
	s.enqueue(teleItem{connID: connID, ts: ts, tele: tele})
}

// RecordConn enqueues the connections row for a finished capture stream.
// It is an upsert keyed on ID, so a re-recorded stream overwrites the
// row.
func (s *Store) RecordConn(info capture.CloseInfo, evicted int) {
	s.checkOpen("RecordConn")
	s.enqueue(connItem{info: info, evicted: evicted})
}

// RecordChunk enqueues one chunk row. seq is the chunk's position in its
// stream's retained record: the store counts per connection (0-based),
// and a connection is persisted exactly once by persistStream.
func (s *Store) RecordChunk(c capture.Chunk, connID string) {
	s.checkOpen("RecordChunk")
	s.mu.Lock()
	seq := s.chunkSeq[connID]
	s.chunkSeq[connID] = seq + 1
	s.mu.Unlock()
	s.enqueue(chunkItem{connID: connID, seq: seq, c: c})
}

// RecordFrame enqueues the frame row (and, for tele-class frames, the
// derived telemetry row). The queue is bounded: a full queue blocks the
// caller (backpressure — see the package docs).
//
// A Modbus write that reached the inverter (forwarded from the cloud, or
// injected by us) also records the registers it set, so the settings the
// app reports follow every write without a frame-log scan.
func (s *Store) RecordFrame(r FrameRecord) {
	s.checkOpen("RecordFrame")
	noTele := r.Class == classTele && s.PollFresh(r.TS)
	addr, vals, wrote := s.view.observeFrame(&r, noTele)
	s.enqueue(frameItem{r: r, noTele: noTele})
	if wrote {
		s.recordRegs(r.TS, addr, vals)
	}
}

// RecordAudit enqueues one audit_events row. The queue is bounded: a full
// queue blocks the caller (backpressure — see the package docs).
func (s *Store) RecordAudit(ts time.Time, kind, detail string) {
	s.checkOpen("RecordAudit")
	s.enqueue(auditItem{ts: ts, kind: kind, detail: detail})
}

// Flush drains the write queue synchronously: it returns once every item
// enqueued before the call has been applied. Used by tests and by Close.
// The handshake with the flusher is the enq/applied atomics plus the
// store-owned poke channel — never a caller-created channel: the flusher
// outlives the caller's context (a synctest bubble) and must not touch
// its channels (crossing them is a fatal error).
func (s *Store) Flush() {
	s.checkOpen("Flush")
	want := s.enq.Load()
	select {
	case s.poke <- struct{}{}:
	default:
	}
	for s.applied.Load() < want {
		runtime.Gosched()
	}
}

// enqueue appends one item to the write queue (the queue is bounded: a
// full queue blocks the caller — backpressure, see the package docs).
func (s *Store) enqueue(it writeItem) {
	s.batch <- it
	s.enq.Add(1)
}

// flusher is the single writer. It applies batches in enqueue order,
// flushing on the batch-size threshold, the tick channel, a Flush
// request, or stop.
func (s *Store) flusher() {
	defer close(s.done)
	var batch []writeItem
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := s.apply(batch); err != nil {
			s.setLastErr(err)
		}
		s.applied.Add(int64(len(batch)))
		batch = batch[:0]
	}
	for {
		select {
		case it := <-s.batch:
			batch = append(batch, it)
			if _, urgent := it.(auditItem); urgent || len(batch) >= maxPending {
				batch = s.drain(batch)
				flush()
			}
		case ch := <-s.flushReq:
			batch = s.drain(batch)
			flush()
			close(ch)
		case <-s.poke:
			// A Flush prompt: write everything queued so far.
			batch = s.drain(batch)
			flush()
		case <-s.tick:
			// A tick means "write everything queued so far": drain as
			// well, because select may take the tick before queued items
			// reach the in-flight batch.
			batch = s.drain(batch)
			flush()
		case <-s.stop:
			batch = s.drain(batch)
			flush()
			return
		}
	}
}

// drain pulls every currently queued item into batch (used by Flush so
// it covers everything enqueued before the call).
func (s *Store) drain(batch []writeItem) []writeItem {
	for {
		select {
		case it := <-s.batch:
			batch = append(batch, it)
		default:
			return batch
		}
	}
}

// apply writes one batch of items in a single transaction.
func (s *Store) apply(items []writeItem) error {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return fmt.Errorf("store: begin batch: %w", err)
	}
	for _, it := range items {
		var err error
		switch v := it.(type) {
		case connItem:
			err = s.applyConn(tx, v)
		case chunkItem:
			err = applyChunk(tx, v)
		case frameItem:
			err = applyFrame(tx, v)
		case auditItem:
			err = applyAudit(tx, v)
		case teleItem:
			err = applyTele(tx, v.connID, v.ts, v.tele)
		case regItem:
			err = applyRegs(tx, v)
		case socEstItem:
			_, err = tx.Exec(`UPDATE telemetry SET soc_est = ? WHERE conn_id = ? AND ts = ?`,
				v.centi, v.connID, encTS(v.ts))
		default:
			err = fmt.Errorf("store: unknown item %T", it)
		}
		if err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit batch: %w", err)
	}
	return nil
}

// applyConn upserts one connections row, stamped with the version of
// the binary that opened the store (release traceability, D-018).
func (s *Store) applyConn(tx *sql.Tx, it connItem) error {
	host, tlsVersion, result := "", 0, ""
	if it.info.TLS != nil {
		host, tlsVersion, result = it.info.TLS.Host, int(it.info.TLS.Version), it.info.TLS.Result
	}
	_, err := tx.Exec(`
		INSERT OR REPLACE INTO connections
			(id, kind, sni, client, dst, opened, closed, cause,
			 tls_host, tls_version, tls_result, evicted_chunks, version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		it.info.ID, it.info.Kind, it.info.SNI, it.info.Client, it.info.Dst,
		encTS(it.info.Opened), encTS(it.info.Closed), it.info.Cause,
		host, tlsVersion, result, it.evicted, s.version)
	return err
}

// applyChunk inserts one chunks row; the payload column is NULL unless
// the stream was opened with byte capture enabled (G2).
func applyChunk(tx *sql.Tx, it chunkItem) error {
	_, err := tx.Exec(`
		INSERT INTO chunks
			(conn_id, seq, dir, ts, len, decision, reason, origin, payload)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		it.connID, it.seq, it.c.Dir.String(), encTS(it.c.TS), it.c.Len,
		it.c.Decision.String(), it.c.Reason, it.c.Origin, it.c.Payload)
	return err
}

// applyFrame inserts one frames row; a tele-class frame additionally
// projects a telemetry row (a malformed tele payload keeps the audit row
// and skips only the projection).
func applyFrame(tx *sql.Tx, it frameItem) error {
	r := it.r
	_, err := tx.Exec(`
		INSERT INTO frames
			(conn_id, seq, dir, off, ts, class, dev, fn, plen, payload,
			 decision, reason, origin, decoded)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ConnID, r.Seq, r.Dir.String(), r.Off, encTS(r.TS), r.Class,
		hex.EncodeToString(r.Dev[:]), int(r.Fn), r.Plen, r.Payload,
		r.Decision, r.Reason, r.Origin, string(r.Decoded))
	if err != nil {
		return err
	}
	if r.Class != classTele || it.noTele {
		return nil
	}
	tele, err := frame.ParseTele(r.Payload)
	if err != nil {
		return nil //nolint:nilerr // the frame row is stored; a malformed tele payload just adds no telemetry row
	}
	return applyTele(tx, r.ConnID, r.TS, tele)
}

// applyAudit inserts one audit_events row.
func applyAudit(tx *sql.Tx, it auditItem) error {
	_, err := tx.Exec(`
		INSERT INTO audit_events (ts, kind, detail)
		VALUES (?, ?, ?)`, encTS(it.ts), it.kind, it.detail)
	return err
}

// applyTele inserts one telemetry row projected from a tele frame.
// applyRegs upserts one row per address of a register block read.
func applyRegs(tx *sql.Tx, it regItem) error {
	for i, v := range it.vals {
		if _, err := tx.Exec(`
			INSERT INTO registers(addr, value, ts) VALUES (?, ?, ?)
			ON CONFLICT(addr) DO UPDATE SET value = excluded.value, ts = excluded.ts`,
			it.addr+i, int(v), encTS(it.ts)); err != nil {
			return err
		}
	}
	return nil
}

func applyTele(tx *sql.Tx, connID string, ts time.Time, tele frame.Tele) error {
	var tb, ti, ta, bc, cmin, cmax, m2 any // NULL without temperatures / meter 2 (the 150 B frame)
	if tele.Meter2 != nil {
		m2 = *tele.Meter2
	}
	if t := tele.Temps; t != nil {
		tb, ti, ta, bc = t.BattDeciC, t.InvDeciC, t.AmbDeciC, t.BMSChargeDeciA
		if t.CellMinDeciC != nil {
			cmin, cmax = *t.CellMinDeciC, *t.CellMaxDeciC
		}
	}
	_, err := tx.Exec(`
		INSERT INTO telemetry
			(conn_id, ts, sol, grid, haus, acport, batt, soc, charge_state,
			 idnt_grid, idnt_ac, haus_dup, eps, t_batt, t_inv, t_amb, bms_chg,
			 t_cell_min, t_cell_max, meter2)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		connID, encTS(ts), tele.Sol, tele.Grid, tele.Haus, tele.Acport,
		tele.Batt, int(tele.Soc), int(tele.ChargeState), tele.IDGrid,
		tele.IDAcport, tele.HausDup, tele.Eps, tb, ti, ta, bc, cmin, cmax, m2)
	return err
}

// SetWriteDelay sets how long records may wait in memory before they are
// written (at least flushInterval; applies from the next tick).
func (s *Store) SetWriteDelay(d time.Duration) {
	d = max(d, flushInterval)
	s.writeDelay.Store(int64(d))
	if s.ticker != nil {
		s.ticker.Reset(d)
	}
}

// WriteDelay is the current write delay.
func (s *Store) WriteDelay() time.Duration {
	if d := time.Duration(s.writeDelay.Load()); d > 0 {
		return d
	}
	return flushInterval
}

// catchUp writes what waits before a read of recent data, when a write
// delay holds records back (with the default delay the tick is soon
// enough, and a read never forces a write).
func (s *Store) catchUp() {
	if s.WriteDelay() > flushInterval && s.applied.Load() < s.enq.Load() {
		s.Flush()
	}
}
