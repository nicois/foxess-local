package api

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"foxesslocal/internal/frame"
)

// Window times and the standard token's name, as the tests use them.
const (
	tm1102 = "11:02"
	tm1200 = "12:00"
	tm1230 = "12:30"
	tm1358 = "13:58"
	tm1530 = "15:30"
	tm1500 = "15:00"
	tm1750 = "17:50"
	tm2000 = "20:00"
	tokStd = "tok-standard"
)

// The owner's table shape (2026-11-20): group 2 a force charge
// 11:02–13:58, group 4 a force discharge 17:50–20:00, group 6 the
// default self-use 00:00–23:59 (max 100, min on grid 11); 1, 3, 5, 7, 8
// free.
func ownerTable() []uint16 {
	t := make([]uint16, 80)
	copy(t[10:20], []uint16{1, 0x0B02, 0x0D3A, 6, 0x640A, 81, 10500, 0, 3, 2})
	copy(t[30:40], []uint16{1, 0x1132, 0x1400, 7, 0x640A, 50, 10076, 0, 3, 2})
	copy(t[50:60], []uint16{1, 0x0000, 0x173B, 1, 0x640B, 10, 0, 0, 1, 2})
	for k := range 8 {
		t[10*k+9] = 2
	}
	return t
}

// ownerSchedule is ownerTable as the API reads it.
func ownerSchedule() Schedule {
	g := func(i int) ScheduleGroup {
		return ScheduleGroup{Index: i, Start: hhmmZero, End: hhmmZero, WorkMode: workModeSelfUse, MaxSocPct: 100, MinSocOnGridPct: 10, FdSocPct: 10}
	}
	s := Schedule{Enabled: true, Groups: []ScheduleGroup{g(1), g(2), g(3), g(4), g(5), g(6), g(7), g(8)}}
	s.Groups[1] = ScheduleGroup{Index: 2, Enabled: true, Start: tm1102, End: tm1358, WorkMode: workModeForceCharge, MaxSocPct: 100, MinSocOnGridPct: 10, FdSocPct: 81, FdPwrW: 10500}
	s.Groups[3] = ScheduleGroup{Index: 4, Enabled: true, Start: tm1750, End: tm2000, WorkMode: workModeForceDischarge, MaxSocPct: 100, MinSocOnGridPct: 10, FdSocPct: 50, FdPwrW: 10076}
	s.Groups[5] = ScheduleGroup{Index: 6, Enabled: true, Start: hhmmZero, End: hhmmDayEnd, WorkMode: workModeSelfUse, MaxSocPct: 100, MinSocOnGridPct: 11, FdSocPct: 10}
	return s
}

// TestPlanOverride (D-029): the window lies inside or beside every
// group it meets (cut short where one begins inside it, or ends first
// around its start), in the lowest free group below each group it lies
// inside; the owner's groups never move.
func TestPlanOverride(t *testing.T) {
	m := minuteOf
	busy1 := ownerSchedule()
	busy1.Groups[0] = ScheduleGroup{Index: 1, Enabled: true, Start: "06:00", End: "07:00", WorkMode: workModeFeedIn, MaxSocPct: 100, MinSocOnGridPct: 10, FdSocPct: 10}
	renewing := ownerSchedule()
	renewing.Groups[0] = ScheduleGroup{Index: 1, Enabled: true, Start: tm1500, End: tm1530, WorkMode: workModeForceDischarge, MaxSocPct: 100, MinSocOnGridPct: 11, FdSocPct: 30, FdPwrW: 5000}
	off := ownerSchedule()
	off.Enabled = false
	cases := []struct {
		name     string
		sched    Schedule
		lo, hi   string
		own      int
		group    int
		end      string
		base     int
		wantCode string
	}{
		{"inside the default", ownerSchedule(), tm1500, tm1530, 0, 1, tm1530, 6, ""},
		{"cut where a window begins", ownerSchedule(), "17:40", "18:10", 0, 1, tm1750, 6, ""},
		{"inside a force window", ownerSchedule(), tm1200, tm1230, 0, 1, tm1230, 2, ""},
		{"cut where the window around it ends", ownerSchedule(), "13:50", "14:20", 0, 1, tm1358, 2, ""},
		{"no free group before the window around it", busy1, tm1200, tm1230, 0, 0, "", 0, "override_no_group"},
		{"a renewal keeps its group", renewing, tm1500, "16:00", 1, 1, "16:00", 6, ""},
		{"below the default when group 1 is taken", busy1, tm1500, tm1530, 0, 3, tm1530, 6, ""},
		{"scheduler off", off, tm1500, tm1530, 0, 0, "", 0, "override_unavailable"},
	}
	for _, c := range cases {
		plan, p := planOverride(c.sched, m(c.lo), m(c.hi), c.own)
		if c.wantCode != "" {
			if p == nil || p.Code != c.wantCode {
				t.Errorf("%s: problem %+v, want %s", c.name, p, c.wantCode)
			}
			continue
		}
		if p != nil {
			t.Errorf("%s: problem %+v", c.name, p)
			continue
		}
		if plan.group != c.group || plan.end != m(c.end) || plan.base.Index != c.base {
			t.Errorf("%s: group %d end %d base %d, want %d %s %d", c.name, plan.group, plan.end, plan.base.Index, c.group, c.end, c.base)
		}
	}
}

// seedOwner records the owner's table and the inverter's clock (15:00
// on the inverter at ts).
func seedOwner(h *harness, ts time.Time) {
	h.st.RecordRegisters(ts, 48000, []uint16{1, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	h.st.RecordRegisters(ts, 48010, ownerTable())
	h.st.RecordRegisters(ts, 40000, []uint16{2026, 11, 8, 15, 0, 0})
}

// lastTable is the last 48010 table written to the inverter (ok false:
// none).
func lastTable(t *testing.T, h *harness) (frame.Modbus, bool) {
	t.Helper()
	frs := h.inv.injected()
	for _, fr := range slices.Backward(frs) {
		mb, err := frame.ParseModbus(fr.Payload)
		if err == nil && mb.Fn == 0x10 && mb.Addr == regSlotTableAddr {
			return mb, true
		}
	}
	return frame.Modbus{}, false
}

// slotRegs is group k's first seven registers in a 48010 write.
func slotRegs(mb frame.Modbus, k int) [7]int {
	b := 10 * (k - 1)
	var out [7]int
	for i := range out {
		out[i] = regAt(mb.Data, b+i)
	}
	return out
}

// overrideBody is a PUT /schedule/override body.
func overrideBody(until time.Time, soc int) map[string]any {
	return map[string]any{"mode": workModeForceDischarge, "until": until.Format(time.RFC3339), "fd_soc_pct": soc, "fd_pwr_w": 5000}
}

const pathOverride = "/api/v1/schedule/override"

// TestScheduleOverrideStart: PUT writes the window into the lowest free
// group below the default (min on grid from it), the other groups as
// they were; GET and /state show it.
func TestScheduleOverrideStart(t *testing.T) {
	h := newHarness(t)
	seedOwner(h, testBase)
	rr := h.do(t, httpPut, pathOverride, RoleStandard, overrideBody(testBase.Add(30*time.Minute), 30))
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", rr.Code, rr.Body.String())
	}
	var got ScheduleOverride
	decodeJSON(t, rr, &got)
	if !got.Active || got.Group != 1 || got.Start != tm1500 || got.End != tm1530 || got.Holder != tokStd ||
		got.Until == nil || !got.Until.Equal(testBase.Add(30*time.Minute)) {
		t.Fatalf("override %+v", got)
	}
	mb, ok := lastTable(t, h)
	if !ok {
		t.Fatal("no table written")
	}
	if g := slotRegs(mb, 1); g != [7]int{1, 15 << 8, 15<<8 | 30, 7, 100<<8 | 11, 30, 5000} {
		t.Fatalf("group 1: %v", g)
	}
	if g := slotRegs(mb, 4); g != [7]int{1, 0x1132, 0x1400, 7, 0x640A, 50, 10076} {
		t.Fatalf("group 4 moved: %v", g)
	}
	checkOverrideShown(t, h)
}

// checkOverrideShown: GET /schedule/override and /state show the
// override of TestScheduleOverrideStart.
func checkOverrideShown(t *testing.T, h *harness) {
	t.Helper()
	var again ScheduleOverride
	decodeJSON(t, h.do(t, httpGet, pathOverride, RoleReadOnly, nil), &again)
	if again.Group != 1 || again.Holder != tokStd {
		t.Fatalf("GET: %+v", again)
	}
	var st struct {
		Override *ScheduleOverride `json:"schedule_override"`
	}
	decodeJSON(t, h.do(t, httpGet, "/api/v1/state", RoleReadOnly, nil), &st)
	if st.Override == nil || st.Override.End != tm1530 {
		t.Fatalf("/state: %+v", st.Override)
	}
}

// TestScheduleOverrideRenew: the holder's later until moves the end and
// keeps the group, start and since; another token is refused while it
// is held.
func TestScheduleOverrideRenew(t *testing.T) {
	h := newHarness(t)
	seedOwner(h, testBase)
	var first, second ScheduleOverride
	decodeJSON(t, h.do(t, httpPut, pathOverride, RoleStandard, overrideBody(testBase.Add(30*time.Minute), 30)), &first)
	h.clk.advance(10 * time.Minute)
	rr := h.do(t, httpPut, pathOverride, RoleStandard, overrideBody(h.clk.now.Add(40*time.Minute), 30))
	if rr.Code != http.StatusOK {
		t.Fatalf("renew: %d %s", rr.Code, rr.Body.String())
	}
	decodeJSON(t, rr, &second)
	if second.Group != 1 || second.Start != tm1500 || second.End != "15:50" || !second.Since.Equal(*first.Since) {
		t.Fatalf("renewed %+v (first %+v)", second, first)
	}
	mb, _ := lastTable(t, h)
	if g := slotRegs(mb, 1); g[1] != 15<<8 || g[2] != 15<<8|50 {
		t.Fatalf("group 1 window: %v", g)
	}
	rr = h.do(t, httpPut, pathOverride, RoleExpert, overrideBody(h.clk.now.Add(20*time.Minute), 30))
	checkProblem(t, rr, http.StatusConflict, "override_held")
}

// TestScheduleOverrideDelete: only the holder, an expert or an installer
// ends it; ending it clears its group.
func TestScheduleOverrideDelete(t *testing.T) {
	h := newHarness(t)
	seedOwner(h, testBase)
	h.do(t, httpPut, pathOverride, RoleStandard, overrideBody(testBase.Add(30*time.Minute), 30))
	_, other, err := h.tok.Create("other", RoleStandard)
	if err != nil {
		t.Fatal(err)
	}
	checkProblem(t, h.doAuth(t, "DELETE", pathOverride, other, nil), http.StatusForbidden, "override_held")
	rr := h.do(t, "DELETE", pathOverride, RoleExpert, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("DELETE: %d %s", rr.Code, rr.Body.String())
	}
	mb, _ := lastTable(t, h)
	if g := slotRegs(mb, 1); g != [7]int{0, 0, 0, 1, 100<<8 | 10, 10, 0} {
		t.Fatalf("group 1 not cleared: %v", g)
	}
	if g := slotRegs(mb, 6); g[0] != 1 || g[2] != 0x173B {
		t.Fatalf("default group changed: %v", g)
	}
	if o, _ := h.srv.loadOverride(); o != nil {
		t.Fatalf("still stored: %+v", o)
	}
}

// TestScheduleOverrideExpiry: the loop clears the group once the window
// has ended, and a restarted server does too (the override is stored).
func TestScheduleOverrideExpiry(t *testing.T) {
	h := newHarness(t)
	seedOwner(h, testBase)
	h.do(t, httpPut, pathOverride, RoleStandard, overrideBody(testBase.Add(30*time.Minute), 30))
	writes := len(h.inv.injected())
	if d := h.srv.overrideTick(context.Background()); d != 30*time.Minute+overrideGrace {
		t.Fatalf("wait %v", d)
	}
	if len(h.inv.injected()) != writes {
		t.Fatal("cleared early")
	}
	h.clk.advance(31 * time.Minute)
	restarted := New(h.st, h.gw, h.mnt, h.tok, h.fwd, h.bus, h.clk.get)
	if d := restarted.overrideTick(context.Background()); d != overrideIdle {
		t.Fatalf("after the clear: wait %v", d)
	}
	mb, _ := lastTable(t, h)
	if g := slotRegs(mb, 1); g[0] != 0 {
		t.Fatalf("group 1 not cleared: %v", g)
	}
	if o, _ := restarted.loadOverride(); o != nil {
		t.Fatalf("still stored: %+v", o)
	}
}

// TestScheduleOverrideOwnerChanged: when a newer table shows the group
// changed by someone else, the expiry leaves it and forgets the override.
func TestScheduleOverrideOwnerChanged(t *testing.T) {
	h := newHarness(t)
	seedOwner(h, testBase)
	h.do(t, httpPut, pathOverride, RoleStandard, overrideBody(testBase.Add(30*time.Minute), 30))
	h.clk.advance(time.Minute)
	tbl := ownerTable()
	copy(tbl[0:10], []uint16{1, 0x1000, 0x1100, 2, 0x640A, 10, 0, 0, 3, 2}) // the owner's feed-in 16:00–17:00
	h.st.RecordRegisters(h.clk.now, 48010, tbl)
	writes := len(h.inv.injected())
	h.clk.advance(31 * time.Minute)
	h.srv.overrideTick(context.Background())
	if len(h.inv.injected()) != writes {
		t.Fatal("wrote over the owner's group")
	}
	if o, _ := h.srv.loadOverride(); o != nil {
		t.Fatalf("still stored: %+v", o)
	}
}

// TestScheduleOverrideRefusals: offline (nothing queued or stored), no
// clock or schedule read yet, a bad body.
func TestScheduleOverrideRefusals(t *testing.T) {
	h := newHarness(t)
	checkProblem(t, h.do(t, httpPut, pathOverride, RoleStandard, overrideBody(testBase.Add(30*time.Minute), 30)),
		http.StatusServiceUnavailable, "wall_time_unknown")
	h.st.RecordRegisters(testBase, 40000, []uint16{2026, 11, 8, 15, 0, 0})
	checkProblem(t, h.do(t, httpPut, pathOverride, RoleStandard, overrideBody(testBase.Add(30*time.Minute), 30)),
		http.StatusServiceUnavailable, "schedule_unknown")
	seedOwner(h, testBase)
	checkProblem(t, h.do(t, httpPut, pathOverride, RoleStandard, overrideBody(testBase.Add(30*time.Second), 30)),
		http.StatusBadRequest, "invalid_value")
	checkProblem(t, h.do(t, httpPut, pathOverride, RoleStandard, overrideBody(testBase.Add(30*time.Minute), 5)),
		http.StatusBadRequest, "invalid_value")
	checkProblem(t, h.do(t, httpPut, pathOverride, RoleReadOnly, overrideBody(testBase.Add(30*time.Minute), 30)),
		http.StatusForbidden, "insufficient_role")
	h.gw.SetFlow("")
	checkProblem(t, h.do(t, httpPut, pathOverride, RoleStandard, overrideBody(testBase.Add(30*time.Minute), 30)),
		http.StatusServiceUnavailable, "inverter_offline")
	if n := len(h.inv.injected()); n != 0 {
		t.Fatalf("injected %d frames", n)
	}
	if o, _ := h.srv.loadOverride(); o != nil {
		t.Fatalf("stored: %+v", o)
	}
}

// TestRunOverridesExpiry (synctest): the loop sleeps until the window's
// end and clears the group then, not before.
func TestRunOverridesExpiry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		start := time.Now()
		h.srv.clock = time.Now
		h.st.RecordRegisters(start, 48000, []uint16{1, 0, 0, 0, 0, 0, 0, 0, 0, 0})
		h.st.RecordRegisters(start, 48010, ownerTable())
		h.st.RecordRegisters(start, 40000, []uint16{2026, 11, 8, 15, 0, 0})
		if rr := h.do(t, httpPut, pathOverride, RoleStandard, overrideBody(start.Add(20*time.Minute), 30)); rr.Code != http.StatusOK {
			t.Fatalf("PUT: %d %s", rr.Code, rr.Body.String())
		}
		ctx, cancel := context.WithCancel(context.Background())
		go h.srv.RunOverrides(ctx)
		time.Sleep(19 * time.Minute)
		if o, _ := h.srv.loadOverride(); o == nil {
			t.Fatal("cleared before its end")
		}
		time.Sleep(2 * time.Minute)
		if o, _ := h.srv.loadOverride(); o != nil {
			t.Fatalf("not cleared after its end: %+v", o)
		}
		cancel()
		synctest.Wait()
	})
}
