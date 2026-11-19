package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"foxesslocal/internal/settings"
	"foxesslocal/internal/siteclock"
)

// testZone is the site zone of the tests.
const testZone = "Australia/Melbourne"

// melbourne is the site zone of the tests.
func melbourne(t *testing.T) *time.Location {
	t.Helper()
	loc, err := siteclock.Zone(testZone)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// rtcAt is what the inverter's clock shows when it reads w's wall time.
func rtcAt(w time.Time) [6]int {
	return [6]int{w.Year(), int(w.Month()), w.Day(), w.Hour(), w.Minute(), w.Second()}
}

// TestClockCheckSiteZone (owner, 2026-11-19): the inverter's clock is
// judged against the site's time zone, never the host's. No zone yet is
// a failing check (setup asks for it); a whole-hours offset says how
// far; drift and a missed daylight-saving change are corrected
// automatically; anything else offers to set the clock.
func TestClockCheckSiteZone(t *testing.T) {
	loc := melbourne(t)
	now := time.Date(2026, 11, 19, 5, 17, 26, 0, time.UTC)
	wall := now.In(loc)

	if c := clockCheck(nil, [6]int{}, time.Time{}, false, now); c.Status != checkFail || !strings.Contains(c.Fix, "time zone") {
		t.Errorf("no zone: %+v", c)
	}
	if c := clockCheck(loc, [6]int{}, time.Time{}, false, now); c.Status != checkPending {
		t.Errorf("not read: %+v", c)
	}
	c := clockCheck(loc, rtcAt(wall.Add(20*time.Second)), now, true, now)
	if c.Status != checkOK || !strings.Contains(c.Detail, testZone) {
		t.Errorf("in step: %+v", c)
	}
	c = clockCheck(loc, rtcAt(wall.Add(-3*time.Minute)), now, true, now)
	if c.Status != checkWarn || !strings.Contains(c.Detail, "3 min behind") || !strings.Contains(c.Detail, "automatically") || c.Action != "" {
		t.Errorf("drift: %+v", c)
	}
	c = clockCheck(loc, rtcAt(now), now, true, now) // an inverter on UTC
	if c.Status != checkWarn || !strings.Contains(c.Detail, "11 h behind") || c.Action != actionSetClock || c.Fix == "" {
		t.Errorf("11 h: %+v", c)
	}
	c = clockCheck(loc, rtcAt(wall.Add(-5*time.Hour-30*time.Minute)), now, true, now)
	if !strings.Contains(c.Detail, "5 h 30 min behind") {
		t.Errorf("5:30: %+v", c)
	}
}

// TestReadyzClockNeverFails: no site zone, or an impossible clock
// reading, is no reason for "not ready" (the upgrade guard rolls back on
// it).
func TestReadyzClockNeverFails(t *testing.T) {
	h := newHarness(t)
	set := withSettings(t, h)
	h.seedFullState()
	h.st.RecordRegisters(h.clk.get(), 0x9C40, []uint16{2026, 13, 40, 0, 0, 0})
	h.st.Flush()
	clockStatus := func() string {
		var b struct {
			Checks []struct{ ID, Status string } `json:"checks"`
		}
		decode(t, h.do(t, httpGet, "/readyz", "", nil), &b)
		for _, c := range b.Checks {
			if c.ID == "clock" {
				return c.Status
			}
		}
		return ""
	}
	if s := clockStatus(); s != checkPending {
		t.Errorf("no zone: %s", s)
	}
	if err := set.Set(settings.KeySiteTimeZone, testZone, "installer"); err != nil {
		t.Fatal(err)
	}
	if s := clockStatus(); s != checkWarn {
		t.Errorf("impossible reading: %s", s)
	}
}

// TestSiteZoneMismatchConfirm: setting a zone the inverter's clock
// disagrees with (beyond drift or a daylight-saving change) needs a
// confirmation; one it agrees with does not.
func TestSiteZoneMismatchConfirm(t *testing.T) {
	h := newHarness(t)
	set := withSettings(t, h)
	rtc := rtcAt(h.clk.get().In(melbourne(t)))
	//nolint:gosec // G115: calendar fields fit uint16
	h.st.RecordRegisters(h.clk.get(), 0x9C40, []uint16{
		uint16(rtc[0]), uint16(rtc[1]), uint16(rtc[2]), uint16(rtc[3]), uint16(rtc[4]), uint16(rtc[5]),
	})
	h.st.Flush()

	p := checkProblem(t, h.do(t, httpPut, pathSettings, RoleInstaller, map[string]any{settings.KeySiteTimeZone: "Europe/London"}),
		http.StatusConflict, "zone_mismatch")
	if !strings.Contains(p.Detail, "Europe/London") || set.Get().SiteZone != nil {
		t.Fatalf("mismatch: %+v, zone %v", p, set.Get().SiteZone)
	}
	wantStatus(t, h.do(t, httpPut, pathSettings+"?confirm=zone", RoleInstaller, map[string]any{settings.KeySiteTimeZone: "Europe/London"}), http.StatusOK)
	wantStatus(t, h.do(t, httpPut, pathSettings, RoleInstaller, map[string]any{settings.KeySiteTimeZone: testZone}), http.StatusOK)
	if z := set.Get().SiteZone; z == nil || z.String() != testZone {
		t.Fatalf("zone %v", z)
	}
}

// TestClockSetEndpoint: POST /clock/set (installer) writes the site's
// wall time now to the inverter (fn10 40000 ×6) — refused until a zone
// is chosen.
func TestClockSetEndpoint(t *testing.T) {
	h := newHarness(t)
	withSettings(t, h)
	checkProblem(t, h.do(t, httpPost, pathClockSet, RoleInstaller, nil), http.StatusConflict, "zone_unset")
	checkProblem(t, h.do(t, httpPost, pathClockSet, RoleExpert, nil), http.StatusForbidden, "insufficient_role")

	wantStatus(t, h.do(t, httpPut, pathSettings, RoleInstaller, map[string]any{settings.KeySiteTimeZone: testZone}), http.StatusOK)
	rr := h.do(t, httpPost, pathClockSet, RoleInstaller, nil)
	wantStatus(t, rr, http.StatusOK)
	want := rtcAt(h.clk.get().In(melbourne(t)))
	var got []int
	for _, fr := range h.inv.injected() {
		p := fr.Payload
		if len(p) >= 19 && p[1] == 0x10 && p[2] == 0x9c && p[3] == 0x40 {
			for i := range 6 {
				got = append(got, int(p[7+2*i])<<8|int(p[8+2*i]))
			}
		}
	}
	if len(got) != 6 || [6]int(got) != want {
		t.Fatalf("written %v, want %v", got, want)
	}
	if !strings.Contains(rr.Body.String(), testZone) {
		t.Errorf("body %s", rr.Body.String())
	}
}
