package api

import "time"

// ActiveMode is the work mode the inverter runs now and why: the first
// enabled schedule group (by index) whose window holds the time when the
// scheduler is on — the KH's "default work mode" is itself an all-day
// group (profile §5), and a narrower group inside it (the 2026-11-17
// force charge, group 1 inside group 3) wins — else the base work mode
// (41000). The precedence and the exclusive end minute are inferred from
// the inverter's behaviour, not documented.
type ActiveMode struct {
	Mode   string `json:"mode"`
	Source string `json:"source"`          // "schedule" | "base"
	Group  int    `json:"group,omitempty"` // the deciding group (source schedule)
	Start  string `json:"start,omitempty"`
	End    string `json:"end,omitempty"`
}

// Active-mode sources.
const (
	activeFromSchedule = "schedule"
	activeFromBase     = "base"
)

// activeModeNow is the running mode on the inverter's wall time; nil
// while that time is unknown (no fresh clock reading, no site zone).
func (s *Server) activeModeNow(sched Schedule, base string) *ActiveMode {
	now, ok := s.wallNow()
	if !ok {
		return nil
	}
	am := activeMode(sched, base, now)
	return &am
}

// activeMode resolves the running mode at now (inverter-local time).
func activeMode(s Schedule, base string, now time.Time) ActiveMode {
	if s.Enabled {
		m := now.Hour()*60 + now.Minute()
		for _, g := range s.Groups {
			if g.Enabled && minuteOf(g.Start) <= m && m < minuteOf(g.End) {
				return ActiveMode{Mode: g.WorkMode, Source: activeFromSchedule, Group: g.Index, Start: g.Start, End: g.End}
			}
		}
	}
	return ActiveMode{Mode: base, Source: activeFromBase}
}

// minuteOf is minutes since midnight of "HH:MM" (malformed = -1: never
// matches).
func minuteOf(s string) int {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return -1
	}
	return t.Hour()*60 + t.Minute()
}

// ForceDischargeNow reports the force discharge the inverter is running
// now, for the battery forecast (owner, 2026-11-20): the time left in its
// window and its cut-off SoC. ok is false outside one, or while the
// inverter's wall time or the schedule is unknown.
func (s *Server) ForceDischargeNow() (left time.Duration, cutoffPct int, ok bool) {
	st, err := s.st.QueryState()
	if err != nil {
		return 0, 0, false
	}
	now, known := s.wallNow()
	if !known {
		return 0, 0, false
	}
	cfg := s.scanConfigRegs(st.SchedTS)
	return forceDischargeAt(scheduleOf(st.Schedule, cfg.schedOn), now)
}

// forceDischargeAt is ForceDischargeNow at now (inverter-local time):
// the running group must be a force discharge; its end minute is
// exclusive.
func forceDischargeAt(sched Schedule, now time.Time) (time.Duration, int, bool) {
	am := activeMode(sched, "", now)
	if am.Mode != workModeForceDischarge || am.Group < 1 || am.Group > len(sched.Groups) {
		return 0, 0, false
	}
	g := sched.Groups[am.Group-1]
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	end := day.Add(time.Duration(minuteOf(g.End)) * time.Minute)
	return end.Sub(now), g.FdSocPct, true
}
