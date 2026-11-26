package api

import (
	"fmt"
	"net/http"
	"time"
	"unicode/utf8"

	"foxesslocal/internal/bundle"
	"foxesslocal/internal/compat"
	"foxesslocal/internal/scan"
)

// Problem report limits.
const (
	reportMaxDescription = 4000 // characters
	reportDefaultHours   = 24
	reportMaxEvents      = 200
	auditKindReport      = "report-export"
)

// reportEventKinds are the audit kinds a problem report includes: what
// went wrong or changed (not the routine frame traffic).
var reportEventKinds = []string{
	"ack-error", "inject-error", "serve", "reconnect", "flow-open", "flow-close", "tls", "cloud-drop",
	auditKindCommand, auditKindMaintenance, auditKindUpdate, "setting", auditKindWriteOverride,
}

// handleReport is POST /report (installer, F8): a problem report — the
// de-identified diagnostic bundle of the last `hours` plus report.json
// (the tester's description, the app's facts and recent problem events).
// Nothing is uploaded: the tester downloads it and attaches it to an
// issue (/config issue_url).
func (s *Server) handleReport(w http.ResponseWriter, r *http.Request, tk Token) {
	var body struct {
		Description string `json:"description"`
		Hours       *int   `json:"hours"`
	}
	if err := readBody(r, &body); err != nil {
		writeProblem(w, http.StatusBadRequest, badField("body", err.Error()))
		return
	}
	if utf8.RuneCountInString(body.Description) > reportMaxDescription {
		writeProblem(w, http.StatusBadRequest, badField("description", fmt.Sprintf("at most %d characters", reportMaxDescription)))
		return
	}
	hours := reportDefaultHours
	if body.Hours != nil {
		hours = *body.Hours
	}
	if hours < 1 || hours > bundleMaxHours {
		writeProblem(w, http.StatusBadRequest, badField("hours", fmt.Sprintf("must be 1..%d", bundleMaxHours)))
		return
	}
	in, err := s.bundleInput(hours)
	if err != nil {
		s.internal(w, err)
		return
	}
	facts, err := s.reportFacts()
	if err != nil {
		s.internal(w, err)
		return
	}
	events, err := s.st.QueryAudit(in.Now.Add(-time.Duration(hours)*time.Hour), reportEventKinds...)
	if err != nil {
		s.internal(w, err)
		return
	}
	if len(events) > reportMaxEvents {
		events = events[len(events)-reportMaxEvents:]
	}
	in.Report = &bundle.Report{Description: body.Description, Facts: facts}
	in.Events = events
	if s.scanner != nil && s.scanner.Status().State == scan.StateDone {
		in.Scan = s.scanner.Results()
	}
	s.sendBundle(w, in, "foxess-report", auditKindReport, fmt.Sprintf("report, %dh, %d events", hours, len(events)), tk)
}

// reportFacts is what the app knows about itself and the device.
func (s *Server) reportFacts() (map[string]any, error) {
	qs, err := s.st.QueryState()
	if err != nil {
		return nil, err
	}
	st, err := s.buildState(qs)
	if err != nil {
		return nil, err
	}
	v := s.info.values()
	facts := map[string]any{
		"app_version":      s.info.Version,
		"model":            st.Identity.Model,
		"firmware":         st.Identity.Firmware,
		"verified_profile": st.Identity.VerifiedProfile,
		"compatibility":    st.Identity.Compatibility,
		"writes":           st.Identity.Writes,
		"forward_mode":     st.ForwardMode,
		"online":           st.Online,
		"settings": map[string]any{
			"battery_capacity_wh": v.CapacityWh,
			"public_read_only":    v.PublicReadOnly,
			"public_all_pages":    v.PublicAllPages,
		},
		"setup_checks": s.setupChecks(st),
		// which inverter quirks the reporter's pages compensated for
		"quirks": v.Quirks,
	}
	if st.AsOf != nil {
		facts["data_age_s"] = int(s.clock().Sub(*st.AsOf).Seconds())
	}
	if d, ok := s.inverterClockOffset(); ok {
		facts["inverter_clock_offset_s"] = d
	}
	return facts, nil
}

// reportRoutes registers the problem report and the device scan (F8).
func (s *Server) reportRoutes(mux *http.ServeMux) {
	s.handle(mux, "POST /api/v1/report", s.protect(RoleInstaller, s.handleReport))
	s.handle(mux, "POST /api/v1/device-scan", s.protect(RoleInstaller, s.handleScanStart))
	s.handle(mux, "GET /api/v1/device-scan", s.protect(RoleInstaller, s.handleScanStatus))
}

// WithScanner offers read-only device scans (F8).
func WithScanner(sc *scan.Scanner) ServerOption {
	return func(s *Server) { s.scanner = sc }
}

// auditKindScan is the audit kind of a started device scan.
const auditKindScan = "device-scan"

// handleScanStart is POST /device-scan (installer): a read-only scan of
// every register the identified model's candidate maps name (identity
// blocks only for an unidentified one); 202 with the status, 409 while
// one runs. Its results join the next problem report.
func (s *Server) handleScanStart(w http.ResponseWriter, _ *http.Request, tk Token) {
	if s.scanner == nil {
		writeProblem(w, http.StatusServiceUnavailable, problem{Title: "Scan unavailable", Code: "scan_unavailable"})
		return
	}
	qs, err := s.st.QueryState()
	if err != nil {
		s.internal(w, err)
		return
	}
	id, err := s.stateIdentity(qs)
	if err != nil {
		s.internal(w, err)
		return
	}
	plan := compat.ScanPlan(id.Model)
	if err := s.scanner.Start(id.Model, plan); err != nil {
		writeProblem(w, http.StatusConflict, problem{Title: "A scan is running", Code: "scan_running", Detail: err.Error()})
		return
	}
	s.st.RecordAudit(s.clock(), auditKindScan, fmt.Sprintf("model %q, %d blocks, by %s", id.Model, len(plan), tk.Name))
	writeJSON(w, http.StatusAccepted, s.scanner.Status())
}

// handleScanStatus is GET /device-scan (installer): the scan's progress.
func (s *Server) handleScanStatus(w http.ResponseWriter, _ *http.Request, _ Token) {
	if s.scanner == nil {
		writeProblem(w, http.StatusServiceUnavailable, problem{Title: "Scan unavailable", Code: "scan_unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, s.scanner.Status())
}
