package api

import (
	"testing"
	"time"
)

// TestActiveMode: with the scheduler on, the first enabled group (by
// index) whose window holds the time decides the mode — the KH's
// "default work mode" is itself an all-day group (profile §5), and the
// 2026-11-17 force charge (group 1) ran inside the all-day group 3; with
// the scheduler off, or no group covering the time, the base work mode
// (41000) applies.
func TestActiveMode(t *testing.T) {
	groups := []ScheduleGroup{
		{Index: 1, Enabled: true, Start: "11:02", End: "13:58", WorkMode: workModeForceCharge},
		{Index: 2, Enabled: true, Start: "17:50", End: "20:30", WorkMode: workModeForceDischarge},
		{Index: 3, Enabled: true, Start: "00:00", End: "23:59", WorkMode: workModeSelfUse},
		{Index: 4, Enabled: false, Start: "00:00", End: "23:59", WorkMode: workModeFeedIn},
	}
	at := func(hm string) time.Time {
		tm, _ := time.Parse("15:04", hm)
		return time.Date(2026, 11, 17, tm.Hour(), tm.Minute(), 30, 0, time.Local)
	}
	cases := []struct {
		name   string
		sched  Schedule
		now    time.Time
		mode   string
		source string
		group  int
	}{
		{"all-day group", Schedule{Enabled: true, Groups: groups}, at("09:00"), workModeSelfUse, activeFromSchedule, 3},
		{"force charge wins", Schedule{Enabled: true, Groups: groups}, at("12:00"), workModeForceCharge, activeFromSchedule, 1},
		{"end is exclusive", Schedule{Enabled: true, Groups: groups}, at("13:58"), workModeSelfUse, activeFromSchedule, 3},
		{"the last minute", Schedule{Enabled: true, Groups: groups}, at("23:59"), workModeFeedIn, activeFromBase, 0},
		{"scheduler off", Schedule{Enabled: false, Groups: groups}, at("12:00"), workModeFeedIn, activeFromBase, 0},
	}
	for _, c := range cases {
		got := activeMode(c.sched, workModeFeedIn, c.now)
		if got.Mode != c.mode || got.Source != c.source || got.Group != c.group {
			t.Errorf("%s: %+v, want %s/%s/%d", c.name, got, c.mode, c.source, c.group)
		}
	}
}

// TestStateInverterLimits: /state reports every limit the Control page
// draws — the grid pair, the inverter's load / generation limits (41200 /
// 41201), the battery current limits (41007 / 41008), the power limit %
// (41016) and the EPS output mode (41015).
func TestStateInverterLimits(t *testing.T) {
	h := newHarness(t)
	block := make([]uint16, 40)
	block[0], block[1] = 450, 450 // 41007 / 41008: 45.0 A
	block[41015-41007] = 3        // UPS mode
	block[41016-41007] = 100      // power limit %
	h.st.RecordRegisters(testBase, 41007, block)
	h.st.RecordRegisters(testBase, 41200, []uint16{10000, 7000, 0})
	h.st.RecordRegisters(testBase, 40000, []uint16{2026, 11, 8, 23, 0, 0}) // the inverter's clock
	var st struct {
		Limits struct {
			LoadW       *int     `json:"load_limit_w"`
			GenerationW *int     `json:"generation_limit_w"`
			ChargeA     *float64 `json:"max_charge_current_a"`
			DischargeA  *float64 `json:"max_discharge_current_a"`
			PowerPct    *int     `json:"power_limit_pct"`
		} `json:"limits"`
		EpsMode    string `json:"eps_mode"`
		ActiveMode *struct {
			Mode   string `json:"mode"`
			Source string `json:"source"`
		} `json:"active_mode"`
	}
	decodeJSON(t, h.do(t, "GET", "/api/v1/state", RoleReadOnly, nil), &st)
	l := st.Limits
	if l.LoadW == nil || *l.LoadW != 10000 || l.GenerationW == nil || *l.GenerationW != 7000 ||
		l.ChargeA == nil || *l.ChargeA != 45 || l.DischargeA == nil || *l.DischargeA != 45 ||
		l.PowerPct == nil || *l.PowerPct != 100 {
		t.Fatalf("limits %+v", l)
	}
	if st.EpsMode != "ups" || st.ActiveMode == nil || st.ActiveMode.Mode == "" {
		t.Fatalf("eps_mode %q active %+v", st.EpsMode, st.ActiveMode)
	}
}
