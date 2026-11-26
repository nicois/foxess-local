package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nicois/prox/pkg/capture"
)

// TestDiscardRawBefore: the disk-pressure discard deletes raw rows
// (frames, chunks, telemetry) older than the cutoff whatever their
// session — an open session too, unlike the retention pass — after a
// rollup, so the history survives as 15-minute buckets.
func TestDiscardRawBefore(t *testing.T) {
	s := newTestStore(t)
	s.now = fixedClock(testBase.Add(24 * time.Hour))
	old := testBase.Add(-2 * time.Hour)
	recent := testBase.Add(23 * time.Hour)
	for _, ts := range []time.Time{old, old.Add(time.Minute), recent} {
		seedOldFrameChunk(t, s, "open-session", ts) // never closed
		seedTeleRowFor(t, s, "open-session", ts)
	}

	oldest, ok, err := s.OldestRaw()
	if err != nil || !ok || !oldest.Equal(old) {
		t.Fatalf("OldestRaw: %v %v %v, want %v", oldest, ok, err, old)
	}
	rep, err := s.DiscardRawBefore(testBase)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Frames != 2 || rep.Chunks != 2 || rep.Telemetry != 2 {
		t.Fatalf("discard: %+v, want 2 frames, 2 chunks, 2 telemetry", rep)
	}
	for table, want := range map[string]int{tableFrames: 1, "chunks": 1, "telemetry": 1} {
		if got := countRows(t, s, table); got != want {
			t.Errorf("%s rows: %d, want %d", table, got, want)
		}
	}
	if got := countRows(t, s, "buckets15m"); got == 0 {
		t.Error("no rollup buckets: the discarded telemetry left no history")
	}
	if oldest, ok, _ := s.OldestRaw(); !ok || !oldest.Equal(recent) {
		t.Errorf("OldestRaw after: %v %v, want %v", oldest, ok, recent)
	}
}

// TestNewStoreIncrementalVacuum: a new database is created with
// incremental auto-vacuum, so Reclaim returns discarded pages to the
// filesystem (without it they are only reused).
func TestNewStoreIncrementalVacuum(t *testing.T) {
	s := newTestStore(t)
	var mode int
	if err := s.db.QueryRow(`PRAGMA auto_vacuum`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != autoVacuumIncremental {
		t.Fatalf("auto_vacuum: %d, want %d (incremental)", mode, autoVacuumIncremental)
	}
	big := make([]byte, 4096)
	for i := range 500 {
		r := frameRecord(testConnID, i, capture.DirC2S, "keepalive", big)
		r.TS = testBase.Add(-time.Duration(i) * time.Second)
		s.RecordFrame(r)
	}
	s.Flush()
	before := s.pageCount(t)
	if _, err := s.DiscardRawBefore(testBase.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.Reclaim(); err != nil {
		t.Fatal(err)
	}
	if after := s.pageCount(t); after >= before/2 {
		t.Fatalf("pages %d -> %d: Reclaim returned too little", before, after)
	}
}

// TestEnableIncrementalVacuum: an existing database without auto-vacuum
// converts when there is room for the rewrite, and not otherwise.
func TestEnableIncrementalVacuum(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	s, err := openStore(path, nil, maxConns, fixedClock(testBase), testVersion)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`PRAGMA auto_vacuum = NONE; VACUUM`); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.EnableIncrementalVacuum(1); err != nil || ok {
		t.Fatalf("converted with no room: %v %v", ok, err)
	}
	if ok, err := s.EnableIncrementalVacuum(1 << 30); err != nil || !ok {
		t.Fatalf("not converted with room: %v %v", ok, err)
	}
	var mode int
	if err := s.db.QueryRow(`PRAGMA auto_vacuum`).Scan(&mode); err != nil || mode != autoVacuumIncremental {
		t.Fatalf("auto_vacuum after: %d %v", mode, err)
	}
	_ = s.Close()
}

func (s *Store) pageCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`PRAGMA page_count`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestMigrationRefusesNewerSchema (install review G3): a database written
// by a newer binary (a rollback runs the old one) is refused with a
// clear message, never run against.
func TestMigrationRefusesNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.db")
	s, err := openStore(path, nil, maxConns, fixedClock(testBase), testVersion)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO schema_migrations(version) VALUES (99)`); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if _, err := openStore(path, nil, maxConns, fixedClock(testBase), testVersion); err == nil ||
		!strings.Contains(err.Error(), "newer") {
		t.Fatalf("opened a v99 database: %v", err)
	}
}

// TestMigrationSnapshot (install review G3): before migrating an existing
// database the store keeps a copy of it (<path>.pre-v<N>), so a rollback
// has the old schema to return to.
func TestMigrationSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.db")
	s, err := openStore(path, nil, maxConns, fixedClock(testBase), testVersion)
	if err != nil {
		t.Fatal(err)
	}
	latest := 0
	if err := s.db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&latest); err != nil {
		t.Fatal(err)
	}
	// back one version: the latest step (v15, meter 2) undone
	if _, err := s.db.Exec(`DELETE FROM schema_migrations WHERE version = ?`, latest); err != nil {
		t.Fatal(err)
	}
	undoV15Columns(t, s)
	_ = s.Close()
	s2, err := openStore(path, nil, maxConns, fixedClock(testBase), testVersion)
	if err != nil {
		t.Fatal(err)
	}
	_ = s2.Close()
	snap := fmt.Sprintf("%s.pre-v%d", path, latest)
	if _, err := os.Stat(snap); err != nil {
		t.Fatalf("no snapshot before migrating: %v", err)
	}
}

// undoV9Columns drops the schema-v9 columns (a test going back to an
// older schema re-runs v9, which adds them).
func undoV9Columns(t *testing.T, s *Store) {
	t.Helper()
	for _, c := range []string{
		"telemetry DROP COLUMN t_batt", "telemetry DROP COLUMN t_inv", "telemetry DROP COLUMN t_amb",
		"telemetry DROP COLUMN bms_chg", "buckets15m DROP COLUMN t_batt_avg", "buckets15m DROP COLUMN t_inv_avg",
		"buckets15m DROP COLUMN t_amb_avg", "buckets15m DROP COLUMN bms_chg_avg",
	} {
		if _, err := s.db.Exec("ALTER TABLE " + c); err != nil {
			t.Fatal(err)
		}
	}
}

// undoV10Columns drops the schema-v10 columns.
// undoV15Columns drops the v15 columns (meter 2).
func undoV15Columns(t *testing.T, s *Store) {
	t.Helper()
	for _, c := range []string{
		"telemetry DROP COLUMN meter2", "buckets15m DROP COLUMN meter2_avg", "buckets15m DROP COLUMN meter2_gen_wh",
	} {
		if _, err := s.db.Exec("ALTER TABLE " + c); err != nil {
			t.Fatal(err)
		}
	}
}

// undoV14 drops the v14 column and table (the notices).
func undoV14(t *testing.T, s *Store) {
	t.Helper()
	for _, q := range []string{"ALTER TABLE notify_outbox DROP COLUMN origin", "DROP TABLE notices"} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
}

// undoV13Columns drops the v13 columns (the SoC estimate).
func undoV13Columns(t *testing.T, s *Store) {
	t.Helper()
	for _, c := range []string{"telemetry DROP COLUMN soc_est", "buckets15m DROP COLUMN soc_est_last"} {
		if _, err := s.db.Exec("ALTER TABLE " + c); err != nil {
			t.Fatal(err)
		}
	}
}

// undoV12Columns drops the v12 column (the buckets' EPS average).
func undoV12Columns(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.db.Exec("ALTER TABLE buckets15m DROP COLUMN eps_avg"); err != nil {
		t.Fatal(err)
	}
}

func undoV10Columns(t *testing.T, s *Store) {
	t.Helper()
	for _, c := range []string{
		"telemetry DROP COLUMN t_cell_min", "telemetry DROP COLUMN t_cell_max",
		"buckets15m DROP COLUMN t_cell_min_avg", "buckets15m DROP COLUMN t_cell_max_avg",
	} {
		if _, err := s.db.Exec("ALTER TABLE " + c); err != nil {
			t.Fatal(err)
		}
	}
}
