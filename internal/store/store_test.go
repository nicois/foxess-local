package store

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/nicois/prox/pkg/capture"

	"foxesslocal/internal/frame"
)

// testBase is the deterministic origin of every synthetic timestamp.
var testBase = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

const (
	teleLenTest   = 150
	snapLenTest   = 626
	tickFrames    = 3
	waitSpinLimit = 2_000_000
)

// Shared fixture literals (goconst: repeated across the test files).
const (
	testKindTLS      = "tls"
	testSNI          = "47.94.1.2"
	testSNIAlt       = "1.2.3.4"
	testClient       = "192.168.4.10:55123"
	testDst          = "47.94.1.2:14431"
	testDstAlt       = "1.2.3.4:14431"
	testConnID       = "c1"
	testStreamID     = "conn-1"
	testCauseEOF     = "eof"
	testDropReason   = "c001 default-deny"
	testGatewayOrig  = "gateway"
	testDecision     = "passed"
	testConnOld      = "old"
	testConnRecent   = "recent"
	testConnOpen     = "open"
	testVersion      = "vtest-prox-abc1234"
	tableConnections = "connections"
	tableChunks      = "chunks"
	tableFrames      = "frames"
	tableTelemetry   = "telemetry"
	tableCommands    = "commands"
	tableAuditEvents = "audit_events"
)

// newTestStore opens a store in a temp dir with a fixed clock and no tick
// channel: flushes are driven by Flush() or the manual tick channel of
// TestBatchWriter, never by the wall clock.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := openStore(filepath.Join(t.TempDir(), "store.db"), nil, maxConns, fixedClock(testBase), testVersion)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

// fixedClock returns a clock pinned to ts (tests; C-004).
func fixedClock(ts time.Time) func() time.Time {
	return func() time.Time { return ts }
}

// waitFor busy-waits (Gosched, no wall-clock) until f() is true. The bound
// makes a broken flusher fail the test instead of hanging it.
func waitFor(t *testing.T, f func() bool) {
	t.Helper()
	for range waitSpinLimit {
		if f() {
			return
		}
		runtime.Gosched()
	}
	t.Fatal("waitFor: condition not met")
}

func countRows(t *testing.T, s *Store, table string) int {
	t.Helper()
	// Query (not QueryRow): the row must be closed explicitly, or the
	// single shared connection stays pinned until GC and starves the
	// flusher.
	//nolint:gosec // G202: table is a named constant from the test fixtures.
	rows, err := s.db.Query("SELECT COUNT(*) FROM " + table)
	if err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	if !rows.Next() {
		t.Fatalf("count %s: no row", table)
	}
	var n int
	if err := rows.Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	if err := closeRows(rows, rows.Err()); err != nil {
		t.Fatalf("count %s close: %v", table, err)
	}
	return n
}

// telePayload builds a 150 B tele payload at the profile §4 offsets.
func telePayload(sol, grid, haus, acport, batt int32, soc, chargeState byte) []byte {
	const (
		solOff     = 35
		gridOff    = 39
		hausOff    = 43
		acOff      = 59
		hausDupOff = 65
		battOff    = 67
		csOff      = 72
		socOff     = 79
	)
	p := make([]byte, teleLenTest)
	//nolint:gosec // G115: test values are small protocol integers.
	binary.BigEndian.PutUint32(p[solOff:], uint32(sol))
	//nolint:gosec // G115: test values are small protocol integers.
	binary.BigEndian.PutUint32(p[gridOff:], uint32(grid))
	//nolint:gosec // G115: test values are small protocol integers.
	binary.BigEndian.PutUint32(p[hausOff:], uint32(haus))
	//nolint:gosec // G115: test values are small protocol integers.
	binary.BigEndian.PutUint32(p[acOff:], uint32(acport))
	//nolint:gosec // G115: test values are small protocol integers.
	binary.BigEndian.PutUint16(p[hausDupOff:], uint16(haus))
	//nolint:gosec // G115: test values are small protocol integers.
	binary.BigEndian.PutUint32(p[battOff:], uint32(batt))
	p[csOff] = chargeState
	p[socOff] = soc
	return p
}

// frameRecord builds the standard test frame record.
func frameRecord(connID string, seq int, dir capture.Dir, class frame.Kind, payload []byte) FrameRecord {
	return FrameRecord{
		ConnID:   connID,
		Seq:      seq,
		Dir:      dir,
		TS:       testBase,
		Class:    class,
		Dev:      [4]byte{0x06, 0, 0, 0},
		Fn:       0x10,
		Plen:     len(payload),
		Payload:  payload,
		Decision: testDecision,
	}
}

// checkSchemaObjects asserts the v1 tables and declared indexes exist.
func checkSchemaObjects(t *testing.T, s *Store) {
	t.Helper()
	var (
		tables  = map[string]bool{}
		indexes = map[string]bool{}
	)
	rows, err := s.db.Query("SELECT type, name FROM sqlite_master")
	if err != nil {
		t.Fatalf("sqlite_master: %v", err)
	}
	var typ, name string
	for rows.Next() {
		if err := rows.Scan(&typ, &name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		switch typ {
		case "table":
			tables[name] = true
		case "index":
			indexes[name] = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	for _, want := range []string{
		tableConnections, tableChunks, tableFrames, tableTelemetry,
		tableCommands, tableAuditEvents,
	} {
		if !tables[want] {
			t.Errorf("missing table %s", want)
		}
	}
	for _, want := range []string{
		"idx_frames_dir_dec_ts", "idx_frames_class_ts", "idx_telemetry_ts", "idx_frames_conn_seq",
		"idx_telemetry_conn_ts", "idx_audit_ts",
	} {
		if !indexes[want] {
			t.Errorf("missing index %s", want)
		}
	}
}

// assertRowCounts checks each table's row count.
func assertRowCounts(t *testing.T, s *Store, want map[string]int) {
	t.Helper()
	for table, n := range want {
		if got := countRows(t, s, table); got != n {
			t.Errorf("%s rows: got %d, want %d", table, got, n)
		}
	}
}

// TestOpenCreatesSchema verifies the v1 migration: every table exists, the
// declared indexes exist, and a sanity row lands in each table.
func TestOpenCreatesSchema(t *testing.T) {
	s := newTestStore(t)
	checkSchemaObjects(t, s)

	// Sanity row into each table: connections/chunks/frames/telemetry via
	// the API (telemetry is projected from the tele frame), the rest via
	// SQL (their writer APIs land with tasks 5/6).
	s.RecordConn(capture.CloseInfo{
		ID: testConnID, Kind: testKindTLS, SNI: testSNIAlt,
		Client: testClient, Dst: testDstAlt,
		Opened: testBase, Closed: testBase.Add(time.Second), Cause: testCauseEOF,
	}, 0)
	s.RecordChunk(capture.Chunk{
		Dir: capture.DirC2S, TS: testBase, Len: 135, Decision: capture.Forwarded,
	}, testConnID)
	s.RecordFrame(frameRecord(testConnID, 1, capture.DirC2S, classTele,
		telePayload(1000, -500, 400, 600, -100, 57, 2)))
	s.Flush()
	assertRowCounts(t, s, map[string]int{
		tableConnections: 1, tableChunks: 1, tableFrames: 1, tableTelemetry: 1,
	})
	if _, err := s.db.Exec("INSERT INTO commands (id, ts, kind, params, status, frame_seq, ack, error) VALUES ('k1', ?, 'workmode', '{}', 'pending', 0, 0, '')",
		encTS(testBase)); err != nil {
		t.Fatalf("commands insert: %v", err)
	}
	if _, err := s.db.Exec("INSERT INTO audit_events (ts, kind, detail) VALUES (?, 'conn-closed', 'eof')",
		encTS(testBase)); err != nil {
		t.Fatalf("audit insert: %v", err)
	}
	assertRowCounts(t, s, map[string]int{
		tableCommands: 1, tableAuditEvents: 1,
	})
}

// TestTimestampEncoding pins the fixed-width UTC encoding: the stored text
// is the tsLayout form (fraction always present) and parses back to the
// same instant, for integer, sub-second, and non-UTC-zone timestamps.
func TestTimestampEncoding(t *testing.T) {
	const offsetSec = 18000 // a +05:00 zone, to prove UTC normalization
	cases := []time.Time{
		time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC),
		time.Date(2026, 11, 8, 12, 0, 0, 123456789, time.UTC),
		time.Date(2026, 11, 8, 13, 30, 5, 500000000, time.UTC).
			Add(time.Hour).In(time.FixedZone("test", offsetSec)),
	}
	for _, tc := range cases {
		got := encTS(tc)
		want := tc.UTC().Format(tsLayout)
		if got != want {
			t.Errorf("encTS(%v): got %q, want %q", tc, got, want)
		}
		back, err := decTS(got)
		if err != nil {
			t.Fatalf("decTS(%q): %v", got, err)
		}
		if !back.Equal(tc) {
			t.Errorf("decTS(%q): got %v, want %v", got, back, tc)
		}
	}
	if got := encTS(time.Time{}); got != "" {
		t.Errorf("encTS(zero): got %q, want empty", got)
	}
	if got, _ := decTS(""); !got.IsZero() {
		t.Errorf("decTS(\"\"): got %v, want zero", got)
	}
}

// TestFrameRecordRoundTrip pins the raw-byte audit: a RecordFrame+Flush
// lands a frames row with the payload byte-exact and every field intact.
func TestFrameRecordRoundTrip(t *testing.T) {
	s := newTestStore(t)
	payload := telePayload(1000, -500, 400, 600, -100, 57, 2)
	rec := frameRecord(testConnID, 7, capture.DirS2C, classTele, payload)
	rec.Off = 1234
	rec.Decoded = json.RawMessage(`{"sol":1000,"soc":57}`)
	s.RecordFrame(rec)
	s.Flush()

	got, err := s.AuditFrames(testDecision, capture.DirS2C)
	if err != nil {
		t.Fatalf("AuditFrames: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("frames: got %d, want 1", len(got))
	}
	g := got[0]
	if g.ConnID != testConnID || g.Seq != 7 || g.Dir != capture.DirS2C || g.Off != 1234 {
		t.Errorf("identity: got %+v", g)
	}
	if !g.TS.Equal(testBase) {
		t.Errorf("ts: got %v, want %v", g.TS, testBase)
	}
	if g.Class != classTele || g.Dev != [4]byte{0x06, 0, 0, 0} ||
		g.Fn != 0x10 || g.Plen != len(payload) {
		t.Errorf("class fields: got %+v", g)
	}
	if !bytes.Equal(g.Payload, payload) {
		t.Errorf("payload not byte-exact: got %x", g.Payload)
	}
	if !bytes.Equal(g.Decoded, rec.Decoded) {
		t.Errorf("decoded: got %s, want %s", g.Decoded, rec.Decoded)
	}
}

// checkAudit asserts AuditFrames(decision, dir) returns exactly one record
// matching want (seq, payload byte-exact, reason, origin).
func checkAudit(t *testing.T, s *Store, decision Decision, dir capture.Dir, want FrameRecord) {
	t.Helper()
	got, err := s.AuditFrames(decision, dir)
	if err != nil {
		t.Fatalf("AuditFrames(%s, %s): %v", decision, dir, err)
	}
	if len(got) != 1 {
		t.Fatalf("AuditFrames(%s, %s): got %d records, want 1", decision, dir, len(got))
	}
	g := got[0]
	if g.Seq != want.Seq || g.Reason != want.Reason || g.Origin != want.Origin {
		t.Errorf("AuditFrames(%s): got %+v", decision, g)
	}
	if !bytes.Equal(g.Payload, want.Payload) {
		t.Errorf("AuditFrames(%s): payload not byte-exact", decision)
	}
}

// TestAuditFramesC001 pins the C-001 audit query: AuditFrames returns
// exactly the frames with the requested verdict in the requested
// direction, payloads byte-exact.
func TestAuditFramesC001(t *testing.T) {
	s := newTestStore(t)
	a := telePayload(1, 2, 3, 4, 5, 6, 2)
	b := telePayload(7, 8, 9, 10, 11, 12, 2)
	c := telePayload(13, 14, 15, 16, 17, 18, 2)
	d := telePayload(19, 20, 21, 22, 23, 24, 2)
	f1 := frameRecord(testConnID, 1, capture.DirS2C, classTele, a)
	f2 := frameRecord(testConnID, 2, capture.DirS2C, classTele, b)
	f2.Decision = decisionDropped
	f2.Reason = testDropReason
	f3 := frameRecord(testConnID, 3, capture.DirC2S, classTele, c)
	f4 := frameRecord(testConnID, 4, capture.DirS2C, classTele, d)
	f4.Decision = "injected"
	f4.Origin = testGatewayOrig
	for _, r := range []FrameRecord{f1, f2, f3, f4} {
		s.RecordFrame(r)
	}
	s.Flush()

	checkAudit(t, s, testDecision, capture.DirS2C, f1)
	checkAudit(t, s, decisionDropped, capture.DirS2C, f2)
	checkAudit(t, s, "injected", capture.DirS2C, f4)
	// The c2s "passed" frame must never leak into an s2c audit.
	if n := countRows(t, s, tableFrames); n != 4 {
		t.Errorf("%s rows: got %d, want 4", tableFrames, n)
	}
}

// TestBatchWriter drives the batch-size path (no Flush, no tick): the
// 128th enqueued record flushes on its own. The tick path and the
// Close-flush path are covered in the same test.
func TestBatchWriter(t *testing.T) {
	tick := make(chan time.Time) // never fires: only the size path may flush
	path := filepath.Join(t.TempDir(), "store.db")
	s, err := openStore(path, tick, maxConns, fixedClock(testBase), testVersion)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}

	payload := telePayload(1, 1, 1, 1, 1, 1, 2)
	const (
		countPathN = maxPending // the memory cap (the write delay holds the rest)
		totalN     = countPathN + tickFrames + 1
	)
	for i := range countPathN {
		s.RecordFrame(frameRecord(testConnID, i, capture.DirC2S, classTele, payload))
	}
	// The maxPending-th record must trigger a flush without Flush() or a tick.
	waitFor(t, func() bool { return countRows(t, s, tableFrames) == countPathN })

	// Tick path: a few records below the batch size, then one tick.
	for i := range tickFrames {
		s.RecordFrame(frameRecord(testConnID, countPathN+i, capture.DirC2S, classTele, payload))
	}
	tick <- time.Time{}
	waitFor(t, func() bool {
		return countRows(t, s, tableFrames) == countPathN+tickFrames
	})

	// Close path: one pending record must be flushed by Close; verify by
	// reopening the same file.
	s.RecordFrame(frameRecord(testConnID, countPathN+tickFrames, capture.DirC2S, classTele, payload))
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s2, err := openStore(path, nil, maxConns, fixedClock(testBase), testVersion)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	if n := countRows(t, s2, tableFrames); n != totalN {
		t.Errorf("frames after Close: got %d, want %d", n, totalN)
	}
}

// TestMigrationAddsVersionColumn: a pre-v3 database (schema 1+2,
// connections without the version column) upgrades in place — old rows
// read 'unknown', new rows carry the opening binary's version, and the
// migration ledger records 3 (release traceability, D-018).
func TestMigrationAddsVersionColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	db, err := sql.Open(driver, "file:"+path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	for _, stmt := range []string{schemaV1, schemaV2} {
		if _, err := db.Exec(stmt); err != nil {
			_ = db.Close()
			t.Fatalf("seed schema: %v", err)
		}
	}
	if _, err := db.Exec("CREATE TABLE schema_migrations(version INTEGER NOT NULL)"); err != nil {
		_ = db.Close()
		t.Fatalf("seed migrations table: %v", err)
	}
	if _, err := db.Exec("INSERT INTO schema_migrations(version) VALUES (1), (2)"); err != nil {
		_ = db.Close()
		t.Fatalf("seed migrations: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO connections
			(id, kind, sni, client, dst, opened, closed, cause, tls_host, tls_version, tls_result, evicted_chunks)
		VALUES ('old', 'unknown', '', '', '', '2026-01-01T00:00:00Z', '2026-01-01T00:10:00Z', 'eof', '', 0, '', 0)`); err != nil {
		_ = db.Close()
		t.Fatalf("seed old row: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seed db: %v", err)
	}

	s, err := openStore(path, nil, maxConns, fixedClock(testBase), testVersion)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	var v string
	if err := s.db.QueryRow(`SELECT version FROM connections WHERE id = 'old'`).Scan(&v); err != nil {
		t.Fatalf("old row version: %v", err)
	}
	if v != "unknown" {
		t.Errorf("old row version: got %q, want %q", v, "unknown")
	}

	s.RecordConn(capture.CloseInfo{
		ID: "new", Kind: testKindTLS, Opened: testBase,
		Closed: testBase.Add(time.Second), Cause: testCauseEOF,
	}, 0)
	s.Flush()
	if err := s.db.QueryRow(`SELECT version FROM connections WHERE id = 'new'`).Scan(&v); err != nil {
		t.Fatalf("new row version: %v", err)
	}
	if v != testVersion {
		t.Errorf("new row version: got %q, want %q", v, testVersion)
	}

	var n int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	if n != 11 { // v11: the notification outbox
		t.Errorf("schema_migrations max: got %d, want 11", n)
	}
}
