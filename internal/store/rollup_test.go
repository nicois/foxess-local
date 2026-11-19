package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/nicois/prox/pkg/capture"

	"foxesslocal/internal/frame"
)

// seedTeleRow inserts one raw telemetry row directly (test helper).
func seedTeleRow(t *testing.T, s *Store, ts time.Time, sol, grid, haus, acport, batt, soc, charge int) {
	t.Helper()
	_, err := s.db.Exec(`INSERT INTO telemetry(conn_id, ts, sol, grid, haus, acport, batt, soc, charge_state, idnt_grid, idnt_ac, haus_dup)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		"c1", encTS(ts), sol, grid, haus, acport, batt, soc, charge, 0, 0, 0)
	if err != nil {
		t.Fatalf("seedTeleRow: %v", err)
	}
}

// seedBucket seeds 180 samples (a full 15-min bucket at 5 s cadence) with constant values.
func seedBucket(t *testing.T, s *Store, start time.Time, sol, grid, haus, acport, batt, soc, charge int) {
	t.Helper()
	for i := range 180 {
		seedTeleRow(t, s, start.Add(time.Duration(i)*5*time.Second), sol, grid, haus, acport, batt, soc, charge)
	}
}

func TestRollUpStats(t *testing.T) {
	s := newTestStore(t) // s.now = testBase (12:00, 15-min aligned)
	seedBucket(t, s, time.Date(2026, 11, 8, 11, 45, 0, 0, time.UTC), 1000, -500, 600, 10, 200, 61, 4)
	n, err := s.RollUp()
	if err != nil {
		t.Fatalf("RollUp: %v", err)
	}
	if n != 1 {
		t.Fatalf("RollUp: got %d buckets, want 1", n)
	}
	got, err := s.HistoryBuckets(time.Date(2026, 11, 8, 11, 45, 0, 0, time.UTC), testBase)
	if err != nil {
		t.Fatalf("HistoryBuckets: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("HistoryBuckets: got %d rows, want 1", len(got))
	}
	checkRollUpStats(t, got[0])
}

// checkRollUpStats asserts the per-series stats of the constant-value
// bucket of TestRollUpStats.
func checkRollUpStats(t *testing.T, a Agg) {
	t.Helper()
	// 180 samples * 5 s = 900 s of energy per constant-W value.
	if a.SolAvg != 1000 || a.SolMin != 1000 || a.SolMax != 1000 || a.SolWh != 1000*900/3600 {
		t.Errorf("sol = avg %d min %d max %d wh %d, want 1000/1000/1000/250", a.SolAvg, a.SolMin, a.SolMax, a.SolWh)
	}
	if a.GridAvg != -500 || a.GridImpWh != 0 || a.GridExpWh != 500*900/3600 {
		t.Errorf("grid = avg %d imp %d exp %d, want -500/0/125", a.GridAvg, a.GridImpWh, a.GridExpWh)
	}
	if a.BattChgWh != 200*900/3600 || a.BattDisWh != 0 {
		t.Errorf("batt chg %d dis %d, want 50/0", a.BattChgWh, a.BattDisWh)
	}
	if a.AcportAvg != 10 || a.SocLast != 61 {
		t.Errorf("acport %d soc %d, want 10/61", a.AcportAvg, a.SocLast)
	}
}

func TestRollUpDirectionSplits(t *testing.T) {
	s := newTestStore(t)
	// 90 samples of +300 grid, 90 of -300 grid, in one bucket.
	start := time.Date(2026, 11, 8, 11, 45, 0, 0, time.UTC)
	for i := range 180 {
		g := -300
		if i%2 == 0 {
			g = 300
		}
		seedTeleRow(t, s, start.Add(time.Duration(i)*5*time.Second), 0, g, 0, 0, 0, 50, 2)
	}
	if _, err := s.RollUp(); err != nil {
		t.Fatalf("RollUp: %v", err)
	}
	got, err := s.HistoryBuckets(start, testBase)
	if err != nil || len(got) != 1 {
		t.Fatalf("HistoryBuckets: %v, %d rows", err, len(got))
	}
	a := got[0]
	if a.GridAvg != 0 || a.GridImpWh != 90*300*5/3600 || a.GridExpWh != 90*300*5/3600 {
		t.Errorf("grid avg %d imp %d exp %d, want 0/37/37", a.GridAvg, a.GridImpWh, a.GridExpWh)
	}
}

func TestRollUpIdempotent(t *testing.T) {
	s := newTestStore(t)
	start := time.Date(2026, 11, 8, 11, 45, 0, 0, time.UTC)
	seedBucket(t, s, start, 100, 0, 0, 0, 0, 50, 2)
	if _, err := s.RollUp(); err != nil {
		t.Fatalf("RollUp 1: %v", err)
	}
	again, err := s.HistoryBuckets(start, testBase)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RollUp(); err != nil { // second pass re-covers the watermark bucket
		t.Fatalf("RollUp 2: %v", err)
	}
	got, err := s.HistoryBuckets(start, testBase)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != again[0] {
		t.Fatalf("rollup not idempotent: %v vs %v", got, again)
	}
}

func TestRollUpBackfillFromMin(t *testing.T) {
	s := newTestStore(t)
	// Two non-adjacent old buckets, nothing in between.
	seedBucket(t, s, time.Date(2026, 11, 8, 11, 45, 0, 0, time.UTC), 100, 0, 0, 0, 0, 50, 2)
	seedBucket(t, s, time.Date(2026, 11, 8, 11, 0, 0, 0, time.UTC), 200, 0, 0, 0, 0, 51, 2)
	n, err := s.RollUp()
	if err != nil {
		t.Fatalf("RollUp: %v", err)
	}
	if n != 2 {
		t.Fatalf("RollUp: got %d buckets, want 2 (empty middle window is skipped)", n)
	}
}

func TestRollUpPartialBucketThenAdvance(t *testing.T) {
	s := newTestStore(t) // now = 12:00 → bucket [12:00,12:15) is "in flight"
	seedTeleRow(t, s, testBase, 1000, 0, 0, 0, 0, 50, 2)
	n, err := s.RollUp()
	if err != nil {
		t.Fatalf("RollUp: %v", err)
	}
	if n != 0 {
		t.Fatalf("RollUp: got %d buckets, want 0 (nothing completed)", n)
	}
	s.now = func() time.Time { return testBase.Add(30 * time.Minute) } // now = 12:30 → bucket 12:00 completed
	n, err = s.RollUp()
	if err != nil {
		t.Fatalf("RollUp: %v", err)
	}
	if n != 1 {
		t.Fatalf("RollUp: got %d buckets, want 1", n)
	}
	got, err := s.HistoryBuckets(testBase, testBase.Add(30*time.Minute))
	if err != nil || len(got) != 1 {
		t.Fatalf("HistoryBuckets: %v, %d rows", err, len(got))
	}
	// One sample → 1 * 5 s of energy.
	if got[0].SolWh != 1000*5/3600 {
		t.Fatalf("sol_wh = %d, want %d", got[0].SolWh, 1000*5/3600)
	}
}

func TestRollUpSelfHealsLastBucket(t *testing.T) {
	s := newTestStore(t)
	start := time.Date(2026, 11, 8, 11, 45, 0, 0, time.UTC)
	seedBucket(t, s, start, 1000, 0, 0, 0, 0, 60, 2)
	if _, err := s.RollUp(); err != nil {
		t.Fatalf("RollUp 1: %v", err)
	}
	// A late sample lands in the last (watermark) bucket.
	seedTeleRow(t, s, start.Add(14*time.Minute), 2000, 0, 0, 0, 0, 62, 2)
	if _, err := s.RollUp(); err != nil { // re-covers the watermark bucket
		t.Fatalf("RollUp 2: %v", err)
	}
	got, err := s.HistoryBuckets(start, testBase)
	if err != nil || len(got) != 1 {
		t.Fatalf("HistoryBuckets: %v, %d rows", err, len(got))
	}
	want := (180*1000 + 2000) / 181
	if got[0].SolAvg != want || got[0].SocLast != 62 {
		t.Fatalf("after heal: sol_avg %d (want %d), soc %d (want 62)", got[0].SolAvg, want, got[0].SocLast)
	}
}

func TestHistoryRaw(t *testing.T) {
	s := newTestStore(t)
	// 6 samples in the minute [11:55, 11:56).
	for i := range 6 {
		seedTeleRow(t, s, time.Date(2026, 11, 8, 11, 55, 0, 0, time.UTC).Add(time.Duration(i)*5*time.Second), 300, 0, 0, 0, 0, 50, 2)
	}
	got, err := s.HistoryRaw(time.Date(2026, 11, 8, 11, 50, 0, 0, time.UTC), testBase)
	if err != nil {
		t.Fatalf("HistoryRaw: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("HistoryRaw: got %d rows, want 1", len(got))
	}
	if !got[0].TS.Equal(time.Date(2026, 11, 8, 11, 55, 0, 0, time.UTC)) {
		t.Fatalf("TS = %v", got[0].TS)
	}
	if got[0].SolAvg != 300 || got[0].SolWh != 6*5*300/3600 {
		t.Fatalf("sol avg %d wh %d, want 300/25", got[0].SolAvg, got[0].SolWh)
	}
}

func TestRollUpEmptyStore(t *testing.T) {
	s := newTestStore(t)
	n, err := s.RollUp()
	if err != nil {
		t.Fatalf("RollUp: %v", err)
	}
	if n != 0 {
		t.Fatalf("RollUp: got %d buckets, want 0", n)
	}
}

// TestHistoryRawMinutes pins the per-minute windows: one row per minute
// with samples (empty minutes absent), each with its own aggregates and
// the SoC of its last-arriving sample; a sample at or after until is out.
func TestHistoryRawMinutes(t *testing.T) {
	s := newTestStore(t)
	m0 := time.Date(2026, 11, 8, 11, 50, 0, 0, time.UTC)
	seedTeleRow(t, s, m0.Add(10*time.Second), 100, 0, 0, 0, 0, 40, 2)
	seedTeleRow(t, s, m0.Add(50*time.Second), 300, 0, 0, 0, 0, 41, 2)
	seedTeleRow(t, s, m0.Add(2*time.Minute), 500, 0, 0, 0, 0, 45, 2) // 11:52, minute 11:51 empty
	seedTeleRow(t, s, m0.Add(3*time.Minute), 900, 0, 0, 0, 0, 47, 2) // at until: out
	got, err := s.HistoryRaw(m0.Add(5*time.Second), m0.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("rows: %d, want 2: %+v", len(got), got)
	}
	if !got[0].TS.Equal(m0) || got[0].SolAvg != 200 || got[0].SolMin != 100 || got[0].SolMax != 300 || got[0].SocLast != 41 || got[0].N != 2 {
		t.Errorf("11:50: %+v", got[0])
	}
	if !got[1].TS.Equal(m0.Add(2*time.Minute)) || got[1].SolAvg != 500 || got[1].SocLast != 45 {
		t.Errorf("11:52: %+v", got[1])
	}
}

// TestRepairWrappedPV: telemetry written by the unsigned PV decode (a
// −2 W string stored as 65 534 W) is un-wrapped on open (schema v8) to 0
// W — standby draw, not generation, as the fixed decode reports it —
// and the 15-minute bucket built from it is recomputed from the raw
// samples.
func TestRepairWrappedPV(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.db")
	s, err := openStore(path, nil, maxConns, fixedClock(testBase.Add(time.Hour)), testVersion)
	if err != nil {
		t.Fatal(err)
	}
	b := testBase.Truncate(15 * time.Minute)
	seedTeleRow(t, s, b, 65534, 0, 300, 0, -300, 50, 2) // a wrapped −2 W
	seedTeleRow(t, s, b.Add(5*time.Second), 0, 0, 300, 0, -300, 50, 2)
	seedTeleRow(t, s, b.Add(10*time.Second), 3000, 0, 300, 0, 2700, 50, 2) // a real 3 kW
	if _, err := s.RollUp(); err != nil {
		t.Fatal(err)
	}
	// back to v7: the repair (v8) has not run on this database yet
	if _, err := s.db.Exec(`DELETE FROM schema_migrations WHERE version >= 8`); err != nil {
		t.Fatal(err)
	}
	undoV13Columns(t, s)
	undoV12Columns(t, s)
	undoV10Columns(t, s)
	undoV9Columns(t, s)
	_ = s.Close()

	s2, err := openStore(path, nil, maxConns, fixedClock(testBase.Add(time.Hour)), testVersion)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close() }()
	var minSol, maxSol int
	if err := s2.db.QueryRow(`SELECT MIN(sol), MAX(sol) FROM telemetry`).Scan(&minSol, &maxSol); err != nil {
		t.Fatal(err)
	}
	if minSol != 0 || maxSol != 3000 {
		t.Fatalf("telemetry sol %d..%d, want 0..3000 (the wrapped −2 W stored as 0)", minSol, maxSol)
	}
	var bMax int
	if err := s2.db.QueryRow(`SELECT sol_max FROM buckets15m WHERE bucket = ?`, b.Unix()).Scan(&bMax); err != nil {
		t.Fatal(err)
	}
	if bMax != 3000 {
		t.Fatalf("bucket sol_max %d, want 3000 (recomputed)", bMax)
	}
}

// TestTemperaturesRollUp: the register poll's temperatures and BMS
// charge limit (tenths) are stored with each sample and averaged into the
// 15-minute buckets; a sample without them (the cloud-era 150 B frame)
// leaves them NULL — never a misleading 0 °C.
func TestTemperaturesRollUp(t *testing.T) {
	s := newTestStore(t)
	b := testBase.Truncate(15 * time.Minute)
	s.now = fixedClock(b.Add(time.Hour))
	s.RecordTelemetry(testConnID, b, frame.Tele{
		Sol: 100, Soc: 50,
		Temps: &frame.Temps{BattDeciC: 270, InvDeciC: 410, AmbDeciC: 300, BMSChargeDeciA: 500},
	})
	s.RecordTelemetry(testConnID, b.Add(5*time.Second), frame.Tele{
		Sol: 100, Soc: 50,
		Temps: &frame.Temps{BattDeciC: 290, InvDeciC: 430, AmbDeciC: 300, BMSChargeDeciA: 300},
	})
	s.RecordTelemetry(testConnID, b.Add(15*time.Minute), frame.Tele{Sol: 100, Soc: 50}) // no temps
	s.Flush()
	if _, err := s.RollUp(); err != nil {
		t.Fatal(err)
	}
	aggs, err := s.HistoryBuckets(b, b.Add(time.Hour))
	if err != nil || len(aggs) != 2 {
		t.Fatalf("buckets %+v %v", aggs, err)
	}
	a := aggs[0]
	if !a.TBatt.Valid || a.TBatt.Int64 != 280 || a.TInv.Int64 != 420 || a.TAmb.Int64 != 300 || a.BMSChg.Int64 != 400 {
		t.Fatalf("bucket temps %+v", a)
	}
	if aggs[1].TBatt.Valid || aggs[1].BMSChg.Valid {
		t.Fatalf("a bucket without temperature samples: %+v", aggs[1])
	}
	raw, err := s.HistoryRaw(b, b.Add(2*time.Minute))
	if err != nil || len(raw) == 0 || raw[0].TBatt.Int64 != 280 {
		t.Fatalf("raw minute temps %+v %v", raw, err)
	}
}

// TestCellTempsHeldFromSnap61: the BMS cell temperatures arrive in the
// ~5-minute snap61 snapshot; each live sample carries the newest one
// while it is at most CellTempMaxAge old (then NULL: never a stale value
// presented as current).
func TestCellTempsHeldFromSnap61(t *testing.T) {
	s := newTestStore(t)
	b := testBase.Truncate(15 * time.Minute)
	s.now = fixedClock(b.Add(2 * time.Hour))
	p := snap61Payload("INVSN12345678", 57, 600, nil)
	e := 40 + 2*12
	p[e], p[e+1] = 0x18, 0x74
	copy(p[e+2:], []byte{0xfd, 0x00, 0xaf, 0x00}) // max 25.3, min 17.5 °C
	snap := frameRecord(testConnID, 1, capture.DirC2S, classSnap61, p)
	snap.TS = b
	s.RecordFrame(snap)
	s.Flush()
	temps := func() frame.Tele { return frame.Tele{Soc: 50, Temps: &frame.Temps{BattDeciC: 240}} }
	s.RecordTelemetry(testConnID, b.Add(time.Minute), temps())
	s.RecordTelemetry(testConnID, b.Add(CellTempMaxAge+time.Minute), temps()) // the snapshot is stale by then
	s.Flush()
	if _, err := s.RollUp(); err != nil {
		t.Fatal(err)
	}
	aggs, err := s.HistoryBuckets(b, b.Add(time.Hour))
	if err != nil || len(aggs) != 2 {
		t.Fatalf("buckets %+v %v", aggs, err)
	}
	if a := aggs[0]; a.TCellMin.Int64 != 175 || a.TCellMax.Int64 != 253 || !a.TCellMin.Valid {
		t.Fatalf("held cell temps: %+v", a)
	}
	if a := aggs[1]; a.TCellMin.Valid || !a.TBatt.Valid {
		t.Fatalf("stale snapshot: %+v", a)
	}
}

// TestEPSRollUp (2026-11-19): the EPS (backup) port output is averaged
// into the 15-minute buckets, so the history can show the share of the
// household load that the backup circuits drew (load includes it).
func TestEPSRollUp(t *testing.T) {
	s := newTestStore(t)
	b := testBase.Truncate(15 * time.Minute)
	s.now = fixedClock(b.Add(time.Hour))
	s.RecordTelemetry(testConnID, b, frame.Tele{Haus: 2000, Eps: 600, Soc: 50})
	s.RecordTelemetry(testConnID, b.Add(5*time.Second), frame.Tele{Haus: 2000, Eps: 800, Soc: 50})
	s.Flush()
	if _, err := s.RollUp(); err != nil {
		t.Fatal(err)
	}
	aggs, err := s.HistoryBuckets(b, b.Add(time.Hour))
	if err != nil || len(aggs) != 1 {
		t.Fatalf("buckets %+v %v", aggs, err)
	}
	if !aggs[0].Eps.Valid || aggs[0].Eps.Int64 != 700 {
		t.Fatalf("bucket eps %+v", aggs[0].Eps)
	}
	raw, err := s.HistoryRaw(b, b.Add(time.Minute))
	if err != nil || len(raw) != 1 || raw[0].Eps.Int64 != 700 {
		t.Fatalf("raw minute eps %+v %v", raw, err)
	}
}

// TestEPSBackfill (2026-11-19): a bucket rolled up before buckets kept
// EPS (eps_avg NULL) gets it from its raw samples while they are kept;
// one whose samples are gone stays NULL (unknown, never 0).
func TestEPSBackfill(t *testing.T) {
	s := newTestStore(t)
	b := testBase.Truncate(15 * time.Minute)
	s.now = fixedClock(b.Add(time.Hour))
	s.RecordTelemetry(testConnID, b, frame.Tele{Haus: 2000, Eps: 500, Soc: 50})
	s.Flush()
	if _, err := s.RollUp(); err != nil {
		t.Fatal(err)
	}
	gone := b.Add(-24 * time.Hour).Unix() // a bucket with no raw samples left
	if _, err := s.db.Exec(`INSERT INTO buckets15m(bucket, sol_avg, sol_min, sol_max, sol_wh, haus_avg, haus_min, haus_max, haus_wh,
		grid_avg, grid_min, grid_max, grid_imp_wh, grid_exp_wh, batt_avg, batt_min, batt_max, batt_chg_wh, batt_dis_wh, acport_avg, soc_last)
		VALUES (?, 0,0,0,0, 1000,1000,1000,250, 0,0,0,0,0, 0,0,0,0,0, 0, 50)`, gone); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE buckets15m SET eps_avg = NULL`); err != nil {
		t.Fatal(err)
	}
	if err := s.backfillEPS(); err != nil {
		t.Fatal(err)
	}
	aggs, err := s.HistoryBuckets(b.Add(-48*time.Hour), b.Add(time.Hour))
	if err != nil || len(aggs) != 2 {
		t.Fatalf("buckets %+v %v", aggs, err)
	}
	if aggs[0].Eps.Valid {
		t.Fatalf("a bucket without raw samples: eps %+v, want NULL", aggs[0].Eps)
	}
	if !aggs[1].Eps.Valid || aggs[1].Eps.Int64 != 500 {
		t.Fatalf("backfilled eps %+v", aggs[1].Eps)
	}
}

// TestSocEstimateHistory (D-028, 2026-11-20): the SoC estimate (0.01 %)
// is stored on the sample it was computed for — after the row, through
// the same ordered queue — and the history carries each window's last
// one: the 15-minute rollups and the raw minutes. A window without an
// estimate (no capacity yet, or rows stored before it) has none.
func TestSocEstimateHistory(t *testing.T) {
	s := newTestStore(t)
	b := testBase.Truncate(15 * time.Minute)
	s.now = fixedClock(b.Add(time.Hour))
	for i, est := range []int{6712, 6705, 6698} {
		ts := b.Add(time.Duration(i) * 5 * time.Second)
		s.RecordTelemetry(testConnID, ts, frame.Tele{Soc: 68})
		s.RecordSocEstimate(testConnID, ts, est)
	}
	s.RecordTelemetry(testConnID, b.Add(20*time.Minute), frame.Tele{Soc: 67}) // no estimate
	s.Flush()
	if _, err := s.RollUp(); err != nil {
		t.Fatal(err)
	}
	aggs, err := s.HistoryBuckets(b, b.Add(time.Hour))
	if err != nil || len(aggs) != 2 {
		t.Fatalf("buckets %+v %v", aggs, err)
	}
	if a := aggs[0]; !a.SocEst.Valid || a.SocEst.Int64 != 6698 {
		t.Fatalf("bucket estimate: %+v", a.SocEst)
	}
	if a := aggs[1]; a.SocEst.Valid {
		t.Fatalf("no estimate in the second bucket: %+v", a.SocEst)
	}
	raw, err := s.HistoryRaw(b, b.Add(30*time.Minute))
	if err != nil || len(raw) != 2 {
		t.Fatalf("raw %+v %v", raw, err)
	}
	if !raw[0].SocEst.Valid || raw[0].SocEst.Int64 != 6698 || raw[1].SocEst.Valid {
		t.Fatalf("raw estimates: %+v %+v", raw[0].SocEst, raw[1].SocEst)
	}
}
