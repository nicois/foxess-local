package api

import (
	"database/sql"
	"fmt"
	"net/http"
	"time"

	"foxesslocal/internal/store"
)

// historyKind selects the range's data source (spec §4 table).
type historyKind string

const (
	kindRaw     historyKind = "raw"     // raw telemetry, 1-min windows
	kindRawStep historyKind = "rawstep" // raw 1-min windows folded to the step
	kindBuckets historyKind = "buckets" // stored 15-min rollups
	kindHourly  historyKind = "hourly"  // 15-min rollups folded to the hour
)

// historyRange is one accepted range value.
type historyRange struct {
	dur     time.Duration
	kind    historyKind
	step    string // response step label
	stepDur time.Duration
}

// label15m is the 15-minute label shared by the history step and the
// average window.
const label15m = "15m"

// historyRanges: the phone ranges (1h, 6h, 24h, 72h) stay near 60-100
// points; 7d and 30d are for wide screens.
var historyRanges = map[string]historyRange{
	"1h":  {time.Hour, kindRaw, "1m", time.Minute},
	"6h":  {6 * time.Hour, kindRawStep, "5m", 5 * time.Minute},
	"24h": {24 * time.Hour, kindBuckets, label15m, 15 * time.Minute},
	"72h": {72 * time.Hour, kindHourly, "1h", time.Hour},
	"7d":  {7 * 24 * time.Hour, kindBuckets, label15m, 15 * time.Minute},
	"30d": {30 * 24 * time.Hour, kindHourly, "1h", time.Hour},
}

// historyRangeNames lists the accepted ranges (the 400 message).
const historyRangeNames = "1h, 6h, 24h, 72h, 7d, 30d"

// historyBody is the GET /history response (spec
// 2026-11-12-dashboard-history-design.md §4).
type historyBody struct {
	Range string  `json:"range"`
	Step  string  `json:"step"`
	TS    []int64 `json:"ts"`
	Sol   []*int  `json:"sol"`
	Grid  []*int  `json:"grid"`
	Haus  []*int  `json:"haus"`
	Batt  []*int  `json:"batt"`
	Soc   []*int  `json:"soc"`
	// SocEst is the SoC estimate (%, two decimals; D-028) of the step's
	// last sample that has one; null where none was stored.
	SocEst []*float64 `json:"soc_est"`
	// Temperatures (°C) and the BMS charge-current limit (A), averaged
	// per step; null where no sample carried them (the register poll
	// does; the cloud-era 150 B frame does not).
	TBatt  []*float64 `json:"t_batt"`
	TInv   []*float64 `json:"t_inv"`
	TAmb   []*float64 `json:"t_amb"`
	BMSChg []*float64 `json:"bms_chg_a"`
	// The BMS's lowest / highest cell temperature (°C) — the lowest is
	// what the BMS derives the charge limit from — from the ~5-minute
	// battery snapshot.
	TCellMin []*float64 `json:"t_cell_min"`
	TCellMax []*float64 `json:"t_cell_max"`
	// Eps is the EPS (backup) port output, W: the part of haus the
	// backup circuits drew (haus includes it). Null where unknown — a
	// step stored before buckets kept it whose raw samples are gone.
	Eps []*int `json:"eps"`
	// Meter2 is the second CT (meter 2), W, − = into the site; null
	// where no sample carried it.
	Meter2 []*int        `json:"meter2"`
	Totals historyTotals `json:"totals"`
}

// tenths converts a stored tenth-unit average to the unit (null when
// absent).
func tenths(v sql.NullInt64) *float64 {
	if !v.Valid {
		return nil
	}
	f := float64(v.Int64) / tenthsPerUnit
	return &f
}

// intOr is *p, or 0 for nil.
func intOr(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// nullInt is a nullable stored integer as a JSON value (null when absent).
func nullInt(v sql.NullInt64) *int {
	if !v.Valid {
		return nil
	}
	n := int(v.Int64)
	return &n
}

// centiPct converts a stored 0.01 % value to % (null when absent).
func centiPct(v sql.NullInt64) *float64 {
	if !v.Valid {
		return nil
	}
	f := float64(v.Int64) / hundredthsPerPct
	return &f
}

// hundredthsPerPct: the SoC estimate is stored in 0.01 %.
const hundredthsPerPct = 100

// tenthsPerUnit: temperatures are stored in 0.1 °C, the charge limit in
// 0.1 A.
const tenthsPerUnit = 10

type historyTotals struct {
	SolWh            int  `json:"sol_wh"`
	HausWh           int  `json:"haus_wh"`
	GridImportWh     int  `json:"grid_import_wh"`
	GridExportWh     int  `json:"grid_export_wh"`
	BattChargedWh    int  `json:"batt_charged_wh"`
	BattDischargedWh int  `json:"batt_discharged_wh"`
	StoredDeltaWh    *int `json:"stored_delta_wh"` // null: the battery capacity is not set
	// Meter2GenWh is the energy the second CT brought into the site
	// (its − side); null when no step carried it.
	Meter2GenWh *int `json:"meter2_gen_wh"`
}

// handleHistory is GET /history (spec §4): bucketed series over a fixed
// range. Read-only role.
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request, _ Token) {
	rv, ok := historyRanges[r.URL.Query().Get("range")]
	if !ok {
		writeProblem(w, http.StatusBadRequest, badField("range", "must be one of "+historyRangeNames))
		return
	}
	now := s.clock()
	until := now.Truncate(rv.stepDur)
	if rv.kind != kindRaw {
		// The bucketed ranges end on the current, in-progress period
		// (live aggregate — HistoryBucketsLive), so they reach "now"
		// instead of lagging up to one step behind.
		until = until.Add(rv.stepDur)
	}
	effSince := until.Add(-rv.dur)

	aggs, firstSoc, lastSoc, err := s.historyAggs(rv, effSince, until, now)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, buildHistory(r.URL.Query().Get("range"), rv, effSince, until, aggs, firstSoc, lastSoc, s.info.capacityWh()))
}

// historyAggs loads the aggregates for the range plus the SoC span over
// the present data (firstSoc, lastSoc — the spec's stored_delta_wh
// endpoints). 30d folds 15-min buckets into hourly rows in Go.
func (s *Server) historyAggs(rv historyRange, since, until, now time.Time) ([]store.Agg, int, int, error) {
	switch rv.kind {
	case kindRaw:
		aggs, err := s.st.HistoryRaw(since, until)
		first, last := socSpan(aggs)
		return aggs, first, last, err
	case kindBuckets:
		aggs, err := s.st.HistoryBucketsLive(since, now)
		first, last := socSpan(aggs)
		return aggs, first, last, err
	case kindRawStep:
		aggs, err := s.st.HistoryRaw(since, until)
		first, last := socSpan(aggs)
		return foldAggs(aggs, rv.stepDur), first, last, err
	case kindHourly:
		bs, err := s.st.HistoryBucketsLive(since, now)
		first, last := socSpan(bs) // buckets are time-ordered
		return foldAggs(bs, time.Hour), first, last, err
	}
	return nil, 0, 0, fmt.Errorf("api: unknown history kind %q", rv.kind)
}

// socSpan is the SoC span over present windows: the first and last
// window's soc_last (the oldest sample's SoC inside a window is not
// stored; the window's own soc_last is the nearest available value).
func socSpan(aggs []store.Agg) (first, last int) {
	if len(aggs) == 0 {
		return 0, 0
	}
	return aggs[0].SocLast, aggs[len(aggs)-1].SocLast
}

// foldAggs folds time-ordered windows into step-long rows: avg = mean
// of the present windows' avgs, min/max = extremes, wh = sum, soc = last
// present (the 15-min rollups to hours; raw minutes to 5 minutes).
func foldAggs(in []store.Agg, step time.Duration) []store.Agg {
	sec := int64(step / time.Second)
	byStep := map[int64]*hourAcc{}
	var order []int64
	for _, b := range in {
		k := (b.TS.Unix() / sec) * sec
		c, ok := byStep[k]
		if !ok {
			// Seed the row from the first window (its min/max/wh/soc
			// are already the row's values for one window).
			c = &hourAcc{a: b, n: 1}
			c.sol, c.haus, c.grid, c.batt = b.SolAvg, b.HausAvg, b.GridAvg, b.BattAvg
			c.addTemps(b)
			byStep[k] = c
			order = append(order, k)
			continue
		}
		c.add(b)
	}
	out := make([]store.Agg, 0, len(order))
	for _, k := range order {
		c := byStep[k]
		c.a.TS = time.Unix(k, 0).UTC()
		c.a.SolAvg = c.sol / c.n
		c.a.HausAvg = c.haus / c.n
		c.a.GridAvg = c.grid / c.n
		c.a.BattAvg = c.batt / c.n
		c.a.TBatt, c.a.TInv, c.a.TAmb, c.a.BMSChg = c.temps[0].mean(), c.temps[1].mean(), c.temps[2].mean(), c.temps[3].mean()
		c.a.TCellMin, c.a.TCellMax, c.a.Eps = c.temps[4].mean(), c.temps[5].mean(), c.temps[6].mean()
		c.a.Meter2, c.a.Meter2GenWh = c.temps[7].mean(), c.m2Wh.total()
		out = append(out, c.a)
	}
	return out
}

// hourAcc accumulates one folded row from the present windows.
type hourAcc struct {
	a                     store.Agg
	n                     int
	sol, haus, grid, batt int // sums of present 15-min avgs
	// battery, inverter, ambient temperature, BMS charge limit, the
	// lowest / highest cell temperature and the EPS port: the mean of the
	// windows that carried them, and meter 2
	temps [8]nullMean
	m2Wh  nullMean // meter 2's energy in: summed
}

// nullMean averages the present values of a nullable series.
type nullMean struct {
	sum int64
	n   int64
}

func (m *nullMean) add(v sql.NullInt64) {
	if v.Valid {
		m.sum += v.Int64
		m.n++
	}
}

// total is the present values' sum (not Valid when none was present).
func (m *nullMean) total() sql.NullInt64 {
	if m.n == 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: m.sum, Valid: true}
}

func (m *nullMean) mean() sql.NullInt64 {
	if m.n == 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: m.sum / m.n, Valid: true}
}

// addTemps folds one window's nullable averages (temperatures, EPS).
func (c *hourAcc) addTemps(b store.Agg) {
	for i, v := range [8]sql.NullInt64{b.TBatt, b.TInv, b.TAmb, b.BMSChg, b.TCellMin, b.TCellMax, b.Eps, b.Meter2} {
		c.temps[i].add(v)
	}
	c.m2Wh.add(b.Meter2GenWh)
}

// add folds a later bucket into a seeded row: sums for the means,
// extremes for min/max, Wh sums, soc_last last-wins (buckets are
// time-ordered).
func (c *hourAcc) add(b store.Agg) {
	c.n++
	c.sol += b.SolAvg
	c.haus += b.HausAvg
	c.grid += b.GridAvg
	c.batt += b.BattAvg
	c.a.SolMin = min(c.a.SolMin, b.SolMin)
	c.a.SolMax = max(c.a.SolMax, b.SolMax)
	c.a.HausMin = min(c.a.HausMin, b.HausMin)
	c.a.HausMax = max(c.a.HausMax, b.HausMax)
	c.a.SolWh += b.SolWh
	c.a.HausWh += b.HausWh
	c.a.GridImpWh += b.GridImpWh
	c.a.GridExpWh += b.GridExpWh
	c.a.BattChgWh += b.BattChgWh
	c.a.BattDisWh += b.BattDisWh
	c.a.SocLast = b.SocLast
	if b.SocEst.Valid {
		c.a.SocEst = b.SocEst
	}
	c.addTemps(b)
}

// buildHistory maps the (sparse) aggregates onto the dense response grid:
// every step slot is present; missing data is null. An entirely empty range
// returns literally empty arrays (spec §4). firstSoc/lastSoc are the SoC
// span over the present data (spec §4 stored_delta_wh endpoints).
func buildHistory(rangeLabel string, rv historyRange, since, until time.Time, aggs []store.Agg, firstSoc, lastSoc, capacityWh int) historyBody {
	n := int(until.Sub(since) / rv.stepDur)
	if len(aggs) == 0 {
		n = 0 // an empty range: literally empty arrays, zero totals
	}
	body := historyBody{
		Range:    rangeLabel,
		Step:     rv.step,
		TS:       make([]int64, n),
		Sol:      make([]*int, n),
		Grid:     make([]*int, n),
		Haus:     make([]*int, n),
		Batt:     make([]*int, n),
		Soc:      make([]*int, n),
		SocEst:   make([]*float64, n),
		TBatt:    make([]*float64, n),
		TInv:     make([]*float64, n),
		TAmb:     make([]*float64, n),
		BMSChg:   make([]*float64, n),
		TCellMin: make([]*float64, n),
		TCellMax: make([]*float64, n),
		Eps:      make([]*int, n),
		Meter2:   make([]*int, n),
	}
	bySlot := make(map[int64]int, len(aggs))
	for i, a := range aggs {
		bySlot[a.TS.Unix()] = i
	}
	for i := range n {
		body.TS[i] = since.Unix() + int64(i)*int64(rv.stepDur/time.Second)
		j, ok := bySlot[body.TS[i]]
		if !ok {
			continue
		}
		a := aggs[j]
		body.Sol[i] = &a.SolAvg
		body.Grid[i] = &a.GridAvg
		body.Haus[i] = &a.HausAvg
		body.Batt[i] = &a.BattAvg
		body.Soc[i] = &a.SocLast
		body.SocEst[i] = centiPct(a.SocEst)
		body.TBatt[i], body.TInv[i], body.TAmb[i], body.BMSChg[i] = tenths(a.TBatt), tenths(a.TInv), tenths(a.TAmb), tenths(a.BMSChg)
		body.TCellMin[i], body.TCellMax[i] = tenths(a.TCellMin), tenths(a.TCellMax)
		body.Eps[i] = nullInt(a.Eps)
		body.Meter2[i] = nullInt(a.Meter2)
	}
	if len(aggs) == 0 {
		return body
	}
	t := historyTotals{}
	for _, a := range aggs {
		t.SolWh += a.SolWh
		t.HausWh += a.HausWh
		t.GridImportWh += a.GridImpWh
		t.GridExportWh += a.GridExpWh
		t.BattChargedWh += a.BattChgWh
		t.BattDischargedWh += a.BattDisWh
		if a.Meter2GenWh.Valid {
			t.Meter2GenWh = new(intOr(t.Meter2GenWh) + int(a.Meter2GenWh.Int64))
		}
	}
	if capacityWh > 0 {
		d := (lastSoc - firstSoc) * capacityWh / 100
		t.StoredDeltaWh = &d
	}
	body.Totals = t
	return body
}
