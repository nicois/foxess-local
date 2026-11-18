package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/nicois/prox/pkg/capture"
)

var maintainCfg = MaintainConfig{RawHours: 72, BucketDays: 365}

// seedOldFrameChunk seeds one chunk row and one frame row for connID at ts.
func seedOldFrameChunk(t *testing.T, s *Store, connID string, ts time.Time) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO chunks(conn_id, seq, dir, ts, len, decision, reason, origin) VALUES (?,?,?,?,?,?,?,?)`,
		connID, 1, "c2s", encTS(ts), 10, "forward", "", "capture"); err != nil {
		t.Fatalf("chunk: %v", err)
	}
	// frames has an `off` column (not `len`); 14 values for 14 placeholders.
	if _, err := s.db.Exec(`INSERT INTO frames(conn_id, seq, dir, off, ts, class, dev, fn, plen, payload, decision, reason, origin, decoded) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		connID, 1, "c2s", 0, encTS(ts), "", 0, 0, 0, []byte{1}, "forward", "", "capture", nil); err != nil {
		t.Fatalf("frame: %v", err)
	}
}

// seedBucketRow inserts a synthetic rollup row directly (guard tests).
func seedBucketRow(t *testing.T, s *Store, ts time.Time) {
	t.Helper()
	if _, err := s.db.Exec(upsertSQL, upsertArgs(ts.Unix(), Agg{})...); err != nil {
		t.Fatalf("seedBucketRow: %v", err)
	}
}

// seedTeleRowFor inserts one raw telemetry row for the given connection.
func seedTeleRowFor(t *testing.T, s *Store, connID string, ts time.Time) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO telemetry(conn_id, ts, sol, grid, haus, acport, batt, soc, charge_state, idnt_grid, idnt_ac, haus_dup)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		connID, encTS(ts), 100, 0, 0, 0, 0, 50, 2, 0, 0, 0); err != nil {
		t.Fatalf("seedTeleRowFor: %v", err)
	}
}

// seedFinished seeds one finished connection (closed at closedAt) with one
// chunk (payload may be nil) at rowTS, one tele-class frame at the test
// clock, and — with withTele — one raw telemetry row at rowTS.
func seedFinished(t *testing.T, s *Store, id string, opened, closedAt, rowTS time.Time, withTele bool, payload []byte) {
	t.Helper()
	s.RecordConn(capture.CloseInfo{
		ID: id, Kind: testKindTLS, Opened: opened, Closed: closedAt, Cause: testCauseEOF,
	}, 0)
	s.RecordChunk(capture.Chunk{
		Dir: capture.DirC2S, TS: rowTS, Len: 10, Decision: capture.Forwarded, Payload: payload,
	}, id)
	s.RecordFrame(frameRecord(id, 1, capture.DirC2S, classTele, telePayload(1, 2, 3, 4, 5, 6, 2)))
	if withTele {
		seedTeleRowFor(t, s, id, rowTS)
	}
	s.Flush() // Record* enqueues; make the seed visible to Maintain
}

// Retention is measured from when the stream stopped being written to
// (connection close), not from the row's ts: a keep-alive session can run
// for days, and the rows of a still-open session survive the raw window
// no matter how old they are.
func TestMaintainLongLivedSessionSurvives(t *testing.T) {
	s := newTestStore(t) // now = testBase
	opened := testBase.Add(-10 * 24 * time.Hour)
	s.RecordConn(capture.CloseInfo{ID: "live", Kind: testKindTLS, Opened: opened}, 0)
	seedTeleRowFor(t, s, "live", opened) // a 10-day-old row on the live session
	s.RecordChunk(capture.Chunk{
		Dir: capture.DirC2S, TS: opened, Len: 10, Decision: capture.Forwarded, Payload: []byte{9},
	}, "live")
	// Record* enqueues; make the seed visible to Maintain.
	s.Flush()
	seedBucketRow(t, s, testBase.Add(-15*time.Minute)) // a current watermark exists

	rep, err := s.Maintain(MaintainConfig{RawHours: 72, BucketDays: 365, ChunkBytesHours: 24})
	if err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	if rep.DeletedConns != 0 || rep.DeletedRaw != 0 || rep.DeletedFrames != 0 ||
		rep.DeletedChunks != 0 || rep.NulledBytes != 0 {
		t.Fatalf("live session reaped: %+v (want all zero)", rep)
	}
	var p []byte
	if err := s.db.QueryRow(`SELECT payload FROM chunks WHERE conn_id = 'live'`).Scan(&p); err != nil {
		t.Fatal(err)
	}
	if len(p) != 1 {
		t.Fatalf("live-session chunk bytes nulled: %v (the byte horizon counts from close)", p)
	}
}

// A finished session is reaped as a whole once its close time is past the
// raw window — every row goes, however old; a session that closed recently
// keeps every row, however old.
func TestMaintainReapsFinishedByClose(t *testing.T) {
	s := newTestStore(t)
	seedBucketRow(t, s, testBase) // a current watermark
	seedFinished(t, s, "old", testBase.Add(-9*24*time.Hour), testBase.Add(-80*time.Hour), testBase.Add(-9*24*time.Hour), true, []byte{1})
	seedFinished(t, s, "recent", testBase.Add(-3*24*time.Hour), testBase.Add(-time.Hour), testBase.Add(-3*24*time.Hour), true, []byte{2})

	rep, err := s.Maintain(maintainCfg)
	if err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	if rep.DeletedConns != 1 {
		t.Fatalf("DeletedConns = %d, want 1 (only 'old')", rep.DeletedConns)
	}
	if rep.DeletedFrames != 1 || rep.DeletedChunks != 1 {
		t.Fatalf("frames %d chunks %d, want 1/1", rep.DeletedFrames, rep.DeletedChunks)
	}
	if n := countRows(t, s, tableFrames); n != 1 {
		t.Fatalf("recent frame rows = %d, want 1 (close is recent: all rows survive)", n)
	}
	if n := countRows(t, s, tableTelemetry); n != 2 {
		t.Fatalf("recent telemetry rows = %d, want 2", n)
	}
}

// Watermark clamp: finished work is only reaped after the rollup has fully
// covered it — an un-rolled connection survives however old its close is.
func TestMaintainReapWatermarkGuard(t *testing.T) {
	s := newTestStore(t)
	seedBucketRow(t, s, testBase.Add(-100*time.Hour)) // a stale watermark
	// Frame + chunk only (no telemetry): the rollup cannot advance the
	// watermark past the seeded row.
	seedFinished(t, s, "covered", testBase.Add(-111*time.Hour), testBase.Add(-110*time.Hour), testBase.Add(-111*time.Hour), false, nil)
	seedFinished(t, s, "uncovered", testBase.Add(-96*time.Hour), testBase.Add(-95*time.Hour), testBase.Add(-96*time.Hour), false, nil)

	rep, err := s.Maintain(maintainCfg)
	if err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	// cutoff = min(72 h ago, 99.75 h ago) = 99.75 h ago → only 'covered' goes.
	if rep.DeletedConns != 1 {
		t.Fatalf("DeletedConns = %d, want 1 (the un-rolled 'uncovered' must survive)", rep.DeletedConns)
	}
	if rep.DeletedFrames != 1 || rep.DeletedChunks != 1 {
		t.Fatalf("frames %d chunks %d, want 1/1", rep.DeletedFrames, rep.DeletedChunks)
	}
}

// No buckets exist (watermark 0): nothing is reaped, ever — un-rolled data
// is never deleted.
func TestMaintainReapNoWatermark(t *testing.T) {
	s := newTestStore(t)
	seedFinished(t, s, "old", testBase.Add(-101*time.Hour), testBase.Add(-100*time.Hour), testBase.Add(-101*time.Hour), false, nil)

	rep, err := s.Maintain(maintainCfg)
	if err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	if rep.DeletedConns != 0 || rep.DeletedRaw != 0 || rep.DeletedFrames != 0 || rep.DeletedChunks != 0 {
		t.Fatalf("reaped with no watermark: %+v (want all zero)", rep)
	}
}

// The chunk payload bytes have their own horizon (chunk_bytes_hours),
// measured from close: nulled at the horizon while the metadata row lives
// out the full raw window.
func TestMaintainChunkBytes(t *testing.T) {
	s := newTestStore(t)
	seedBucketRow(t, s, testBase)
	cfg := MaintainConfig{RawHours: 72, BucketDays: 365, ChunkBytesHours: 24}
	seedFinished(t, s, "gone", testBase.Add(-31*24*time.Hour), testBase.Add(-30*time.Hour), testBase.Add(-31*24*time.Hour), true, []byte{1})
	seedFinished(t, s, "live", testBase.Add(-11*24*time.Hour), testBase.Add(-10*time.Hour), testBase.Add(-11*24*time.Hour), true, []byte{2})

	rep, err := s.Maintain(cfg)
	if err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	if rep.NulledBytes != 1 {
		t.Fatalf("NulledBytes = %d, want 1 (only 'gone', closed 30 h ago)", rep.NulledBytes)
	}
	// 'gone': the metadata row survives the raw window, the payload is gone.
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM chunks WHERE conn_id = 'gone'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("'gone' chunk rows = %d, want 1", n)
	}
	var p []byte
	if err := s.db.QueryRow(`SELECT payload FROM chunks WHERE conn_id = 'gone'`).Scan(&p); err != nil {
		t.Fatal(err)
	}
	if p != nil {
		t.Fatalf("'gone' payload = %v, want NULL", p)
	}
	// 'live': the payload is intact.
	if err := s.db.QueryRow(`SELECT payload FROM chunks WHERE conn_id = 'live'`).Scan(&p); err != nil {
		t.Fatal(err)
	}
	if len(p) != 1 {
		t.Fatalf("'live' payload = %v, want intact", p)
	}
}

// Knob 0 = off: no byte pass runs at all — payloads ride with the row until
// the connection is reaped.
func TestMaintainChunkBytesOff(t *testing.T) {
	s := newTestStore(t)
	seedBucketRow(t, s, testBase)
	seedFinished(t, s, "old", testBase.Add(-31*24*time.Hour), testBase.Add(-30*time.Hour), testBase.Add(-31*24*time.Hour), true, []byte{1})

	rep, err := s.Maintain(maintainCfg) // ChunkBytesHours = 0
	if err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	if rep.NulledBytes != 0 {
		t.Fatalf("NulledBytes = %d, want 0 (off)", rep.NulledBytes)
	}
	var p []byte
	if err := s.db.QueryRow(`SELECT payload FROM chunks WHERE conn_id = 'old'`).Scan(&p); err != nil {
		t.Fatal(err)
	}
	if len(p) != 1 {
		t.Fatalf("payload = %v, want intact (knob off)", p)
	}
}

func TestMaintainRawRollupThenPrune(t *testing.T) {
	s := newTestStore(t) // now = testBase (12:00)
	// A finished session whose last bucket the rollup covers in this pass:
	// the bucket rolls up and the connection reaps in the same pass.
	// Watermark 07:00.
	seedBucketRow(t, s, testBase.Add(-101*time.Hour))
	// Bucket 08:00.
	seedTeleRow(t, s, testBase.Add(-100*time.Hour), 100, 0, 0, 0, 0, 50, 2)
	// c1 closed at 08:10 — after its last tele, inside the watermark's cover.
	closed := testBase.Add(-100*time.Hour + 10*time.Minute)
	s.RecordConn(capture.CloseInfo{
		ID: "c1", Kind: testKindTLS, Opened: testBase.Add(-101 * time.Hour),
		Closed: closed, Cause: testCauseEOF,
	}, 0)
	s.Flush()
	rep, err := s.Maintain(maintainCfg)
	if err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	if rep.Rolled != 1 || rep.DeletedRaw != 1 || rep.DeletedConns != 1 {
		t.Fatalf("Maintain: rolled %d raw %d conns %d, want 1/1/1", rep.Rolled, rep.DeletedRaw, rep.DeletedConns)
	}
}

func TestMaintainRecentRawSurvives(t *testing.T) {
	s := newTestStore(t)
	s.RecordConn(capture.CloseInfo{
		ID: "c1", Kind: testKindTLS, Opened: testBase.Add(-3 * time.Hour),
		Closed: testBase.Add(-time.Hour), Cause: testCauseEOF,
	}, 0)
	seedTeleRow(t, s, testBase.Add(-2*time.Hour), 100, 0, 0, 0, 0, 50, 2) // completed bucket
	seedTeleRow(t, s, testBase, 100, 0, 0, 0, 0, 50, 2)                   // in-flight bucket
	s.Flush()
	rep, err := s.Maintain(maintainCfg)
	if err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	if rep.Rolled != 1 {
		t.Fatalf("rolled %d, want 1 (only the completed bucket)", rep.Rolled)
	}
	if rep.DeletedRaw != 0 || rep.DeletedConns != 0 {
		t.Fatalf("DeletedRaw = %d conns = %d, want 0/0", rep.DeletedRaw, rep.DeletedConns)
	}
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM telemetry").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("telemetry rows = %d, want 2", n)
	}
}

func TestMaintainPrunesFramesChunks(t *testing.T) {
	s := newTestStore(t)
	seedBucketRow(t, s, testBase) // a current watermark
	old := testBase.Add(-100 * time.Hour)
	recent := testBase.Add(-1 * time.Hour)
	s.RecordConn(capture.CloseInfo{ID: "c1", Kind: testKindTLS, Opened: old.Add(-time.Hour), Closed: old, Cause: testCauseEOF}, 0)
	seedOldFrameChunk(t, s, "c1", old)
	s.RecordConn(capture.CloseInfo{ID: "c2", Kind: testKindTLS, Opened: recent.Add(-time.Hour), Closed: recent, Cause: testCauseEOF}, 0)
	seedOldFrameChunk(t, s, "c2", recent)
	s.Flush()

	rep, err := s.Maintain(maintainCfg)
	if err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	if rep.DeletedFrames != 1 || rep.DeletedChunks != 1 {
		t.Fatalf("frames %d chunks %d, want 1/1", rep.DeletedFrames, rep.DeletedChunks)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM frames`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("recent frame rows remaining = %d, want 1", n)
	}
}

// seedCrashedSession writes one store with a crashed session's 10 days
// of data — frame/chunk/telemetry rows whose connections row never
// came (persistStream writes it only at stream end) — plus a finished
// and an open connection, then closes without flushing that row.
func seedCrashedSession(t *testing.T, path string) {
	t.Helper()
	s, err := openStore(path, nil, maxConns, fixedClock(testBase), testVersion)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	ts := testBase.Add(-240 * time.Hour)
	seedOldFrameChunk(t, s, "orphan", ts)
	seedTeleRowFor(t, s, "orphan", ts)
	// The synthesis must not touch the finished or the open connection.
	s.RecordConn(capture.CloseInfo{
		ID: "done", Kind: testKindTLS, Opened: testBase.Add(-2 * time.Hour),
		Closed: testBase.Add(-time.Hour), Cause: testCauseEOF,
	}, 0)
	s.RecordConn(capture.CloseInfo{ID: "live", Kind: testKindTLS, Opened: testBase.Add(-time.Hour)}, 0)
	s.Flush()
	if err := s.Close(); err != nil { // the crash
		t.Fatalf("crash-close: %v", err)
	}
}

// A crashed session leaves frame/chunk/telemetry rows with no
// connections row — the close-based reaper, which only looks at
// connection rows, can never reach them. At store open, synthesize a
// finished connection with closed = the restart moment (when the stream
// stopped being written to).
func TestRestartOrphanSynthesizedAtOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	seedCrashedSession(t, path)

	s, err := openStore(path, nil, maxConns, fixedClock(testBase), testVersion)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	var cause, closed, opened string
	if err := s.db.QueryRow(`SELECT cause, closed, opened FROM connections WHERE id = 'orphan'`).
		Scan(&cause, &closed, &opened); err != nil {
		t.Fatalf("orphan not synthesized at open: %v", err)
	}
	if cause != "process-restart" || closed != encTS(testBase) || opened != encTS(testBase.Add(-240*time.Hour)) {
		t.Fatalf("synthesized row = (%q, %q, %q), want (process-restart, restart moment, MIN(ts))", cause, closed, opened)
	}

	// A second open synthesizes nothing (no orphans, no resurrection):
	// done, live, and the synthesized orphan — three connections total.
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	s3, err := openStore(path, nil, maxConns, fixedClock(testBase), testVersion)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	t.Cleanup(func() { _ = s3.Close() })
	var n int
	if err := s3.db.QueryRow(`SELECT COUNT(*) FROM connections`).Scan(&n); err != nil {
		t.Fatalf("count connections: %v", err)
	}
	if n != 3 {
		t.Fatalf("connections after the second open = %d, want 3", n)
	}
}

// The synthesized finished connection makes the orphan's rows
// reappable: once the raw window passes the restart moment (and the
// rollup watermark covers it), the regular close-based reap takes the
// whole session.
func TestRestartOrphanSessionReaped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	seedCrashedSession(t, path)

	s, err := openStore(path, nil, maxConns, fixedClock(testBase), testVersion)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// Advance the clock past the raw window; a bucket at the restart
	// moment covers the synthesized close.
	s.now = func() time.Time { return testBase.Add(73 * time.Hour) }
	seedBucketRow(t, s, testBase)
	rep, err := s.Maintain(maintainCfg)
	if err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	if rep.DeletedConns != 2 { // orphan + done; the open one never closes
		t.Fatalf("DeletedConns = %d, want 2", rep.DeletedConns)
	}
	for _, table := range []string{"frames", "chunks", "telemetry"} {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + table + ` WHERE conn_id = 'orphan'`).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 0 {
			t.Fatalf("%s: orphan rows left after the pass = %d", table, n)
		}
	}
	var liveClosed string
	if err := s.db.QueryRow(`SELECT closed FROM connections WHERE id = 'live'`).Scan(&liveClosed); err != nil {
		t.Fatalf("live reaped: %v", err)
	}
	if liveClosed != "" {
		t.Fatalf("live connection closed: %q", liveClosed)
	}
}

func TestMaintainBucketPrune(t *testing.T) {
	s := newTestStore(t)
	now := testBase.Unix()
	for _, d := range []int64{400, 300} { // days ago
		if _, err := s.db.Exec(upsertSQL, upsertArgs(now-d*86400, Agg{})...); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := s.Maintain(maintainCfg)
	if err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	if rep.DeletedBuckets != 1 {
		t.Fatalf("DeletedBuckets = %d, want 1 (only the 400 d row; 365 d horizon)", rep.DeletedBuckets)
	}
}
