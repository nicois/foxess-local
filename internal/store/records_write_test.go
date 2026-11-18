package store

import (
	"bytes"
	"testing"
	"time"

	"github.com/nicois/prox/pkg/capture"
)

// TestRecordAuditRoundTrip: audit events written through the batch queue
// round-trip through QueryAudit with kind/detail and the timestamp intact.
func TestRecordAuditRoundTrip(t *testing.T) {
	s := newTestStore(t)
	s.RecordAudit(testBase, "serve-decision", "conn-1: mirror (mode:telemetry_only)")
	s.RecordAudit(testBase.Add(1), "flow-close", "conn-1: eof")
	s.Flush()

	events, err := s.QueryAudit(testBase)
	if err != nil {
		t.Fatalf("QueryAudit: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("events: got %d, want 2", len(events))
	}
	if events[0].Kind != "serve-decision" || events[0].Detail != "conn-1: mirror (mode:telemetry_only)" {
		t.Errorf("e0: got %s %q", events[0].Kind, events[0].Detail)
	}
	if events[1].Kind != "flow-close" || events[1].Detail != "conn-1: eof" {
		t.Errorf("e1: got %s %q", events[1].Kind, events[1].Detail)
	}
	if only, err := s.QueryAudit(testBase, "flow-close"); err != nil {
		t.Fatalf("QueryAudit(kind): %v", err)
	} else if len(only) != 1 || only[0].Detail != "conn-1: eof" {
		t.Errorf("kind filter: got %d rows", len(only))
	}
}

// TestAuditSinceSameSecond: a row strictly after since but inside
// since's own second must be included by QueryAudit (the fixed-width
// encoding keeps the lexicographic ts comparison time-correct; the old
// RFC3339Nano widths sorted "…:00.000000001Z" before "…:00Z" and lost
// such rows).
// TestRecordChunkPayload: a chunk record carries its payload bytes only
// when the capture stream had byte capture enabled (G2) — the row
// persists them verbatim; otherwise the column is NULL.
func TestRecordChunkPayload(t *testing.T) {
	s := newTestStore(t)
	s.RecordConn(capture.CloseInfo{
		ID: "pc", Kind: testKindTLS, Opened: testBase,
		Closed: testBase.Add(time.Second), Cause: testCauseEOF,
	}, 0)
	s.RecordChunk(capture.Chunk{
		Dir: capture.DirC2S, TS: testBase, Len: 3,
		Decision: capture.Forwarded, Payload: []byte{1, 2, 3},
	}, "pc")
	s.RecordChunk(capture.Chunk{
		Dir: capture.DirC2S, TS: testBase.Add(time.Second), Len: 4,
		Decision: capture.Forwarded,
	}, "pc")
	s.Flush()

	var with, without []byte
	if err := s.db.QueryRow(`SELECT payload FROM chunks WHERE conn_id = 'pc' AND seq = 0`).Scan(&with); err != nil {
		t.Fatalf("scan seq=0: %v", err)
	}
	if !bytes.Equal(with, []byte{1, 2, 3}) {
		t.Fatalf("seq=0 payload = %v, want [1 2 3]", with)
	}
	if err := s.db.QueryRow(`SELECT payload FROM chunks WHERE conn_id = 'pc' AND seq = 1`).Scan(&without); err != nil {
		t.Fatalf("scan seq=1: %v", err)
	}
	if without != nil {
		t.Fatalf("seq=1 payload = %v, want NULL", without)
	}
}

func TestAuditSinceSameSecond(t *testing.T) {
	s := newTestStore(t)
	since := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	s.RecordAudit(since, "a0", "at since")
	s.RecordAudit(since.Add(1), "a1", "one ns after since")
	s.RecordAudit(since.Add(-1), "a-1", "before since")
	s.Flush()

	events, err := s.QueryAudit(since)
	if err != nil {
		t.Fatalf("QueryAudit: %v", err)
	}
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	if len(events) != 2 || kinds[0] != "a0" || kinds[1] != "a1" {
		t.Errorf("events: got %v, want [a0 a1]", kinds)
	}
}

// TestRecordConnStampsAppVersion: every connections row carries the
// version of the binary that persisted it (release traceability,
// D-018) — stored data is attributable to a build.
func TestRecordConnStampsAppVersion(t *testing.T) {
	s := newTestStore(t)
	s.RecordConn(capture.CloseInfo{
		ID: "vc", Kind: testKindTLS, Opened: testBase,
		Closed: testBase.Add(time.Second), Cause: testCauseEOF,
	}, 0)
	s.Flush()

	var v string
	if err := s.db.QueryRow(`SELECT version FROM connections WHERE id = 'vc'`).Scan(&v); err != nil {
		t.Fatalf("SELECT version: %v", err)
	}
	if v != testVersion {
		t.Errorf("version: got %q, want %q", v, testVersion)
	}
}
