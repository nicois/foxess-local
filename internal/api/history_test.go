package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"foxesslocal/internal/frame"
)

// seedHistBucket seeds a full 15-min bucket (180 rows at 5 s cadence) and
// rolls it up (the bucket paths read buckets15m, not raw telemetry).
func seedHistBucket(t *testing.T, h *harness, start time.Time, sol, grid, haus, batt int32, soc byte) {
	t.Helper()
	for i := range 180 {
		h.seedTele(start.Add(time.Duration(i)*5*time.Second), sol, grid, haus, 0, batt, soc, 2)
	}
	if _, err := h.st.RollUp(); err != nil {
		t.Fatalf("RollUp: %v", err)
	}
}

func decodeHist(t *testing.T, rec *httptest.ResponseRecorder) historyBody {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("history: status %d, body %s", rec.Code, rec.Body.String())
	}
	var b historyBody
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatalf("history body: %v", err)
	}
	return b
}

func TestHistoryRaw1h(t *testing.T) {
	h := newHarness(t)
	// 70 minutes of data at 1/min (Wh uses the fixed 5 s/sample convention;
	// this test asserts shape and presence, not absolute Wh).
	for i := range 70 {
		h.seedTele(testBase.Add(-time.Duration(70-i)*time.Minute), 1000, -200, 300, 0, 50, 60, 2)
	}
	b := decodeHist(t, h.do(t, "GET", "/api/v1/history?range=1h", RoleReadOnly, nil))
	if b.Range != "1h" || b.Step != "1m" {
		t.Fatalf("range %q step %q", b.Range, b.Step)
	}
	if len(b.TS) != 60 || len(b.Sol) != 60 {
		t.Fatalf("grid: ts %d sol %d, want 60/60", len(b.TS), len(b.Sol))
	}
	nonNull := 0
	for _, v := range b.Sol {
		if v != nil {
			nonNull++
		}
	}
	if nonNull != 60 {
		t.Fatalf("sol non-null = %d, want 60 (all seeded)", nonNull)
	}
	// no battery capacity set: the stored delta is unknown, not 0
	if b.Totals.StoredDeltaWh != nil {
		t.Fatalf("stored_delta_wh = %d, want null (capacity unknown)", *b.Totals.StoredDeltaWh)
	}
}

func TestHistory24hBucketsAndGapNulls(t *testing.T) {
	h := newHarness(t)
	// One bucket 3 h ago; the rest of the 24 h window is a gap.
	seedHistBucket(t, h, testBase.Add(-3*time.Hour), 1000, -500, 600, 200, 61)
	b := decodeHist(t, h.do(t, "GET", "/api/v1/history?range=24h", RoleReadOnly, nil))
	if b.Step != "15m" || len(b.TS) != 96 {
		t.Fatalf("step %q grid %d, want 15m/96", b.Step, len(b.TS))
	}
	filled := 0
	var solWh, gridExpWh int
	for i, v := range b.Sol {
		if v == nil {
			continue
		}
		filled++
		if *v != 1000 {
			t.Fatalf("sol[%d] = %d, want 1000", i, *v)
		}
		solWh = b.Totals.SolWh
	}
	if filled != 1 {
		t.Fatalf("sol non-null = %d, want 1 (single bucket)", filled)
	}
	if solWh != 1000*900/3600 {
		t.Fatalf("sol_wh = %d, want %d", solWh, 1000*900/3600)
	}
	gridExpWh = b.Totals.GridExportWh
	if gridExpWh != 500*900/3600 {
		t.Fatalf("grid_export_wh = %d, want %d", gridExpWh, 500*900/3600)
	}
	// no battery capacity set: the stored delta is unknown
	if b.Totals.StoredDeltaWh != nil {
		t.Fatalf("stored_delta_wh = %d, want null", *b.Totals.StoredDeltaWh)
	}
}

func TestHistoryEmptyStore(t *testing.T) {
	h := newHarness(t)
	b := decodeHist(t, h.do(t, "GET", "/api/v1/history?range=1h", RoleReadOnly, nil))
	if len(b.TS) != 0 || len(b.Sol) != 0 || len(b.Soc) != 0 {
		t.Fatalf("empty store must return literally empty arrays: ts %d sol %d soc %d", len(b.TS), len(b.Sol), len(b.Soc))
	}
	if b.Totals.SolWh != 0 || b.Totals.StoredDeltaWh != nil {
		t.Fatalf("totals must be zero: %+v", b.Totals)
	}
}

func TestHistory30dHourly(t *testing.T) {
	h := newHarness(t)
	// Two 15-min buckets in different hours, 10 days ago.
	seedHistBucket(t, h, testBase.Add(-10*24*time.Hour), 1000, 0, 0, 0, 60)
	seedHistBucket(t, h, testBase.Add(-10*24*time.Hour+15*time.Minute), 2000, 0, 0, 0, 61)
	h.srv.info.BatteryCapacityWh = 41930
	b := decodeHist(t, h.do(t, "GET", "/api/v1/history?range=30d", RoleReadOnly, nil))
	if b.Step != "1h" || len(b.TS) != 720 {
		t.Fatalf("step %q grid %d, want 1h/720", b.Step, len(b.TS))
	}
	// Both buckets are in the same hour → one hourly row: avg = (1000+2000)/2 = 1500.
	var got *int
	for _, v := range b.Sol {
		if v != nil {
			if got != nil {
				t.Fatalf("expected exactly one non-null sol hour, got more")
			}
			got = v
		}
	}
	if got == nil || *got != 1500 {
		t.Fatalf("hourly sol = %v, want 1500", got)
	}
	if b.Totals.SolWh != (1000+2000)*900/3600 {
		t.Fatalf("sol_wh = %d, want %d", b.Totals.SolWh, (1000+2000)*900/3600)
	}
	// stored delta: 60 → 61 over the present data.
	if b.Totals.StoredDeltaWh == nil || *b.Totals.StoredDeltaWh != 41930/100 {
		t.Fatalf("stored_delta_wh = %v, want %d (1 %% of the capacity)", b.Totals.StoredDeltaWh, 41930/100)
	}
}

func TestHistoryInvalidRange(t *testing.T) {
	h := newHarness(t)
	checkProblem(t, h.do(t, "GET", "/api/v1/history?range=2d", RoleReadOnly, nil), http.StatusBadRequest, "invalid_value")
}

func TestHistoryAuth(t *testing.T) {
	h := newHarness(t)
	// Harness default: public read-only on — tokenless reads pass
	// (spec §5.5); the 401 path is pinned with the flag off.
	if rec := h.do(t, "GET", "/api/v1/history?range=1h", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("no token (public on): status %d, want 200", rec.Code)
	}
	if rec := h.do(t, "GET", "/api/v1/history?range=1h", RoleReadOnly, nil); rec.Code != http.StatusOK {
		t.Fatalf("read-only: status %d, want 200", rec.Code)
	}
	h.srv.info.PublicReadOnly = false
	if rec := h.do(t, "GET", "/api/v1/history?range=1h", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token (public off): status %d, want 401", rec.Code)
	}
}

// TestHistory24hIncludesInProgressBucket: the long ranges reach "now" —
// the grid's last slot is the current (in-progress) 15-min period and
// carries its live aggregate; a fresh install's first minutes are
// visible instead of "no data" for up to 15 min.
func TestHistory24hIncludesInProgressBucket(t *testing.T) {
	h := newHarness(t)
	h.clk.advance(7 * time.Minute) // mid-bucket
	h.seedTele(testBase.Add(2*time.Minute), 1500, -100, 900, 0, 300, 70, 2)
	b := decodeHist(t, h.do(t, "GET", "/api/v1/history?range=24h", RoleReadOnly, nil))
	if len(b.TS) != 96 {
		t.Fatalf("grid %d, want 96", len(b.TS))
	}
	last := len(b.TS) - 1
	if b.TS[last] != testBase.Unix() {
		t.Fatalf("last slot %d, want the current period %d", b.TS[last], testBase.Unix())
	}
	if b.Sol[last] == nil || *b.Sol[last] != 1500 {
		t.Fatalf("current slot sol %v, want 1500", b.Sol[last])
	}
	if b.Totals.SolWh == 0 {
		t.Fatal("totals ignore the in-progress bucket")
	}
}

// TestHistory6hFiveMinute: 6 h is raw telemetry folded to 5-minute steps
// (72 points: readable on a phone) reaching the in-progress step.
func TestHistory6hFiveMinute(t *testing.T) {
	h := newHarness(t)
	slot := testBase.Add(-2 * time.Hour).Truncate(5 * time.Minute)
	for i := range 5 { // one sample a minute, 1000..1800 W
		h.seedTele(slot.Add(time.Duration(i)*time.Minute), int32(1000+200*i), 0, 0, 0, 0, byte(50+i), 2)
	}
	h.seedTele(testBase.Add(-time.Minute), 700, 0, 0, 0, 0, 60, 2) // the current step
	b := decodeHist(t, h.do(t, "GET", "/api/v1/history?range=6h", RoleReadOnly, nil))
	if b.Range != "6h" || b.Step != "5m" || len(b.TS) != 72 {
		t.Fatalf("range %q step %q grid %d, want 6h/5m/72", b.Range, b.Step, len(b.TS))
	}
	var got []int
	for i, v := range b.Sol {
		if v != nil {
			got = append(got, *v)
			if b.TS[i] == slot.Unix() && (*v != 1400 || *b.Soc[i] != 54) {
				t.Errorf("slot %v: sol %d soc %d, want 1400 / 54", slot, *v, *b.Soc[i])
			}
		}
	}
	if len(got) != 2 || got[1] != 700 {
		t.Fatalf("non-null sol %v, want [1400 700]", got)
	}
}

// TestHistory72hHourly: 72 h is the 15-minute rollups folded to hours
// (72 points).
func TestHistory72hHourly(t *testing.T) {
	h := newHarness(t)
	seedHistBucket(t, h, testBase.Add(-30*time.Hour), 1000, 0, 0, 0, 60)
	b := decodeHist(t, h.do(t, "GET", "/api/v1/history?range=72h", RoleReadOnly, nil))
	if b.Step != "1h" || len(b.TS) != 72 {
		t.Fatalf("step %q grid %d, want 1h/72", b.Step, len(b.TS))
	}
	filled := 0
	for _, v := range b.Sol {
		if v != nil {
			filled++
		}
	}
	if filled != 1 {
		t.Fatalf("non-null hours %d, want 1", filled)
	}
}

// TestHistoryTemperatures: the history carries the battery, inverter and
// ambient temperatures (°C) and the BMS charge limit (A) per step, null
// where no sample carried them — on the per-minute (1 h) and the folded
// hourly (30 d) paths alike.
func TestHistoryTemperatures(t *testing.T) {
	h := newHarness(t)
	at := testBase.Add(-10 * time.Minute)
	p := snap61Payload(50, 600, nil) // the BMS cell temperatures: max 25.3, min 17.5 °C
	p[64], p[65] = 0x18, 0x74
	copy(p[66:], []byte{0xfd, 0x00, 0xaf, 0x00})
	h.seedFrame(at.Add(-time.Minute), dirC2S, classSnap61, decisionForwarded, p, nil)
	for i := range 3 {
		h.st.RecordTelemetry(testConnID, at.Add(time.Duration(i)*5*time.Second), frame.Tele{
			Sol: 100, Soc: 50,
			Temps: &frame.Temps{BattDeciC: 275, InvDeciC: 412, AmbDeciC: 301, BMSChargeDeciA: 450},
		})
	}
	h.st.Flush()
	checkTemps(t, decodeHist(t, h.do(t, "GET", "/api/v1/history?range=1h", RoleReadOnly, nil)))
	if _, err := h.st.RollUp(); err != nil {
		t.Fatal(err)
	}
	checkTemps(t, decodeHist(t, h.do(t, "GET", "/api/v1/history?range=30d", RoleReadOnly, nil)))
}

// checkTemps: exactly one slot carries the seeded values, the rest null,
// every series as long as ts.
func checkTemps(t *testing.T, b historyBody) {
	t.Helper()
	series := map[string][]*float64{
		"t_batt": b.TBatt, "t_inv": b.TInv, "t_amb": b.TAmb, "bms_chg_a": b.BMSChg,
		"t_cell_min": b.TCellMin, "t_cell_max": b.TCellMax,
	}
	want := map[string]float64{"t_batt": 27.5, "t_inv": 41.2, "t_amb": 30.1, "bms_chg_a": 45, "t_cell_min": 17.5, "t_cell_max": 25.3}
	for name, s := range series {
		if len(s) != len(b.TS) {
			t.Fatalf("%s: %d values for %d steps", name, len(s), len(b.TS))
		}
		var got []float64
		for _, v := range s {
			if v != nil {
				got = append(got, *v)
			}
		}
		if len(got) != 1 || got[0] != want[name] {
			t.Errorf("%s %s: %v, want one %v", b.Range, name, got, want[name])
		}
	}
}

// TestHistoryEPS (2026-11-19): the history carries the EPS (backup) port
// output per step beside the household load that includes it, on the
// per-minute (1 h) and folded hourly (30 d) paths alike.
func TestHistoryEPS(t *testing.T) {
	h := newHarness(t)
	at := testBase.Add(-10 * time.Minute)
	for i := range 3 {
		h.st.RecordTelemetry(testConnID, at.Add(time.Duration(i)*5*time.Second), frame.Tele{Haus: 2000, Eps: 700, Soc: 50})
	}
	h.st.Flush()
	check := func(b historyBody) {
		t.Helper()
		if len(b.Eps) != len(b.TS) {
			t.Fatalf("%s: %d eps values for %d steps", b.Range, len(b.Eps), len(b.TS))
		}
		var got []int
		for _, v := range b.Eps {
			if v != nil {
				got = append(got, *v)
			}
		}
		if len(got) != 1 || got[0] != 700 {
			t.Errorf("%s eps: %v, want one 700", b.Range, got)
		}
	}
	check(decodeHist(t, h.do(t, "GET", "/api/v1/history?range=1h", RoleReadOnly, nil)))
	if _, err := h.st.RollUp(); err != nil {
		t.Fatal(err)
	}
	check(decodeHist(t, h.do(t, "GET", "/api/v1/history?range=30d", RoleReadOnly, nil)))
}

// TestHistorySocEstimate (D-028, 2026-11-20): the history carries the SoC
// estimate per step — the last one in the step, in % with two decimals —
// beside the reported whole percent, on the per-minute (1 h) and folded
// hourly (30 d) paths alike; null where none was stored.
func TestHistorySocEstimate(t *testing.T) {
	h := newHarness(t)
	at := testBase.Add(-10 * time.Minute)
	for i, est := range []int{6712, 6698} {
		ts := at.Add(time.Duration(i) * 5 * time.Second)
		h.st.RecordTelemetry(testConnID, ts, frame.Tele{Soc: 68})
		h.st.RecordSocEstimate(testConnID, ts, est)
	}
	h.st.Flush()
	check := func(b historyBody) {
		t.Helper()
		if len(b.SocEst) != len(b.TS) {
			t.Fatalf("%s: %d soc_est values for %d steps", b.Range, len(b.SocEst), len(b.TS))
		}
		var got []float64
		for _, v := range b.SocEst {
			if v != nil {
				got = append(got, *v)
			}
		}
		if len(got) != 1 || got[0] != 66.98 {
			t.Errorf("%s soc_est: %v, want one 66.98", b.Range, got)
		}
	}
	check(decodeHist(t, h.do(t, "GET", "/api/v1/history?range=1h", RoleReadOnly, nil)))
	if _, err := h.st.RollUp(); err != nil {
		t.Fatal(err)
	}
	check(decodeHist(t, h.do(t, "GET", "/api/v1/history?range=30d", RoleReadOnly, nil)))
}
