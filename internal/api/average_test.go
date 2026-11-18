package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

type averageResp struct {
	Window   string    `json:"window"`
	From     time.Time `json:"from"`
	To       time.Time `json:"to"`
	Samples  int       `json:"samples"`
	Expected int       `json:"expected_samples"`
	Power    *struct {
		PVW      int `json:"pv_w"`
		GridW    int `json:"grid_w"`
		LoadW    int `json:"load_w"`
		AcPortW  int `json:"ac_port_w"`
		BatteryW int `json:"battery_w"`
	} `json:"power"`
}

// TestPowerAverage: GET /power/average?window=5m is the mean of the raw
// samples in the trailing window, with the sample count and the count a
// full 5 s cadence would give.
func TestPowerAverage(t *testing.T) {
	h := newHarness(t)
	h.seedTele(testBase.Add(-4*time.Minute), 1000, -300, 700, 1000, 0, 60, 2)
	h.seedTele(testBase.Add(-1*time.Minute), 3000, -100, 900, 1000, 1000, 61, 2)
	h.seedTele(testBase.Add(-10*time.Minute), 9999, 0, 0, 0, 0, 60, 2) // outside
	rr := h.do(t, httpGet, "/api/v1/power/average?window=5m", RoleReadOnly, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	var b averageResp
	if err := json.Unmarshal(rr.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	if b.Window != "5m" || b.Samples != 2 || b.Expected != 60 || !b.To.Equal(testBase) || !b.From.Equal(testBase.Add(-5*time.Minute)) {
		t.Fatalf("meta: %+v", b)
	}
	if b.Power == nil || b.Power.PVW != 2000 || b.Power.GridW != -200 || b.Power.LoadW != 800 || b.Power.AcPortW != 1000 || b.Power.BatteryW != 500 {
		t.Fatalf("power: %+v", b.Power)
	}
}

// TestPowerAverageEmptyAndInvalid: no samples → power null (never zeros
// that read as "nothing flowing"); an unknown window → 400.
func TestPowerAverageEmptyAndInvalid(t *testing.T) {
	h := newHarness(t)
	rr := h.do(t, httpGet, "/api/v1/power/average?window=30m", RoleReadOnly, nil)
	if rr.Code != http.StatusOK || !json.Valid(rr.Body.Bytes()) {
		t.Fatalf("empty: %d %s", rr.Code, rr.Body.String())
	}
	var b averageResp
	_ = json.Unmarshal(rr.Body.Bytes(), &b)
	if b.Power != nil || b.Samples != 0 {
		t.Fatalf("empty window: %+v", b)
	}
	if rr := h.do(t, httpGet, "/api/v1/power/average?window=7m", RoleReadOnly, nil); rr.Code != http.StatusBadRequest {
		t.Fatalf("window=7m: %d, want 400", rr.Code)
	}
}
