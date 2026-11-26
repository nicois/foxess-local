package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"foxesslocal/internal/frame"
	"foxesslocal/internal/settings"
)

// meter2State is the part of GET /state this file checks.
type meter2State struct {
	Power struct {
		PVW     int  `json:"pv_w"`
		Meter2W *int `json:"meter2_w"`
	} `json:"power"`
	Present struct {
		PV     bool `json:"pv"`
		Meter2 bool `json:"meter2"`
	} `json:"present"`
	Meter2Label string `json:"meter2_label"`
}

func getMeter2State(t *testing.T, h *harness) meter2State {
	t.Helper()
	rr := h.do(t, httpGet, "/api/v1/state", RoleReadOnly, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /state: %d %s", rr.Code, rr.Body.String())
	}
	var st meter2State
	if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	return st
}

// TestStateMeter2 (issue #2): /state carries the second CT (meter_2_w,
// − = into the site), null when the reading has none (the 150 B
// frame); whether the site has DC solar and a second CT (present); and
// what the second CT measures (meter2_label, AC-coupled solar by
// default).
func TestStateMeter2(t *testing.T) {
	h := newHarness(t)
	at := testBase.Add(-10 * time.Second)
	h.st.RecordTelemetry(testConnID, at, frame.Tele{Haus: 400, Soc: 50})
	st := getMeter2State(t, h)
	if st.Power.Meter2W != nil || st.Present.Meter2 || st.Present.PV {
		t.Fatalf("no meter 2 yet: %+v", st)
	}
	if st.Meter2Label != settings.Meter2ACSolar {
		t.Fatalf("label %q, want the default %q", st.Meter2Label, settings.Meter2ACSolar)
	}
	m := int32(-1327)
	h.st.RecordTelemetry(testConnID, at.Add(5*time.Second), frame.Tele{Haus: 354, Soc: 50, Meter2: &m})
	st = getMeter2State(t, h)
	if st.Power.Meter2W == nil || *st.Power.Meter2W != -1327 || !st.Present.Meter2 || st.Present.PV {
		t.Fatalf("meter 2 at -1327 W: %+v", st)
	}
	set := withSettings(t, h)
	if err := set.Set(settings.KeyMeter2Label, "Shed solar", "installer"); err != nil {
		t.Fatal(err)
	}
	if st = getMeter2State(t, h); st.Meter2Label != "Shed solar" {
		t.Fatalf("label %q", st.Meter2Label)
	}
}

// TestStatePresentPV (owner, 2026-11-27): DC solar is present only when
// both the inverter's lifetime PV total is above zero and this app has
// seen it generate (live or in the stored history). A total not yet read
// does not hide it; a zero total does (a site without PV strings), as
// does no generation on record (strings connected once, long ago).
func TestStatePresentPV(t *testing.T) {
	cases := []struct {
		name   string
		total  *float64 // nil = not read yet
		gen    bool
		wantPV bool
	}{
		{"total and generation", new(1234.5), true, true},
		{"total not read, generation", nil, true, true},
		{"zero total, generation", new(0.0), true, false},
		{"total, no generation on record", new(1234.5), false, false},
	}
	for _, c := range cases {
		h := newHarness(t)
		h.srv.info.Energy = func() Energy { return Energy{PVKWh: c.total, Sources: map[string]string{}} }
		sol := int32(10)
		if c.gen {
			sol = 2000
		}
		h.st.RecordTelemetry(testConnID, testBase.Add(-5*time.Second), frame.Tele{Sol: sol, Haus: 400, Soc: 50})
		if st := getMeter2State(t, h); st.Present.PV != c.wantPV {
			t.Errorf("%s: present.pv %v, want %v", c.name, st.Present.PV, c.wantPV)
		}
	}
}

// TestHistoryMeter2 (issue #2): the history carries the second CT per
// step (− = into the site) and the energy it brought in, on the
// per-minute (1 h) and folded hourly (30 d) paths; null without it.
func TestHistoryMeter2(t *testing.T) {
	h := newHarness(t)
	at := testBase.Add(-10 * time.Minute)
	m := int32(-1200)
	for i := range 3 {
		h.st.RecordTelemetry(testConnID, at.Add(time.Duration(i)*5*time.Second), frame.Tele{Haus: 500, Soc: 50, Meter2: &m})
	}
	h.st.Flush()
	check := func(b historyBody) {
		t.Helper()
		if len(b.Meter2) != len(b.TS) {
			t.Fatalf("%s: %d meter2 values for %d steps", b.Range, len(b.Meter2), len(b.TS))
		}
		var got []int
		for _, v := range b.Meter2 {
			if v != nil {
				got = append(got, *v)
			}
		}
		if len(got) != 1 || got[0] != -1200 {
			t.Errorf("%s meter2: %v, want one -1200", b.Range, got)
		}
		if w := b.Totals.Meter2GenWh; w == nil || *w != 3*1200*5/3600 {
			t.Errorf("%s meter2_gen_wh: %v, want %d", b.Range, w, 3*1200*5/3600)
		}
	}
	check(decodeHist(t, h.do(t, httpGet, "/api/v1/history?range=1h", RoleReadOnly, nil)))
	if _, err := h.st.RollUp(); err != nil {
		t.Fatal(err)
	}
	check(decodeHist(t, h.do(t, httpGet, "/api/v1/history?range=30d", RoleReadOnly, nil)))
}

// TestHistoryMeter2Absent: without a second-CT reading the series is
// null and its energy null — unknown, never 0.
func TestHistoryMeter2Absent(t *testing.T) {
	at := testBase.Add(-10 * time.Minute)
	empty := newHarness(t)
	empty.st.RecordTelemetry(testConnID, at, frame.Tele{Haus: 500, Soc: 50})
	empty.st.Flush()
	b := decodeHist(t, empty.do(t, httpGet, "/api/v1/history?range=1h", RoleReadOnly, nil))
	for _, v := range b.Meter2 {
		if v != nil {
			t.Fatalf("no meter 2: %v", *v)
		}
	}
	if b.Totals.Meter2GenWh != nil {
		t.Fatalf("no meter 2: gen %d Wh, want null", *b.Totals.Meter2GenWh)
	}
}
