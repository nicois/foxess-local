package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"foxesslocal/internal/gateway"
)

// The schedule override (D-029): a force charge or force discharge
// from now until a given time, for an automation (a sidecar) that
// decides when, not how. foxess-local owns the schedule change: it puts
// the window in a free group that runs before the windows it falls in,
// keeps the owner's groups where they are (each group's parameter block
// stays with its slot), and clears the group again when the override
// ends — at its expiry, on DELETE, or after a restart. Schedule windows
// repeat every day, so a group left behind would run again tomorrow.
// Its holder renews it by sending the same PUT with a later until; if
// the holder stops, it lapses.

// overrideKey is the store key of the active override (JSON).
const overrideKey = "schedule_override.active"

// ScheduleOverride bounds.
const (
	// overrideMinLead: until must be at least this far ahead.
	overrideMinLead = time.Minute
	// overrideMaxLen bounds one override (a lease, not a schedule).
	overrideMaxLen = 12 * time.Hour
	// overrideGrace: the group is cleared this long after its end (the
	// inverter has stopped it by then; its end minute is exclusive).
	overrideGrace = 5 * time.Second
	// overrideRetry: a failed clear is tried again after this.
	overrideRetry = time.Minute
	// overrideIdle: the expiry loop's wake-up with nothing to do.
	overrideIdle = time.Hour
	// dayLastMinute is the last end a group can have (23:59).
	dayLastMinute = 23*60 + 59
)

// System event types for the override (start, renewal, end).
const (
	SystemScheduleOverrideStarted = "schedule-override-started"
	SystemScheduleOverrideEnded   = "schedule-override-ended"
)

// schedOverride is the active override as stored (overrideKey).
type schedOverride struct {
	Holder   string    `json:"holder"`      // the token name
	HolderID string    `json:"holder_id"`   // the token id
	Role     Role      `json:"holder_role"` // the token's role
	Mode     string    `json:"mode"`        // force-charge | force-discharge
	FdSocPct int       `json:"fd_soc_pct"`
	FdPwrW   int       `json:"fd_pwr_w"`
	Group    int       `json:"group"` // the schedule group it holds (1..8)
	Day      string    `json:"day"`   // the inverter's date of the window (YYYY-MM-DD)
	Start    string    `json:"start"` // the window, inverter time (HH:MM)
	End      string    `json:"end"`
	Until    time.Time `json:"until"`     // when it ends (the window's end)
	Asked    time.Time `json:"requested"` // the until the holder asked for
	Since    time.Time `json:"since"`     // when it started
	Written  time.Time `json:"written"`   // the last write of the group
}

// ScheduleOverride is the spec's ScheduleOverride (GET/PUT /schedule/override, /state).
type ScheduleOverride struct {
	Active         bool       `json:"active"`
	Mode           string     `json:"mode,omitempty"`
	Holder         string     `json:"holder,omitempty"`
	Group          int        `json:"group,omitempty"`
	Start          string     `json:"start,omitempty"`
	End            string     `json:"end,omitempty"`
	Until          *time.Time `json:"until,omitempty"`
	RequestedUntil *time.Time `json:"requested_until,omitempty"`
	Since          *time.Time `json:"since,omitempty"`
	FdSocPct       int        `json:"fd_soc_pct,omitempty"`
	FdPwrW         int        `json:"fd_pwr_w,omitempty"`
}

// view is the spec's form of a stored override.
func (o *schedOverride) view() ScheduleOverride {
	if o == nil {
		return ScheduleOverride{}
	}
	until, asked, since := o.Until, o.Asked, o.Since
	return ScheduleOverride{
		Active: true, Mode: o.Mode, Holder: o.Holder, Group: o.Group,
		Start: o.Start, End: o.End, Until: &until, RequestedUntil: &asked, Since: &since,
		FdSocPct: o.FdSocPct, FdPwrW: o.FdPwrW,
	}
}

// overrideRequest is the PUT /schedule/override body.
type overrideRequest struct {
	Mode     string    `json:"mode"`
	Until    time.Time `json:"until"`
	FdSocPct *int      `json:"fd_soc_pct"`
	FdPwrW   *int      `json:"fd_pwr_w"`
}

// validate checks the body against now (the host's time).
func (r overrideRequest) validate(now time.Time) *problem {
	if r.Mode != workModeForceDischarge && r.Mode != workModeForceCharge {
		return badFieldPtr("mode", "must be force-discharge or force-charge")
	}
	switch {
	case r.Until.IsZero():
		return badFieldPtr("until", "required (RFC 3339)")
	case r.Until.Before(now.Add(overrideMinLead)):
		return badFieldPtr("until", "must be at least a minute ahead")
	case r.Until.After(now.Add(overrideMaxLen)):
		return badFieldPtr("until", "must be at most 12 hours ahead")
	}
	if r.FdSocPct == nil {
		return badFieldPtr("fd_soc_pct", "required")
	}
	if *r.FdSocPct < slotSocMinPct || *r.FdSocPct > socMaxPct {
		return badFieldPtr("fd_soc_pct", "must be in 10..100")
	}
	if r.FdPwrW == nil {
		return badFieldPtr("fd_pwr_w", "required")
	}
	if *r.FdPwrW < 0 || *r.FdPwrW > slotFdPwrMaxW {
		return badFieldPtr("fd_pwr_w", "must be in 0..10500")
	}
	return nil
}

// overridePlan is where an override goes in the table.
type overridePlan struct {
	group int           // 1..8
	end   int           // the window's end, minutes (may be before the asked end)
	base  ScheduleGroup // the group it runs inside (zero: none)
}

// planOverride places a window from lo to hi (minutes of the inverter's
// day, hi exclusive) in sched. own is the group the override already
// holds (0: none); it is not a conflict. The first enabled group by
// index whose window holds the time runs (activeMode), and two enabled
// windows may nest but not cross (checkScheduleOverlap). So:
//   - the window is cut short where another group's window begins inside
//     it, or where a window it starts inside ends first; it then lies
//     inside (or beside) every group it meets;
//   - it takes the lowest free group below every group it lies inside, so
//     it runs before them.
//
// The owner's groups never move: each group's parameter block (57000 +
// 50·(k−1): import / export limit, PV) belongs to its slot.
func planOverride(sched Schedule, lo, hi, own int) (overridePlan, *problem) {
	if !sched.Enabled {
		return overridePlan{}, overrideUnavailable("the inverter's scheduler is off; an override would turn on every group with it")
	}
	hi = trimWindow(sched, lo, hi, own)
	first, base := innermost(sched, lo, hi, own)
	if own > 0 && own < first {
		return overridePlan{group: own, end: hi, base: base}, nil
	}
	for _, g := range sched.Groups {
		if g.Index < first && !g.Enabled && g.Index != own {
			return overridePlan{group: g.Index, end: hi, base: base}, nil
		}
	}
	detail := "every schedule group is in use"
	if first <= scheduleGroups {
		detail = fmt.Sprintf("no free group runs before group %d (%s %s–%s), which holds this time; keep group 1 disabled so an override always has a place", first, base.WorkMode, base.Start, base.End)
	}
	return overridePlan{}, &problem{Title: titleOverrideUnavailable, Code: "override_no_group", Detail: detail}
}

// trimWindow cuts the window's end until no other enabled group's window
// crosses it.
func trimWindow(sched Schedule, lo, hi, own int) int {
	for cut := true; cut; {
		cut = false
		for _, g := range sched.Groups {
			if !g.Enabled || g.Index == own {
				continue
			}
			glo, ghi := minuteOf(g.Start), minuteOf(g.End)
			switch {
			case ghi <= lo || glo >= hi: // apart
			case glo <= lo && hi <= ghi: // inside it
			case glo > lo: // it starts inside the window
				hi, cut = glo, true
			default: // the window starts inside it and outlasts it
				hi, cut = ghi, true
			}
		}
	}
	return hi
}

// innermost is the lowest-index enabled group the window lies inside
// (scheduleGroups+1 and a zero group: none).
func innermost(sched Schedule, lo, hi, own int) (int, ScheduleGroup) {
	first := scheduleGroups + 1
	var base ScheduleGroup
	for _, g := range sched.Groups {
		if g.Enabled && g.Index != own && minuteOf(g.Start) <= lo && hi <= minuteOf(g.End) && g.Index < first {
			first, base = g.Index, g
		}
	}
	return first, base
}

// Problem titles and helpers of the override.
const (
	titleOverrideUnavailable = "Schedule override unavailable"
	titleOverrideHeld        = "Schedule override held"
	codeOverrideHeld         = "override_held"
)

func overrideUnavailable(detail string) *problem {
	return &problem{Title: titleOverrideUnavailable, Code: "override_unavailable", Detail: detail}
}

// loadOverride reads the active override (nil: none).
func (s *Server) loadOverride() (*schedOverride, error) {
	v, ok, err := s.st.Setting(overrideKey)
	if err != nil || !ok {
		return nil, err
	}
	var o schedOverride
	if err := json.Unmarshal([]byte(v), &o); err != nil {
		return nil, fmt.Errorf("override: stored state: %w", err)
	}
	return &o, nil
}

// saveOverride stores the active override (nil: clears it).
func (s *Server) saveOverride(o *schedOverride) error {
	if o == nil {
		return s.st.DeleteSetting(overrideKey)
	}
	b, err := json.Marshal(o)
	if err != nil {
		return err
	}
	return s.st.SetSetting(overrideKey, string(b))
}

// currentOverride is the active override for /state (nil: none).
func (s *Server) currentOverride() *ScheduleOverride {
	o, err := s.loadOverride()
	if err != nil || o == nil {
		return nil
	}
	v := o.view()
	return &v
}

// storedSchedule is the table as the store knows it, and when it was
// read or written (zero: not known).
func (s *Server) storedSchedule() (Schedule, time.Time, error) {
	st, err := s.st.QueryState()
	if err != nil {
		return Schedule{}, time.Time{}, err
	}
	if len(st.Schedule) == 0 {
		return Schedule{}, time.Time{}, nil
	}
	cfg := s.scanConfigRegs(st.SchedTS)
	return scheduleOf(st.Schedule, cfg.schedOn), st.SchedTS, nil
}

// ownsGroup reports whether the table still shows the override's group
// as the override wrote it. A table from before the last write was
// confirmed (Written) cannot show it: the group is taken to be the
// override's.
func (o *schedOverride) ownsGroup(sched Schedule, asOf time.Time) bool {
	if !asOf.After(o.Written) || o.Group < 1 || o.Group > len(sched.Groups) {
		return true
	}
	g := sched.Groups[o.Group-1]
	return g.Enabled && g.Start == o.Start && g.WorkMode == o.Mode
}

// tableRequest is a table as a PUT /schedule body.
func tableRequest(sched Schedule) scheduleRequest {
	req := scheduleRequest{Enabled: sched.Enabled, Groups: make([]scheduleGroupReq, len(sched.Groups))}
	for i, g := range sched.Groups {
		req.Groups[i] = scheduleGroupReq{
			Index: g.Index, Enabled: g.Enabled, Start: g.Start, End: g.End, WorkMode: g.WorkMode,
			MaxSocPct: g.MaxSocPct, MinSocOnGridPct: g.MinSocOnGridPct, FdSocPct: g.FdSocPct, FdPwrW: g.FdPwrW,
		}
	}
	return req
}

// clearedGroup is a group as an unused one (what validateSchedule
// writes for a disabled group).
func clearedGroup(k int) scheduleGroupReq {
	return scheduleGroupReq{
		Index: k, Start: hhmmZero, End: hhmmZero, WorkMode: workModeSelfUse,
		MaxSocPct: socMaxPct, MinSocOnGridPct: slotSocMinPct, FdSocPct: slotSocMinPct,
	}
}

// pushTable writes one table through the gateway (C-003) and audits it
// as tk. It returns the command's outcome (status 200 = confirmed) and
// whether a failed write may still have reached the inverter (no
// read-back in time, or queued behind a lost session). It never queues
// on purpose: with no session it refuses, since a queued override
// would land after its caller gave up.
func (s *Server) pushTable(ctx context.Context, tk Token, req scheduleRequest, summary string) (commandResponse, bool) {
	params, readBack, p := validateSchedule(req)
	if p != nil {
		return commandResponse{status: http.StatusConflict, body: *overrideUnavailable(
			"the schedule as it stands does not validate: " + p.Detail + fieldDetail(p))}, false
	}
	if !s.gw.HasFlow() {
		return commandResponse{status: http.StatusServiceUnavailable, body: offlineProblem("no active inverter flow")}, false
	}
	done := make(chan workerOut, 1)
	go s.commandWorker(ctx, tk, gateway.Command{Kind: gateway.KindSchedule, Params: params}, summary, done)
	out := <-done // the worker is bounded by commandTimeout
	unsure := out.submitErr == nil && (out.queued || out.waitErr != nil)
	return s.commandOutcome(out, tk, summary, readBack), unsure
}

// fieldDetail is a problem's field errors as text ("" when none).
func fieldDetail(p *problem) string {
	var out strings.Builder
	for _, e := range p.Errors {
		fmt.Fprintf(&out, " (%s: %s)", e.Field, e.Message)
	}
	return out.String()
}

// handleOverrideGet is GET /schedule/override (spec getScheduleOverride).
func (s *Server) handleOverrideGet(w http.ResponseWriter, _ *http.Request, _ Token) {
	o, err := s.loadOverride()
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, o.view())
}

// handleOverridePut is PUT /schedule/override (spec setScheduleOverride): start, renew
// or change the caller's override.
func (s *Server) handleOverridePut(w http.ResponseWriter, r *http.Request, tk Token) {
	var req overrideRequest
	if err := readBody(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, badField("body", err.Error()))
		return
	}
	now := s.clock()
	if p := req.validate(now); p != nil {
		writeProblem(w, http.StatusBadRequest, *p)
		return
	}
	s.ovMu.Lock()
	defer s.ovMu.Unlock()
	cur, err := s.loadOverride()
	if err != nil {
		s.internal(w, err)
		return
	}
	if cur != nil && cur.HolderID != tk.ID {
		writeProblem(w, http.StatusConflict, problem{
			Title: titleOverrideHeld, Code: codeOverrideHeld,
			Detail: fmt.Sprintf("%q holds the override until %s", cur.Holder, cur.Until.UTC().Format(time.RFC3339)),
		})
		return
	}
	wall, ok := s.wallNow()
	if !ok {
		writeProblem(w, http.StatusServiceUnavailable, problem{
			Title: "Inverter time unknown", Code: "wall_time_unknown",
			Detail: "the inverter's time is not known yet (no clock reading and no site time zone); its schedule runs on it",
		})
		return
	}
	sched, asOf, err := s.storedSchedule()
	if err != nil {
		s.internal(w, err)
		return
	}
	if asOf.IsZero() {
		writeProblem(w, http.StatusServiceUnavailable, problem{
			Title: "Schedule unknown", Code: "schedule_unknown",
			Detail: "the inverter's schedule has not been read yet; retry once the settings are read",
		})
		return
	}
	next, resp := s.applyOverride(r.Context(), tk, cur, req, sched, asOf, wall, now)
	if resp.status != http.StatusOK {
		s.writeCommand(w, resp)
		return
	}
	s.wakeOverrides()
	writeJSON(w, http.StatusOK, next.view())
}

// applyOverride plans and writes the caller's override (the lock is
// held). It returns the stored override on success.
func (s *Server) applyOverride(ctx context.Context, tk Token, cur *schedOverride, req overrideRequest,
	sched Schedule, asOf, wall, now time.Time,
) (*schedOverride, commandResponse) {
	nowMin := minuteOfDay(wall)
	lo, own := windowStart(cur, sched, asOf, wall)
	hi := askedEnd(wall, req.Until.Sub(now))
	if hi <= nowMin {
		return nil, commandResponse{status: http.StatusConflict, body: *overrideUnavailable(
			"the inverter's day ends before the override can start; retry after midnight (inverter time)")}
	}
	plan, p := planOverride(sched, lo, hi, own)
	if p != nil {
		return nil, commandResponse{status: http.StatusConflict, body: *p}
	}
	if plan.end <= nowMin {
		return nil, commandResponse{status: http.StatusConflict, body: *overrideUnavailable("another group's window begins now")}
	}
	table := tableRequest(sched)
	if own > 0 && own != plan.group {
		table.Groups[own-1] = clearedGroup(own)
	}
	g := overrideGroup(plan, req, lo)
	table.Groups[plan.group-1] = g
	dayStart := time.Date(wall.Year(), wall.Month(), wall.Day(), 0, 0, 0, 0, wall.Location())
	until := now.Add(dayStart.Add(time.Duration(plan.end) * time.Minute).Sub(wall)).Truncate(time.Second)
	next := &schedOverride{
		Holder: tk.Name, HolderID: tk.ID, Role: tk.Role,
		Mode: req.Mode, FdSocPct: g.FdSocPct, FdPwrW: g.FdPwrW,
		Group: plan.group, Day: wall.Format(time.DateOnly), Start: g.Start, End: g.End,
		Until: until.UTC(), Asked: req.Until.UTC(), Since: now.UTC(), Written: now,
	}
	renewal := cur != nil && own == plan.group && lo == minuteOf(cur.Start) && cur.Mode == req.Mode
	if renewal {
		next.Since = cur.Since
	}
	summary := fmt.Sprintf("override %s %s–%s in group %d (cut-off %d%%, %dW)", g.WorkMode, g.Start, g.End, g.Index, g.FdSocPct, g.FdPwrW)
	if resp := s.commitOverride(ctx, tk, cur, next, table, summary); resp.status != http.StatusOK {
		return nil, resp
	}
	if !renewal {
		s.bus.PublishSystem(SystemEvent{Type: SystemScheduleOverrideStarted, Detail: fmt.Sprintf(
			"%s started a %s at %d W until %s (inverter time), cut-off %d %%", tk.Name, req.Mode, g.FdPwrW, g.End, g.FdSocPct)})
	}
	return next, commandResponse{status: http.StatusOK}
}

// commitOverride stores next, writes the table, and keeps the stored
// override in step with what the inverter may hold.
func (s *Server) commitOverride(ctx context.Context, tk Token, cur, next *schedOverride, table scheduleRequest, summary string) commandResponse {
	// stored first: a group on the inverter is always one the expiry loop
	// knows to clear (a restart in between included)
	if err := s.saveOverride(next); err != nil {
		return commandResponse{status: http.StatusInternalServerError, body: problem{
			Title: titleInternal, Code: codeInternal, Detail: "the override could not be stored; nothing was written",
		}}
	}
	resp, unsure := s.pushTable(ctx, tk, table, summary)
	if resp.status != http.StatusOK {
		if !unsure {
			// confirmed not written: the previous override stands
			if err := s.saveOverride(cur); err != nil {
				slog.Error("override: previous state not restored", "err", err)
			}
		}
		return resp
	}
	next.Written = s.clock()
	if err := s.saveOverride(next); err != nil {
		slog.Error("override: write time not stored", "err", err)
	}
	return resp
}

// minutesPerHour converts minutes of the day to HH:MM.
const minutesPerHour = 60

// minuteOfDay is t's minute of its day.
func minuteOfDay(t time.Time) int { return t.Hour()*minutesPerHour + t.Minute() }

// hhmmOf is minutes of the day as HH:MM.
func hhmmOf(m int) string { return hhmm(m/minutesPerHour, m%minutesPerHour) }

// windowStart is the window's start and the group the override holds
// (0: none): a running override keeps its start (a renewal moves the
// end), else it starts now.
func windowStart(cur *schedOverride, sched Schedule, asOf, wall time.Time) (int, int) {
	nowMin := minuteOfDay(wall)
	if cur == nil || !cur.ownsGroup(sched, asOf) {
		return nowMin, 0
	}
	if cur.Day == wall.Format(time.DateOnly) && minuteOf(cur.Start) <= nowMin && nowMin < minuteOf(cur.End) {
		return minuteOf(cur.Start), cur.Group
	}
	return nowMin, cur.Group
}

// askedEnd is the asked end (ahead of now) on the inverter's clock, in
// minutes of its day rounded up; a window cannot pass midnight (the
// holder renews after it).
func askedEnd(wall time.Time, ahead time.Duration) int {
	at := wall.Add(ahead)
	if at.Format(time.DateOnly) != wall.Format(time.DateOnly) {
		return dayLastMinute
	}
	hi := minuteOfDay(at)
	if at.Second() > 0 || at.Nanosecond() > 0 {
		hi++
	}
	return min(hi, dayLastMinute)
}

// overrideGroup is the override's schedule group (max SoC and min SoC on
// grid from the group it runs inside; else 100 / 10).
func overrideGroup(plan overridePlan, req overrideRequest, lo int) scheduleGroupReq {
	maxSoc, minSoc := socMaxPct, slotSocMinPct
	if plan.base.Enabled {
		maxSoc, minSoc = plan.base.MaxSocPct, plan.base.MinSocOnGridPct
	}
	return scheduleGroupReq{
		Index: plan.group, Enabled: true, Start: hhmmOf(lo), End: hhmmOf(plan.end), WorkMode: req.Mode,
		MaxSocPct: maxSoc, MinSocOnGridPct: minSoc, FdSocPct: *req.FdSocPct, FdPwrW: *req.FdPwrW,
	}
}

// handleOverrideDelete is DELETE /schedule/override (spec clearScheduleOverride): the
// holder, or an expert or installer, ends the override now.
func (s *Server) handleOverrideDelete(w http.ResponseWriter, r *http.Request, tk Token) {
	s.ovMu.Lock()
	defer s.ovMu.Unlock()
	cur, err := s.loadOverride()
	if err != nil {
		s.internal(w, err)
		return
	}
	if cur == nil {
		writeJSON(w, http.StatusOK, ScheduleOverride{})
		return
	}
	if cur.HolderID != tk.ID && roleLevel[tk.Role] < rankExpert {
		writeProblem(w, http.StatusForbidden, problem{
			Title: titleOverrideHeld, Code: codeOverrideHeld,
			Detail: fmt.Sprintf("%q holds the override; only its holder, an expert or an installer ends it", cur.Holder),
		})
		return
	}
	resp := s.endOverride(r.Context(), tk, cur, "ended by "+tk.Name)
	if resp.status != http.StatusOK {
		s.writeCommand(w, resp)
		return
	}
	writeJSON(w, http.StatusOK, ScheduleOverride{})
}

// endOverride clears the override's group (when the table still shows
// it as written) and forgets the override (the lock is held). A failed
// write keeps it: the expiry loop tries again.
func (s *Server) endOverride(ctx context.Context, tk Token, cur *schedOverride, why string) commandResponse {
	sched, asOf, err := s.storedSchedule()
	if err != nil {
		return commandResponse{status: http.StatusInternalServerError, body: problem{Title: titleInternal, Code: codeInternal, Detail: "the schedule could not be read"}}
	}
	owns := cur.ownsGroup(sched, asOf)
	if owns && !asOf.IsZero() {
		table := tableRequest(sched)
		table.Groups[cur.Group-1] = clearedGroup(cur.Group)
		summary := fmt.Sprintf("override %s %s–%s in group %d cleared (%s)", cur.Mode, cur.Start, cur.End, cur.Group, why)
		if resp, _ := s.pushTable(ctx, tk, table, summary); resp.status != http.StatusOK {
			return resp // kept: the expiry loop tries again
		}
	} else if !owns {
		slog.Info("override: its group was changed by someone else; left as it is", "group", cur.Group)
	}
	if err := s.saveOverride(nil); err != nil {
		return commandResponse{status: http.StatusInternalServerError, body: problem{Title: titleInternal, Code: codeInternal, Detail: "the override could not be forgotten"}}
	}
	s.bus.PublishSystem(SystemEvent{Type: SystemScheduleOverrideEnded, Detail: fmt.Sprintf(
		"%s's %s (%s–%s) ended: %s", cur.Holder, cur.Mode, cur.Start, cur.End, why)})
	return commandResponse{status: http.StatusOK}
}

// wakeChan is the expiry loop's wake channel, made on first use.
func (s *Server) wakeChan() chan struct{} {
	s.ovWakeM.Lock()
	defer s.ovWakeM.Unlock()
	if s.ovWake == nil {
		s.ovWake = make(chan struct{}, 1)
	}
	return s.ovWake
}

// wakeOverrides tells the expiry loop the override changed.
func (s *Server) wakeOverrides() {
	select {
	case s.wakeChan() <- struct{}{}:
	default:
	}
}

// RunOverrides is the override's expiry loop (until ctx is done): it
// clears the override's group once its window has ended, after a
// restart too, retrying a failed write.
func (s *Server) RunOverrides(ctx context.Context) {
	wake := s.wakeChan()
	for {
		wait := s.overrideTick(ctx)
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		case <-wake:
			t.Stop()
		}
	}
}

// overrideTick ends an expired override and returns how long to wait
// for the next look.
func (s *Server) overrideTick(ctx context.Context) time.Duration {
	if !s.ovMu.TryLock() {
		return overrideRetry // a request holds it; it wakes the loop when done
	}
	defer s.ovMu.Unlock()
	cur, err := s.loadOverride()
	if err != nil {
		slog.Error("override: state unreadable", "err", err)
		return overrideRetry
	}
	if cur == nil {
		return overrideIdle
	}
	now := s.clock()
	if due := cur.Until.Add(overrideGrace); now.Before(due) {
		return due.Sub(now)
	}
	tk := Token{ID: cur.HolderID, Name: cur.Holder + " (override expiry)", Role: cur.Role}
	if resp := s.endOverride(ctx, tk, cur, "expired"); resp.status != http.StatusOK {
		slog.Warn("override: clear failed; retrying", "status", resp.status, "problem", resp.body)
		return overrideRetry
	}
	return overrideIdle
}
