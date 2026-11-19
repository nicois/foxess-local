package api

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"foxesslocal/internal/gateway"
	"foxesslocal/internal/settings"
	"foxesslocal/internal/siteclock"
)

// regRTCCount is the clock block's length (siteclock.ClockRegs).
const regRTCCount = siteclock.ClockRegs

// actionSetClock is the setup check's one-press remedy: POST /clock/set.
const actionSetClock = "set_clock"

// siteZone is the site's time zone (settings), nil until chosen. The
// host's zone is never used in its place.
func (s *Server) siteZone() *time.Location { return s.info.values().SiteZone }

// inverterClock is the newest reading of the inverter's clock and when
// it was read; false before a complete read.
func (s *Server) inverterClock() ([regRTCCount]int, time.Time, bool) {
	return siteclock.ReadingFrom(s.st.RegisterRead)
}

// inverterClockOffset is the inverter's clock minus the site's time, s;
// false before a read or without a site zone.
func (s *Server) inverterClockOffset() (float64, bool) {
	z := s.siteZone()
	rtc, at, ok := s.inverterClock()
	if z == nil || !ok {
		return 0, false
	}
	v := siteclock.Judge(z, rtc, at, s.clock())
	if v.Kind == siteclock.Invalid {
		return 0, false
	}
	return v.Off.Seconds(), true
}

// wallNow is the inverter's wall time now (siteclock.WallNow).
func (s *Server) wallNow() (time.Time, bool) {
	rtc, at, ok := s.inverterClock()
	return siteclock.WallNow(s.siteZone(), rtc, at, ok, s.clock())
}

func (s *Server) checkClock() Check {
	rtc, at, ok := s.inverterClock()
	return clockCheck(s.siteZone(), rtc, at, ok, s.clock())
}

// clockCheck judges the inverter's clock (read at readAt) against the
// site zone's time.
func clockCheck(zone *time.Location, rtc [regRTCCount]int, readAt time.Time, ok bool, now time.Time) Check {
	c := Check{ID: "clock", Title: "Clocks agree"}
	if zone == nil {
		c.Status, c.Detail = checkFail, "the site's time zone is not chosen yet: the inverter's clock and schedule run on it"
		c.Fix = "Choose the inverter's time zone (Setup, Time zone)."
		return c
	}
	if !ok {
		c.Status, c.Detail = checkPending, "the inverter's clock is read within 5 minutes of connecting"
		return c
	}
	v := siteclock.Judge(zone, rtc, readAt, now)
	way := "ahead of"
	if v.Off < 0 {
		way = "behind"
	}
	off := fmt.Sprintf("%s %s %s time", siteclock.Duration(v.Off.Abs()), way, zone)
	switch v.Kind {
	case siteclock.InStep:
		c.Status, c.Detail = checkOK, fmt.Sprintf("the inverter's clock matches %s time", zone)
	case siteclock.Drift:
		c.Status, c.Detail = checkWarn, "the inverter's clock is "+off+": foxess-local corrects it automatically"
	case siteclock.ZoneShift:
		c.Status, c.Detail = checkWarn, "the inverter's clock is "+off+
			" (it missed a daylight-saving change): foxess-local corrects it automatically"
	case siteclock.Unexplained:
		c.Status, c.Detail = checkWarn, "the inverter's clock is "+off+"; its schedule runs on its own clock"
		c.Fix = fmt.Sprintf("If %s is the inverter's time zone, set the inverter's clock to it; if not, choose the right time zone.", zone)
		c.Action = actionSetClock
	case siteclock.Invalid:
		c.Status, c.Detail = checkFail, fmt.Sprintf("the inverter's clock reads %v, which is not a date and time", rtc)
		c.Fix = fmt.Sprintf("Set the inverter's clock to %s time.", zone)
		c.Action = actionSetClock
	}
	return c
}

// zoneConfirm: choosing a site zone the inverter's clock disagrees with
// (not drift, not a missed daylight-saving change) needs ?confirm=zone —
// usually it is the wrong zone. nil when nothing needs confirming.
func (s *Server) zoneConfirm(r *http.Request, changes []settingChange) *problem {
	if r.URL.Query().Get("confirm") == "zone" {
		return nil
	}
	for _, c := range changes {
		if c.key != settings.KeySiteTimeZone || c.reset {
			continue
		}
		z, err := siteclock.Zone(c.value)
		rtc, at, ok := s.inverterClock()
		if err != nil || !ok {
			return nil // validated already; no reading: nothing to compare
		}
		now := s.clock()
		if _, fresh := siteclock.InverterNow(rtc, at, now); !fresh {
			return nil
		}
		v := siteclock.Judge(z, rtc, at, now)
		if v.Kind != siteclock.Unexplained {
			return nil
		}
		inv, _ := siteclock.InverterNow(rtc, at, now)
		return &problem{
			Title: "Time zone mismatch", Code: "zone_mismatch",
			Detail: fmt.Sprintf("the inverter's clock reads %s; in %s it is %s (%s apart). Choose the inverter's "+
				"time zone, or confirm this one and set the inverter's clock to it.",
				inv.Format("15:04"), z, now.In(z).Format("15:04"), siteclock.Duration(v.Off.Abs())),
		}
	}
	return nil
}

// clockSet is POST /clock/set's answer.
type clockSet struct {
	SetTo    string `json:"set_to"` // the wall time written, 2006-01-02T15:04:05
	TimeZone string `json:"time_zone"`
}

// handleClockSet is POST /clock/set (installer): the inverter's clock to
// the site's wall time now, through the gateway (C-003, C-008).
func (s *Server) handleClockSet(w http.ResponseWriter, r *http.Request, tk Token) {
	z := s.siteZone()
	if z == nil {
		writeProblem(w, http.StatusConflict, problem{
			Title: "No time zone", Code: "zone_unset",
			Detail: "choose the site's time zone first (PUT /settings site.time_zone)",
		})
		return
	}
	if f := s.info.HostClockSynced; f != nil && !f() {
		writeProblem(w, http.StatusConflict, problem{
			Title: "Host clock not synchronised", Code: "host_clock_unsynced",
			Detail: "this machine's clock is not synchronised (NTP): its time cannot set the inverter's",
		})
		return
	}
	now := s.clock()
	wall := siteclock.Wall(z, now)
	params := map[gateway.Param]int{}
	for i, k := range []gateway.Param{
		gateway.ParamYear, gateway.ParamMonth, gateway.ParamDay,
		gateway.ParamHour, gateway.ParamMinute, gateway.ParamSecond,
	} {
		params[k] = wall[i]
	}
	at := now.In(z).Format("2006-01-02T15:04:05")
	resp := s.runCommand(r, tk, gateway.Command{Kind: gateway.KindClock, Params: params},
		fmt.Sprintf("clock set to %s (%s)", strings.Replace(at, "T", " ", 1), z), clockSet{SetTo: at, TimeZone: z.String()})
	s.writeCommand(w, resp)
}
