package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// bucketLen is the rollup period (15 minutes, UTC-aligned).
const bucketLen = 15 * time.Minute

// Agg is one rollup row: per-series stats of all samples in
// [TS, TS+bucketLen). Wh values use a fixed 5 s per sample (the device's
// telemetry cadence — a window with missing samples overstates Wh by at
// most the missing fraction).
type Agg struct {
	TS time.Time
	// per series: avg/min/max W and energy Wh.
	SolAvg, SolMin, SolMax, SolWh     int
	HausAvg, HausMin, HausMax, HausWh int
	// grid: signed avg; Wh split by direction.
	GridAvg, GridMin, GridMax, GridImpWh, GridExpWh int
	// batt: signed avg; Wh split by direction.
	BattAvg, BattMin, BattMax, BattChgWh, BattDisWh int

	AcportAvg int
	EpsAvg    int // EPS port output (windows only: not stored in the rollups)
	// temperatures (0.1 °C) and the BMS charge limit (0.1 A), averaged;
	// not Valid when no sample in the window carried them
	TBatt, TInv, TAmb, BMSChg sql.NullInt64
	// the BMS's lowest / highest cell temperature (0.1 °C), averaged
	TCellMin, TCellMax sql.NullInt64
	// Eps is the EPS (backup) port output, averaged: the share of the
	// household load (HausAvg includes it) that the backup circuits drew.
	// Not Valid for a bucket stored before buckets kept it (schema v12)
	// whose raw samples are gone.
	Eps     sql.NullInt64
	SocLast int // SoC % of the newest sample in the window
	N       int // samples in the window (windowAgg only; not stored)
}

// aggSQL aggregates one window (all columns except SocLast). CAST is
// required: SQLite AVG() returns a float, the driver refuses float→int.
// COALESCE: every aggregate (except COUNT) is NULL on an empty window and
// the driver refuses NULL→int — the n==0 check discards the zero rows.
const aggSQL = `
SELECT COUNT(*),
  COALESCE(CAST(AVG(sol) AS INTEGER), 0), COALESCE(MIN(sol), 0), COALESCE(MAX(sol), 0), COALESCE(SUM(sol) * 5 / 3600, 0),
  COALESCE(CAST(AVG(haus) AS INTEGER), 0), COALESCE(MIN(haus), 0), COALESCE(MAX(haus), 0), COALESCE(SUM(haus) * 5 / 3600, 0),
  COALESCE(CAST(AVG(grid) AS INTEGER), 0), COALESCE(MIN(grid), 0), COALESCE(MAX(grid), 0),
  COALESCE(SUM(CASE WHEN grid > 0 THEN grid ELSE 0 END) * 5 / 3600, 0),
  COALESCE(SUM(CASE WHEN grid < 0 THEN -grid ELSE 0 END) * 5 / 3600, 0),
  COALESCE(CAST(AVG(batt) AS INTEGER), 0), COALESCE(MIN(batt), 0), COALESCE(MAX(batt), 0),
  COALESCE(SUM(CASE WHEN batt > 0 THEN batt ELSE 0 END) * 5 / 3600, 0),
  COALESCE(SUM(CASE WHEN batt < 0 THEN -batt ELSE 0 END) * 5 / 3600, 0),
  COALESCE(CAST(AVG(acport) AS INTEGER), 0),
  COALESCE(CAST(AVG(eps) AS INTEGER), 0),
  CAST(AVG(t_batt) AS INTEGER), CAST(AVG(t_inv) AS INTEGER), CAST(AVG(t_amb) AS INTEGER),
  CAST(AVG(bms_chg) AS INTEGER),
  CAST(AVG(t_cell_min) AS INTEGER), CAST(AVG(t_cell_max) AS INTEGER),
  CAST(AVG(eps) AS INTEGER)
FROM telemetry WHERE ts >= ? AND ts < ?`

// socWindowSQL returns the SoC of the latest-arriving sample in the
// window (rowid = arrival order on the single-writer queue). Arrival, not
// sample ts, is the self-heal order: a frame that lands in an already
// rolled bucket after the rollup must update SocLast even when its ts is
// older than the window's newest sample.
const socWindowSQL = `
SELECT soc FROM telemetry WHERE ts >= ? AND ts < ? ORDER BY rowid DESC LIMIT 1`

const upsertSQL = `
INSERT INTO buckets15m(bucket, sol_avg, sol_min, sol_max, sol_wh,
  haus_avg, haus_min, haus_max, haus_wh,
  grid_avg, grid_min, grid_max, grid_imp_wh, grid_exp_wh,
  batt_avg, batt_min, batt_max, batt_chg_wh, batt_dis_wh,
  acport_avg, soc_last, t_batt_avg, t_inv_avg, t_amb_avg, bms_chg_avg,
  t_cell_min_avg, t_cell_max_avg, eps_avg)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(bucket) DO UPDATE SET
  eps_avg = excluded.eps_avg,
  t_cell_min_avg = excluded.t_cell_min_avg, t_cell_max_avg = excluded.t_cell_max_avg,
  t_batt_avg = excluded.t_batt_avg, t_inv_avg = excluded.t_inv_avg, t_amb_avg = excluded.t_amb_avg,
  bms_chg_avg = excluded.bms_chg_avg,
  sol_avg = excluded.sol_avg, sol_min = excluded.sol_min, sol_max = excluded.sol_max, sol_wh = excluded.sol_wh,
  haus_avg = excluded.haus_avg, haus_min = excluded.haus_min, haus_max = excluded.haus_max, haus_wh = excluded.haus_wh,
  grid_avg = excluded.grid_avg, grid_min = excluded.grid_min, grid_max = excluded.grid_max,
  grid_imp_wh = excluded.grid_imp_wh, grid_exp_wh = excluded.grid_exp_wh,
  batt_avg = excluded.batt_avg, batt_min = excluded.batt_min, batt_max = excluded.batt_max,
  batt_chg_wh = excluded.batt_chg_wh, batt_dis_wh = excluded.batt_dis_wh,
  acport_avg = excluded.acport_avg, soc_last = excluded.soc_last`

// windowAgg aggregates [from, to); ok is false for an empty window.
func (s *Store) windowAgg(from, to time.Time) (Agg, bool, error) {
	row := s.rdb.QueryRow(aggSQL, encTS(from), encTS(to))
	var a Agg
	var n int
	if err := row.Scan(append([]any{&n}, aggCols(&a, &a.EpsAvg)...)...); err != nil {
		return Agg{}, false, fmt.Errorf("store: window agg: %w", err)
	}
	if n == 0 {
		return Agg{}, false, nil
	}
	var soc int
	if err := s.rdb.QueryRow(socWindowSQL, encTS(from), encTS(to)).Scan(&soc); err != nil {
		return Agg{}, false, fmt.Errorf("store: window soc: %w", err)
	}
	a.TS = from
	a.SocLast = soc
	a.N = n
	return a, true, nil
}

// WindowAverage is the mean of every raw telemetry sample in [from, to)
// (signed for grid/battery: the net flow), with the sample count in N;
// ok is false for an empty window.
func (s *Store) WindowAverage(from, to time.Time) (Agg, bool, error) {
	s.catchUp()
	return s.windowAgg(from, to)
}

// aggCols are the scan destinations of an aggregate row's series columns;
// mid is the column between acport and the temperatures (eps for a
// window, soc_last for a stored bucket).
func aggCols(a *Agg, mid *int) []any {
	return []any{
		&a.SolAvg, &a.SolMin, &a.SolMax, &a.SolWh,
		&a.HausAvg, &a.HausMin, &a.HausMax, &a.HausWh,
		&a.GridAvg, &a.GridMin, &a.GridMax, &a.GridImpWh, &a.GridExpWh,
		&a.BattAvg, &a.BattMin, &a.BattMax, &a.BattChgWh, &a.BattDisWh,
		&a.AcportAvg, mid, &a.TBatt, &a.TInv, &a.TAmb, &a.BMSChg,
		&a.TCellMin, &a.TCellMax, &a.Eps,
	}
}

// upsertArgs are upsertSQL's arguments for one bucket.
func upsertArgs(bucket int64, a Agg) []any {
	return []any{
		bucket,
		a.SolAvg, a.SolMin, a.SolMax, a.SolWh,
		a.HausAvg, a.HausMin, a.HausMax, a.HausWh,
		a.GridAvg, a.GridMin, a.GridMax, a.GridImpWh, a.GridExpWh,
		a.BattAvg, a.BattMin, a.BattMax, a.BattChgWh, a.BattDisWh,
		a.AcportAvg, a.SocLast, a.TBatt, a.TInv, a.TAmb, a.BMSChg, a.TCellMin, a.TCellMax, a.Eps,
	}
}

// lastBucket is the rollup watermark (0 when no buckets exist).
func (s *Store) lastBucket() (int64, error) {
	var last sql.NullInt64
	if err := s.db.QueryRow("SELECT MAX(bucket) FROM buckets15m").Scan(&last); err != nil {
		return 0, fmt.Errorf("store: last bucket: %w", err)
	}
	return last.Int64, nil
}

// firstBucket anchors the rollup start: the watermark when buckets
// exist, else the bucket of MIN(ts) — the first run never loops from
// epoch. ok is false when telemetry is empty (nothing to roll up).
func (s *Store) firstBucket(last int64) (time.Time, bool, error) {
	if last != 0 {
		return time.Unix(last, 0).UTC(), true, nil
	}
	var minTS sql.NullString
	if err := s.db.QueryRow("SELECT MIN(ts) FROM telemetry").Scan(&minTS); err != nil {
		return time.Time{}, false, fmt.Errorf("store: rollup: %w", err)
	}
	if !minTS.Valid || minTS.String == "" {
		return time.Time{}, false, nil // empty table (NULL) or sentinel, nothing to roll up
	}
	t, err := decTS(minTS.String)
	if err != nil {
		return time.Time{}, false, err
	}
	return t.Truncate(bucketLen), true, nil
}

// RollUp upserts every completed 15-min bucket in (watermark, now-15m],
// plus the watermark bucket itself — a bucket rolled up while samples
// were still in flight is self-healed by the re-cover, and older buckets
// are never re-read (the channel has no retransmission). Idempotent.
func (s *Store) RollUp() (int, error) {
	s.catchUp()
	end := s.now().Truncate(bucketLen)
	last, err := s.lastBucket()
	if err != nil {
		return 0, err
	}
	first, ok, err := s.firstBucket(last)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, nil // nothing to roll up
	}
	n := 0
	for b := first; b.Before(end); b = b.Add(bucketLen) {
		a, ok, err := s.windowAgg(b, b.Add(bucketLen))
		if err != nil {
			return n, err
		}
		if !ok {
			continue
		}
		if _, err := s.db.Exec(upsertSQL, upsertArgs(b.Unix(), a)...); err != nil {
			return n, fmt.Errorf("store: rollup: %w", err)
		}
		n++
	}
	return n, nil
}

// HistoryBuckets returns rollup buckets with since <= bucket < until.
func (s *Store) HistoryBuckets(since, until time.Time) ([]Agg, error) {
	rows, err := s.rdb.Query(`
SELECT bucket, sol_avg, sol_min, sol_max, sol_wh,
  haus_avg, haus_min, haus_max, haus_wh,
  grid_avg, grid_min, grid_max, grid_imp_wh, grid_exp_wh,
  batt_avg, batt_min, batt_max, batt_chg_wh, batt_dis_wh,
  acport_avg, soc_last, t_batt_avg, t_inv_avg, t_amb_avg, bms_chg_avg,
  t_cell_min_avg, t_cell_max_avg, eps_avg
FROM buckets15m WHERE bucket >= ? AND bucket < ? ORDER BY bucket`,
		since.Unix(), until.Unix())
	if err != nil {
		return nil, fmt.Errorf("store: history buckets: %w", err)
	}
	out := []Agg{}
	for rows.Next() {
		var a Agg
		var bucket int64
		if err := rows.Scan(append([]any{&bucket}, aggCols(&a, &a.SocLast)...)...); err != nil {
			return nil, closeRows(rows, fmt.Errorf("store: history buckets: %w", err))
		}
		a.TS = time.Unix(bucket, 0).UTC()
		out = append(out, a)
	}
	return out, closeRows(rows, rows.Err())
}

// HistoryBucketsLive is HistoryBuckets over [since, now) extended to
// "now": the stored rollups plus live aggregates of every 15-min window
// after the rollup watermark — the in-progress bucket (truncated at now)
// and any completed bucket the periodic RollUp has not stored yet. With
// no rollup at all it starts at the bucket of MIN(ts), never at since
// (a 7-day range must not scan 672 empty windows).
func (s *Store) HistoryBucketsLive(since, now time.Time) ([]Agg, error) {
	s.catchUp()
	out, err := s.HistoryBuckets(since, now)
	if err != nil {
		return nil, err
	}
	last, err := s.lastBucket()
	if err != nil {
		return nil, err
	}
	start, ok, err := s.firstBucket(last)
	if err != nil || !ok {
		return out, err
	}
	if last != 0 {
		start = start.Add(bucketLen) // the watermark bucket is stored
	}
	if lo := since.Truncate(bucketLen); start.Before(lo) {
		start = lo
	}
	for b := start; b.Before(now); b = b.Add(bucketLen) {
		end := b.Add(bucketLen)
		if end.After(now) {
			end = now
		}
		a, ok, err := s.windowAgg(b, end)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, a)
		}
	}
	return out, nil
}

// HistoryRaw aggregates raw telemetry into 1-minute buckets [since, until)
// (the 1-hour range path).
func (s *Store) HistoryRaw(since, until time.Time) ([]Agg, error) {
	s.catchUp()
	from := since.UTC().Truncate(time.Minute)
	to := until.UTC().Truncate(time.Minute)
	if to.Before(until) {
		to = to.Add(time.Minute) // the last window started before until runs whole
	}
	// one grouped query for every minute (not two queries per minute:
	// the store has one connection, so 120 queries queued behind others)
	soc, err := s.lastSocByMinute(from, to)
	if err != nil {
		return nil, err
	}
	rows, err := s.rdb.Query(aggByMinuteSQL, encTS(from), encTS(to))
	if err != nil {
		return nil, fmt.Errorf("store: raw history: %w", err)
	}
	out := []Agg{}
	for rows.Next() {
		var (
			m string
			a Agg
		)
		if err := rows.Scan(append([]any{&m, &a.N}, aggCols(&a, &a.EpsAvg)...)...); err != nil {
			return nil, closeRows(rows, err)
		}
		if a.TS, err = time.Parse(minuteLayout, m); err != nil {
			return nil, closeRows(rows, err)
		}
		a.SocLast = soc[m]
		out = append(out, a)
	}
	return out, closeRows(rows, rows.Err())
}

// minuteLayout is the stored ts prefix naming its UTC minute.
const minuteLayout = "2006-01-02T15:04"

// aggByMinuteSQL is aggSQL grouped by UTC minute (the ts text prefix).
//
//nolint:gosec // G202: built once from two constants; every value is a bound arg.
var aggByMinuteSQL = "SELECT substr(ts, 1, 16) AS m, " +
	strings.TrimPrefix(strings.TrimSpace(aggSQL), "SELECT ") + " GROUP BY m ORDER BY m"

// lastSocByMinute is each minute's SoC of its last-arriving sample
// (rowid = arrival order, as socWindowSQL).
func (s *Store) lastSocByMinute(from, to time.Time) (map[string]int, error) {
	rows, err := s.rdb.Query(`SELECT substr(ts, 1, 16), soc FROM telemetry
		WHERE ts >= ? AND ts < ? ORDER BY rowid`, encTS(from), encTS(to))
	if err != nil {
		return nil, fmt.Errorf("store: raw history soc: %w", err)
	}
	out := map[string]int{}
	for rows.Next() {
		var (
			m   string
			soc int
		)
		if err := rows.Scan(&m, &soc); err != nil {
			return nil, closeRows(rows, err)
		}
		out[m] = soc
	}
	return out, closeRows(rows, rows.Err())
}
