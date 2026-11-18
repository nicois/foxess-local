package store

import (
	"testing"
	"time"

	"github.com/nicois/prox/pkg/capture"
)

// wantChunk is one expected persisted chunk row.
type wantChunk struct {
	seq                 int
	dec, reason, origin string
	dir, tsText         string
	ln                  int
}

// checkChunkRows asserts the persisted chunk rows equal want in order.
func checkChunkRows(t *testing.T, s *Store, want []wantChunk) {
	t.Helper()
	rows, err := s.db.Query("SELECT seq, decision, reason, origin, dir, len, ts FROM chunks ORDER BY seq")
	if err != nil {
		t.Fatalf("chunks: %v", err)
	}
	var (
		i   int
		got wantChunk
	)
	for rows.Next() {
		if err := rows.Scan(&got.seq, &got.dec, &got.reason, &got.origin,
			&got.dir, &got.ln, &got.tsText); err != nil {
			t.Fatalf("scan chunk: %v", err)
		}
		if i >= len(want) || got != want[i] {
			t.Errorf("chunk %d: got %+v", i, got)
		}
		i++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close chunks: %v", err)
	}
	if i != len(want) {
		t.Errorf("chunk rows: got %d, want %d", i, len(want))
	}
}

// TestCaptureWiring pins AttachRegistry: a finished registry stream is
// persisted as a complete connections row (4-tuple, SNI, TLS, cause) plus
// its chunk rows with decision/reason/origin, using real capture types.
func TestCaptureWiring(t *testing.T) {
	s := newTestStore(t)
	reg := capture.NewRegistry()
	s.AttachRegistry(reg)

	st := reg.Open(capture.CloseInfo{
		ID: testStreamID, Kind: testKindTLS, SNI: testSNI,
		Client: testClient, Dst: testDst,
	})
	st.SetTLS(&capture.TLSInfo{Host: testSNI, Version: 0x0303, Result: "success"})
	st.Record(capture.Chunk{Dir: capture.DirC2S, TS: testBase, Len: 135, Decision: capture.Forwarded})
	st.Record(capture.Chunk{
		Dir: capture.DirS2C, TS: testBase.Add(time.Second), Len: 167,
		Decision: capture.Dropped, Reason: testDropReason,
	})
	st.Record(capture.Chunk{
		Dir: capture.DirS2C, TS: testBase.Add(2 * time.Second), Len: 150,
		Decision: capture.Injected, Origin: testGatewayOrig,
	})
	st.Finish(testCauseEOF)
	s.WaitStreams()
	s.Flush()

	var (
		kind, sni, client, dst, opened, closed, cause string
		tlsHost, tlsResult                            string
		tlsVersion, evicted                           int
	)
	err := s.db.QueryRow(`SELECT kind, sni, client, dst, opened, closed, cause,
		tls_host, tls_version, tls_result, evicted_chunks FROM connections WHERE id = ?`,
		testStreamID).
		Scan(&kind, &sni, &client, &dst, &opened, &closed, &cause,
			&tlsHost, &tlsVersion, &tlsResult, &evicted)
	if err != nil {
		t.Fatalf("connections row: %v", err)
	}
	if kind != testKindTLS || sni != testSNI ||
		client != testClient || dst != testDst {
		t.Errorf("4-tuple: got kind=%s sni=%s client=%s dst=%s", kind, sni, client, dst)
	}
	if opened == "" || closed == "" || cause != testCauseEOF {
		t.Errorf("lifecycle: opened=%q closed=%q cause=%q", opened, closed, cause)
	}
	if tlsHost != testSNI || tlsVersion != 0x0303 || tlsResult != "success" {
		t.Errorf("tls: got host=%s version=0x%04x result=%s", tlsHost, tlsVersion, tlsResult)
	}
	if evicted != 0 {
		t.Errorf("evicted: got %d, want 0", evicted)
	}

	want := []wantChunk{
		{0, "forwarded", "", "", "c2s", encTS(testBase), 135},
		{1, string(decisionDropped), testDropReason, "", "s2c", encTS(testBase.Add(time.Second)), 167},
		{2, "injected", "", testGatewayOrig, "s2c", encTS(testBase.Add(2 * time.Second)), 150},
	}
	checkChunkRows(t, s, want)
}

// rangeCount returns the number of rows in one TelemetryRange call.
func rangeCount(t *testing.T, s *Store, since, until time.Time) int {
	t.Helper()
	rows, err := s.TelemetryRange(since, until)
	if err != nil {
		t.Fatalf("TelemetryRange(%v, %v): %v", since, until, err)
	}
	return len(rows)
}

// TestTelemetryRange pins the 5 s-cadence projection and the range-bound
// semantics: since-inclusive, until-exclusive.
func TestTelemetryRange(t *testing.T) {
	s := newTestStore(t)
	const (
		runFrames = 120 // a 10-minute run at 5 s cadence
		cadence   = 5 * time.Second
		runSpan   = 10 * time.Minute
	)
	for i := range runFrames {
		rec := frameRecord(testConnID, i, capture.DirC2S, classTele,
			telePayload(int32(i+1), -int32(i), int32(100-i), 0, 0, byte(i), 2))
		rec.TS = testBase.Add(time.Duration(i) * cadence)
		s.RecordFrame(rec)
	}
	s.Flush()
	if n := countRows(t, s, tableTelemetry); n != runFrames {
		t.Fatalf("telemetry rows: got %d, want %d", n, runFrames)
	}

	// Range-bound semantics: since-inclusive, until-exclusive. The last
	// row is at testBase+595 s, so an until of that exact timestamp
	// excludes it.
	type rangeCase struct {
		since, until time.Time
		want         int
	}
	cases := []rangeCase{
		{testBase, testBase.Add(runSpan), runFrames},
		{testBase.Add(cadence), testBase.Add(runSpan), runFrames - 1},
		{testBase, testBase.Add(time.Duration(runFrames-1) * cadence), runFrames - 1},
		{testBase, testBase.Add(time.Duration(runFrames) * cadence), runFrames},
	}
	for i, c := range cases {
		if n := rangeCount(t, s, c.since, c.until); n != c.want {
			t.Errorf("range %d: got %d, want %d", i, n, c.want)
		}
	}

	// Field values round-trip at both ends.
	checkTelemetryEnds(t, s, runFrames, cadence)
}

// checkTelemetryEnds asserts the first and last telemetry rows round-trip
// their field values.
func checkTelemetryEnds(t *testing.T, s *Store, runFrames int, cadence time.Duration) {
	t.Helper()
	rows, err := s.TelemetryRange(testBase, testBase.Add(time.Duration(runFrames)*cadence))
	if err != nil {
		t.Fatalf("TelemetryRange: %v", err)
	}
	if rows[0].Sol != 1 || rows[0].Grid != 0 || rows[0].Soc != 0 ||
		!rows[0].TS.Equal(testBase) || rows[0].ConnID != testConnID {
		t.Errorf("first row: got %+v", rows[0])
	}
	last := rows[runFrames-1]
	//nolint:gosec // G115: test index arithmetic, bounded by runFrames.
	if int(last.Sol) != runFrames || last.Grid != int32(-(runFrames-1)) ||
		last.Haus != int32(100-(runFrames-1)) || last.Soc != runFrames-1 ||
		!last.TS.Equal(testBase.Add(time.Duration(runFrames-1)*cadence)) {
		t.Errorf("last row: got %+v", last)
	}
}
