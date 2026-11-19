package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"time"

	"foxesslocal/internal/gateway"
)

// commandResponse is one terminal outcome of a synchronous write:
// status 0 = the client went away (nothing to write), 200 = the body,
// else a problem.
type commandResponse struct {
	status     int
	body       any
	retryAfter time.Duration // > 0: a Retry-After header (write_budget)
}

// commandAuditDetail is the /audit row for one operator command (the
// spec's command source identity: the token name + role).
type commandAuditDetail struct {
	Source     string `json:"source"`
	SourceRole string `json:"source_role"`
	Action     string `json:"action"`
	Summary    string `json:"summary"`
	Confirmed  string `json:"confirmed"`
}

// handleWorkMode is PUT /work-mode (spec setWorkMode).
func (s *Server) handleWorkMode(w http.ResponseWriter, r *http.Request, tk Token) {
	var body struct {
		WorkMode string `json:"work_mode"`
	}
	if err := readBody(r, &body); err != nil {
		writeProblem(w, http.StatusBadRequest, badField("body", err.Error()))
		return
	}
	val, err := parseWorkMode(body.WorkMode)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, badField("work_mode", err.Error()))
		return
	}
	resp := s.runCommand(r, tk, gateway.Command{
		Kind:   gateway.KindWorkMode,
		Params: map[gateway.Param]int{gateway.ParamMode: val},
	}, fmt.Sprintf("work-mode %s (41000=%d)", body.WorkMode, val),
		map[string]string{"work_mode": body.WorkMode})
	s.writeCommand(w, resp)
}

// handleMeterCompensation is PUT /meter-compensation (spec
// setMeterCompensation).
func (s *Server) handleMeterCompensation(w http.ResponseWriter, r *http.Request, tk Token) {
	var body struct {
		W *int `json:"w"`
	}
	if err := readBody(r, &body); err != nil {
		writeProblem(w, http.StatusBadRequest, badField("body", err.Error()))
		return
	}
	if body.W == nil {
		writeProblem(w, http.StatusBadRequest, badField("w", "required"))
		return
	}
	if *body.W < meterCompMin || *body.W > meterCompMax {
		writeProblem(w, http.StatusBadRequest, badField("w", "must be in -500..500"))
		return
	}
	resp := s.runCommand(r, tk, gateway.Command{
		Kind:   gateway.KindMeterCompensation,
		Params: map[gateway.Param]int{gateway.ParamValue: *body.W},
	}, fmt.Sprintf("meter-compensation %dW (41029=%d)", *body.W, *body.W),
		map[string]int{"w": *body.W})
	s.writeCommand(w, resp)
}

// handleLimits is PUT /limits (spec setLimits): at least one of
// import_w / export_w, each 0..30000.
func (s *Server) handleLimits(w http.ResponseWriter, r *http.Request, tk Token) {
	var body limitsRequest
	if err := readBody(r, &body); err != nil {
		writeProblem(w, http.StatusBadRequest, badField("body", err.Error()))
		return
	}
	if body.ImportW == nil && body.ExportW == nil {
		p := badField("import_w", "at least one of import_w / export_w is required")
		p.Errors = append(p.Errors, fieldError{Field: "export_w", Message: "at least one of import_w / export_w is required"})
		writeProblem(w, http.StatusBadRequest, p)
		return
	}
	for _, f := range []struct {
		name string
		v    *int
	}{{"import_w", body.ImportW}, {"export_w", body.ExportW}} {
		if f.v != nil && (*f.v < limitMin || *f.v > limitMax) {
			writeProblem(w, http.StatusBadRequest, badField(f.name, "must be in 0..30000"))
			return
		}
	}
	params := map[gateway.Param]int{}
	if body.ImportW != nil {
		params[gateway.ParamImport] = *body.ImportW
	}
	if body.ExportW != nil {
		params[gateway.ParamExport] = *body.ExportW
	}
	resp := s.runCommand(r, tk, gateway.Command{
		Kind:   gateway.KindLimits,
		Params: params,
	}, limitsSummary(body), Limits{
		ImportW: derefLimit(body.ImportW),
		ExportW: derefLimit(body.ExportW),
	})
	s.writeCommand(w, resp)
}

// handleSchedule is PUT /schedule (spec setSchedule): the 8-group
// table, validated at the API level (window + containment overlap)
// before the gateway push.
func (s *Server) handleSchedule(w http.ResponseWriter, r *http.Request, tk Token) {
	var req scheduleRequest
	if err := readBody(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, badField("body", err.Error()))
		return
	}
	params, readBack, p := validateSchedule(req)
	if p != nil {
		writeProblem(w, http.StatusBadRequest, *p)
		return
	}
	resp := s.runCommand(r, tk, gateway.Command{
		Kind:   gateway.KindSchedule,
		Params: params,
	}, "schedule push (48000 enabled, 48010 x80)", readBack)
	s.writeCommand(w, resp)
}

// runCommand executes one synchronous write (C-003: the gateway is the
// sole upstream path): Submit off the request goroutine (the injection
// can block on the inverter's TCP window), the read-back Wait with the
// spec timeout, and the command audit row. It returns status 0 when
// the client went away (the worker still completes the audit).
func (s *Server) runCommand(r *http.Request, tk Token, cmd gateway.Command, summary string, readBack any) commandResponse {
	done := make(chan workerOut, 1)
	go s.commandWorker(r.Context(), tk, cmd, summary, done)
	select {
	case out := <-done:
		switch {
		case out.submitErr != nil:
			if errors.Is(out.submitErr, gateway.ErrWriteBudget) {
				// the caller is often an automation: tell the owner too (F2)
				s.bus.PublishSystem(SystemEvent{
					Type:   SystemAlert,
					Detail: fmt.Sprintf("write refused for %s by %s: %v", summary, tk.Name, out.submitErr),
				})
			}
			return s.submitResponse(out.submitErr)
		case out.queued:
			return commandResponse{status: http.StatusServiceUnavailable, body: problem{
				Title:  "Inverter offline",
				Code:   "inverter_offline",
				Detail: "no active inverter flow; the command stays queued",
			}}
		case out.waitErr == nil && out.res.Status == gateway.StatusAcked:
			if fromResult, ok := readBack.(func(gateway.Result) any); ok {
				return commandResponse{status: http.StatusOK, body: fromResult(out.res)}
			}
			return commandResponse{status: http.StatusOK, body: readBack}
		case out.waitErr == nil && (out.res.Status == gateway.StatusFailed || out.res.Status == gateway.StatusExpired):
			// The device read back a contradiction (failed) or the
			// gateway's TTL elapsed (expired): the command is confirmed
			// not applied — report the real detail, not a timeout.
			detail := out.res.Detail
			if detail == "" {
				detail = "the command expired before the device read back"
			}
			return commandResponse{status: http.StatusBadGateway, body: problem{
				Title:   "Command failed",
				Code:    "command_failed",
				Applied: confirmedNo,
				Detail:  detail,
			}}
		default:
			return commandResponse{status: http.StatusGatewayTimeout, body: problem{
				Title:   "Command failed",
				Code:    "command_failed",
				Applied: confirmedUnknown,
				Detail:  "no device read-back within the timeout",
			}}
		}
	case <-r.Context().Done():
		return commandResponse{}
	}
}

// workerOut is one terminal outcome of the command worker.
type workerOut struct {
	submitErr error
	queued    bool
	res       gateway.Result
	waitErr   error
}

// commandWorker runs the submit → read-back → audit sequence for one
// command. The context is detached from the request (the client going
// away must not abandon an in-flight device write — the audit row must
// record what the device actually did).
func (s *Server) commandWorker(ctx context.Context, tk Token, cmd gateway.Command, summary string, done chan<- workerOut) {
	var out workerOut
	id, err := s.gw.Submit(context.WithoutCancel(ctx), cmd)
	if err != nil {
		out.submitErr = err
		done <- out
		return
	}
	// Queued with no flow = the inverter is offline (the command waits
	// for the next session). Queued with a flow = behind a poll read
	// (~tens of ms): wait for it like any in-flight command.
	if !s.gw.HasFlow() {
		for _, p := range s.gw.Pending() {
			if p.ID == id && p.Status == gateway.StatusQueued {
				out.queued = true
				break
			}
		}
	}
	if out.queued {
		s.auditCommand(tk, summary, confirmedNotApplicable)
		done <- out
		return
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commandTimeout)
	defer cancel()
	out.res, out.waitErr = s.gw.Wait(wctx, id)
	confirmed := confirmedUnknown
	if out.waitErr == nil {
		switch out.res.Status {
		case gateway.StatusAcked:
			confirmed = confirmedYes
		case gateway.StatusFailed, gateway.StatusExpired:
			confirmed = confirmedNo
		case gateway.StatusQueued, gateway.StatusInFlight:
			// Queued/InFlight are not Wait results (Wait blocks
			// until the terminal state).
			confirmed = confirmedUnknown
		}
	}
	s.auditCommand(tk, auditSummary(summary, out.res), confirmed)
	done <- out
}

// auditSummary marks a command the inverter already held: acknowledged
// without a write (the gateway's read-before-write found the values).
func auditSummary(summary string, res gateway.Result) string {
	if res.Status == gateway.StatusAcked && res.Unchanged {
		return summary + " — unchanged, not written"
	}
	return summary
}

// writeCommand sends the command response (status 0 = the client went
// away).
func (s *Server) writeCommand(w http.ResponseWriter, resp commandResponse) {
	if resp.status == 0 {
		return
	}
	if resp.retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(resp.retryAfter.Seconds()))))
	}
	if p, ok := resp.body.(problem); ok && resp.status != http.StatusOK {
		writeProblem(w, resp.status, p)
		return
	}
	writeJSON(w, resp.status, resp.body)
}

// submitResponse maps one gateway Submit error to the spec's codes.
func (s *Server) submitResponse(err error) commandResponse {
	if be, ok := errors.AsType[*gateway.BudgetError](err); ok {
		zero := 0
		next := be.NextFree
		return commandResponse{status: http.StatusTooManyRequests, body: problem{
			Title: "Too many writes", Code: "write_budget", Detail: err.Error(),
			Setting: kindFeature[be.Kind], Remaining: &zero, NextFreeAt: &next,
		}, retryAfter: max(time.Second, next.Sub(s.clock()))}
	}
	return submitResponse(err)
}

// submitResponse maps one gateway Submit error to the spec's codes.
func submitResponse(err error) commandResponse {
	if resp, ok := writeGateResponse(err); ok {
		return resp
	}
	switch {
	case errors.Is(err, gateway.ErrWriteBudget):
		return commandResponse{status: http.StatusTooManyRequests, body: problem{
			Title:  "Too many writes",
			Code:   "write_budget",
			Detail: err.Error(),
		}}
	case errors.Is(err, gateway.ErrNoFlow): // a clock value is never queued
		return commandResponse{status: http.StatusServiceUnavailable, body: problem{
			Title:  "Inverter offline",
			Code:   "inverter_offline",
			Detail: "no active inverter flow",
		}}
	case errors.Is(err, gateway.ErrInFlight):
		return commandResponse{status: http.StatusConflict, body: problem{
			Title:  "Command in flight",
			Code:   "command_in_flight",
			Detail: err.Error(),
		}}
	case errors.Is(err, gateway.ErrScheduleOverlap):
		return commandResponse{status: http.StatusBadRequest, body: problem{
			Title:  "Schedule overlap",
			Code:   "schedule_overlap",
			Detail: err.Error(),
		}}
	default:
		return commandResponse{status: http.StatusBadRequest, body: problem{
			Title:  "Invalid value",
			Code:   "invalid_value",
			Detail: err.Error(),
		}}
	}
}

// auditCommand writes the operator-command audit row (the /audit
// source identity for local writes: the token name).
func (s *Server) auditCommand(tk Token, summary, confirmed string) {
	d := commandAuditDetail{
		Source:     tk.Name,
		SourceRole: string(tk.Role),
		Action:     auditActionAllow,
		Summary:    summary,
		Confirmed:  confirmed,
	}
	b, err := json.Marshal(d)
	if err != nil { // strings and a bool: cannot fail, but never lose the row
		b = []byte(summary)
	}
	s.st.RecordAudit(s.clock(), auditKindCommand, string(b))
	// The store batch-writer only auto-flushes at 128 records / the 250 ms
	// tick; flush now so the row is visible to a subsequent GET /audit.
	s.st.Flush()
}

// confirmed values (the spec's audit Confirmed enum).
const (
	confirmedYes           = "yes"
	confirmedNo            = "no"
	confirmedUnknown       = "unknown"
	confirmedNotApplicable = "not-applicable"
)

// Command value bounds (the spec's schema bounds; the gateway
// re-validates — the API answers with the spec's field-level 400s).
const (
	meterCompMin = -500
	meterCompMax = 500
	limitMin     = 0
	limitMax     = 30000
)

// parseWorkMode maps the spec's work-mode names to the wire values.
func parseWorkMode(v string) (int, error) {
	switch v {
	case workModeSelfUse:
		return 0, nil
	case workModeFeedIn:
		return 1, nil
	}
	return 0, fmt.Errorf("must be self-use or feed-in")
}

// derefLimit unwraps one limits field (absent = 0, the spec's
// "unchanged" read-back for the field that was not set).
func derefLimit(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

// limitsRequest is the PUT /limits body (each field optional; at
// least one required).
type limitsRequest struct {
	ImportW *int `json:"import_w"`
	ExportW *int `json:"export_w"`
}

// limitsSummary is the audit text for one limits command.
func limitsSummary(body limitsRequest) string {
	imp := "unset"
	if body.ImportW != nil {
		imp = strconv.Itoa(*body.ImportW) + "W"
	}
	exp := "unset"
	if body.ExportW != nil {
		exp = strconv.Itoa(*body.ExportW) + "W"
	}
	return fmt.Sprintf("limits import=%s export=%s (41021/41023)", imp, exp)
}

// scheduleRequest is the spec's Schedule (the PUT /schedule body).
type scheduleRequest struct {
	Enabled bool               `json:"enabled"`
	Groups  []scheduleGroupReq `json:"groups"`
}

// scheduleGroupReq is one spec ScheduleGroup (the request form).
type scheduleGroupReq struct {
	Index           int    `json:"index"`
	Enabled         bool   `json:"enabled"`
	Start           string `json:"start"`
	End             string `json:"end"`
	WorkMode        string `json:"work_mode"`
	MaxSocPct       int    `json:"max_soc_pct"`
	MinSocOnGridPct int    `json:"min_soc_on_grid_pct"`
	FdSocPct        int    `json:"fd_soc_pct"`
	FdPwrW          int    `json:"fd_pwr_w"`
}

// hhmmRe is the spec's HH:MM pattern (00:00..23:59).
var hhmmRe = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)

// parseHHMM maps one HH:MM string to minutes (a bad value errors).
func parseHHMM(v string) (int, error) {
	if !hhmmRe.MatchString(v) {
		return 0, fmt.Errorf("must be HH:MM (00:00..23:59)")
	}
	h, _ := strconv.Atoi(v[0:2])
	m, _ := strconv.Atoi(v[3:5])
	return h*60 + m, nil
}

// scheduleWindow is one enabled window (minutes) for the overlap
// check.
type scheduleWindow struct {
	lo, hi int
	idx    int
}

// scheduleGroups is the 48010 group count (the spec's Schedule.groups
// fixed size).
const scheduleGroups = 8

// hhmmZero is the normalized zero time for unused groups.
const hhmmZero = "00:00"

// Enabled-group bounds: the installer schema's segmented-time ranges
// (k121__segmented_time_mode, dump 2026-11-16; the gateway re-checks).
const (
	slotSocMinPct = 10
	socMaxPct     = 100
	slotFdPwrMaxW = 10500
)

// validateSchedule checks the request (group count, and per group the
// HH:MM, work-mode, range, and window rules, then the containment-only
// overlap rule) and returns the gateway params plus the normalized
// read-back (unused groups disabled and zeroed).
func validateSchedule(req scheduleRequest) (map[gateway.Param]int, Schedule, *problem) {
	if len(req.Groups) != scheduleGroups {
		return nil, Schedule{}, badFieldPtr("groups", fmt.Sprintf("exactly %d groups are required", scheduleGroups))
	}
	params := map[gateway.Param]int{gateway.ParamEnabled: 0}
	if req.Enabled {
		params[gateway.ParamEnabled] = 1
	}
	resp := Schedule{Enabled: req.Enabled, Groups: make([]ScheduleGroup, scheduleGroups)}
	var wins []scheduleWindow
	for i := range req.Groups {
		gp, out, start, end, perr := validateScheduleGroup(i+1, req.Groups[i])
		if perr != nil {
			return nil, Schedule{}, perr
		}
		maps.Copy(params, gp)
		if out.Enabled {
			wins = append(wins, scheduleWindow{lo: start, hi: end, idx: i})
		}
		resp.Groups[i] = out
	}
	if err := checkScheduleOverlap(wins); err != nil {
		return nil, Schedule{}, err
	}
	return params, resp, nil
}

// validateScheduleGroup validates one group's fields (HH:MM, the
// work-mode name; for an enabled group the 10..100 SoC fields, the
// 0..10500 power field, end > start) and returns its wire params (keys "<group>_<field>"), the
// normalized read-back group, and — for an enabled group — the window
// bounds in minutes (the group index is 1-based, the spec's).
func validateScheduleGroup(k int, g scheduleGroupReq) (map[gateway.Param]int, ScheduleGroup, int, int, *problem) {
	prefix := fmt.Sprintf("groups[%d]", k-1)
	start, err := parseHHMM(g.Start)
	if err != nil {
		return nil, ScheduleGroup{}, 0, 0, badFieldPtr(prefix+".start", err.Error())
	}
	end, err := parseHHMM(g.End)
	if err != nil {
		return nil, ScheduleGroup{}, 0, 0, badFieldPtr(prefix+".end", err.Error())
	}
	wm, err := parseScheduleWorkMode(g.WorkMode)
	if err != nil {
		return nil, ScheduleGroup{}, 0, 0, badFieldPtr(prefix+".work_mode", err.Error())
	}
	params := map[gateway.Param]int{}
	out := ScheduleGroup{Index: k, Start: g.Start, End: g.End}
	if !g.Enabled {
		// An unused group is normalized: the wire carries a cleared
		// slot, which the gateway writes in the cloud's unused-group
		// shape — the read-back shows that shape (as /state will).
		for _, f := range gateway.SlotFields {
			params[gateway.SlotParam(k, f)] = 0
		}
		out.WorkMode = workModeSelfUse
		out.Start, out.End = hhmmZero, hhmmZero
		out.MaxSocPct, out.MinSocOnGridPct, out.FdSocPct = socMaxPct, slotSocMinPct, slotSocMinPct
		return params, out, 0, 0, nil
	}
	socFields := []struct {
		field string
		v     int
	}{
		{prefix + ".max_soc_pct", g.MaxSocPct},
		{prefix + ".min_soc_on_grid_pct", g.MinSocOnGridPct},
		{prefix + ".fd_soc_pct", g.FdSocPct},
	}
	for _, f := range socFields {
		if f.v < slotSocMinPct || f.v > socMaxPct {
			return nil, ScheduleGroup{}, 0, 0, badFieldPtr(f.field, "must be in 10..100")
		}
	}
	if g.FdPwrW < 0 || g.FdPwrW > slotFdPwrMaxW {
		return nil, ScheduleGroup{}, 0, 0, badFieldPtr(prefix+".fd_pwr_w", "must be in 0..10500")
	}
	if end <= start {
		return nil, ScheduleGroup{}, 0, 0, badFieldPtr(prefix+".end", "end must be after start")
	}
	for f, v := range map[gateway.SlotField]int{
		gateway.SlotEnabled:  1,
		gateway.SlotStart:    packedHHMM(start),
		gateway.SlotEnd:      packedHHMM(end),
		gateway.SlotWorkMode: wm,
		gateway.SlotMaxSoc:   g.MaxSocPct,
		gateway.SlotMinSoc:   g.MinSocOnGridPct,
		gateway.SlotFdSoc:    g.FdSocPct,
		gateway.SlotFdPwr:    g.FdPwrW,
	} {
		params[gateway.SlotParam(k, f)] = v
	}
	out.Enabled = true
	out.WorkMode = g.WorkMode
	out.MaxSocPct = g.MaxSocPct
	out.MinSocOnGridPct = g.MinSocOnGridPct
	out.FdSocPct = g.FdSocPct
	out.FdPwrW = g.FdPwrW
	return params, out, start, end, nil
}

// badFieldPtr is badField with a pointer (the validate helpers
// return *problem, nil for the ok path).
func badFieldPtr(field, msg string) *problem {
	p := badField(field, msg)
	return &p
}

// packedHHMM packs minutes into the wire register (h<<8|m).
func packedHHMM(minutes int) int {
	return (minutes/60)<<8 | minutes%60
}

// parseScheduleWorkMode maps the spec's schedule work-mode names to
// the wire enum (1..7).
func parseScheduleWorkMode(v string) (int, error) {
	for wire := range scheduleWorkModes {
		if scheduleWorkModes[wire] == v {
			return wire, nil
		}
	}
	return 0, fmt.Errorf("must be one of self-use, feed-in, backup, peak-shaving, power-station, force-charge, force-discharge")
}

// checkScheduleOverlap enforces the containment-only rule (partial
// overlap of two enabled windows is 400 schedule_overlap; full
// containment and adjacency are legal).
func checkScheduleOverlap(wins []scheduleWindow) *problem {
	sort.Slice(wins, func(i, j int) bool { return wins[i].lo < wins[j].lo })
	for i := range wins {
		for j := i + 1; j < len(wins); j++ {
			a, b := wins[i], wins[j]
			if b.lo >= a.hi {
				break
			}
			aIn := a.lo <= b.lo && b.hi <= a.hi
			bIn := b.lo <= a.lo && a.hi <= b.hi
			if !aIn && !bIn {
				return &problem{
					Title:  "Schedule overlap",
					Code:   "schedule_overlap",
					Detail: fmt.Sprintf("groups %d and %d overlap", a.idx+1, b.idx+1),
				}
			}
		}
	}
	return nil
}
