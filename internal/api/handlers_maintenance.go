package api

import (
	"errors"
	"io"
	"net/http"
	"time"

	"foxesslocal/internal/forward"
)

// maintenanceView is the spec's MaintenanceState (the /maintenance GET
// body; the open/close handlers respond with it too). The window is
// process-local — a restart drops it (the spec documents this). The
// nulls mirror the State.as_of pattern: they are set only while the
// window is active.
type maintenanceView struct {
	Active         bool       `json:"active"`
	OpenedAt       *time.Time `json:"opened_at"`
	ExpiresAt      *time.Time `json:"expires_at"`
	RemainingS     int64      `json:"remaining_s"`
	Rearms         int        `json:"rearms"`
	OverriddenFrom *string    `json:"overridden_from"`
	// Capturing: the window's traffic is being recorded (opened with
	// capture).
	Capturing bool `json:"capturing"`
}

// maintenanceViewOf maps one machine View to the spec body.
func maintenanceViewOf(v forward.View) maintenanceView {
	if !v.Active {
		return maintenanceView{Active: false}
	}
	mode := string(v.Base)
	return maintenanceView{
		Active:         true,
		OpenedAt:       &v.Opens,
		ExpiresAt:      &v.Expires,
		RemainingS:     int64(v.Remaining / time.Second),
		Rearms:         v.Rearms,
		OverriddenFrom: &mode,
	}
}

// handleMaintenanceGet is GET /maintenance (spec getMaintenance,
// read-only): the current window state.
func (s *Server) handleMaintenanceGet(w http.ResponseWriter, _ *http.Request, _ Token) {
	s.writeMaintenance(w)
}

// writeMaintenance answers with the window's state.
func (s *Server) writeMaintenance(w http.ResponseWriter) {
	v := maintenanceViewOf(s.maint.View())
	v.Capturing = v.Active && s.capturing()
	writeJSON(w, http.StatusOK, v)
}

// handleMaintenanceOpen is POST /maintenance (spec openMaintenance,
// installer): opens the window (or re-arms it when one is active).
// The body is optional: {"duration":"45m"} (a Go duration string;
// empty/absent → the machine default, over the per-call cap → 400).
// The transition events are the machine's (its sink publishes them) —
// this handler publishes nothing.
func (s *Server) handleMaintenanceOpen(w http.ResponseWriter, r *http.Request, tk Token) {
	var body struct {
		Duration string `json:"duration"`
		// Capture records the window's traffic, byte-exact (a firmware
		// update kept in full).
		Capture bool `json:"capture"`
	}
	// an absent/empty body means the default duration; a malformed one
	// must never open the cloud-write window by default
	if err := readBody(r, &body); err != nil && !errors.Is(err, io.EOF) {
		writeProblem(w, http.StatusBadRequest, badField("body", err.Error()))
		return
	}

	var d time.Duration
	if body.Duration != "" {
		parsed, err := time.ParseDuration(body.Duration)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, badField("duration", "must be a duration like 30m or 45m"))
			return
		}
		d = parsed
	}
	if body.Capture && s.captures == nil {
		writeProblem(w, http.StatusBadRequest, badField("capture", "captures are not available on this server"))
		return
	}

	if _, err := s.maint.Open(d, tk.Name); err != nil {
		if errors.Is(err, forward.ErrDurationTooLong) {
			writeProblem(w, http.StatusBadRequest, badField("duration", err.Error()))
			return
		}
		s.internal(w, err)
		return
	}
	if body.Capture {
		if _, err := s.captures.Start(); err != nil {
			s.internal(w, err)
			return
		}
	}
	s.writeMaintenance(w)
}

// handleMaintenanceClose is DELETE /maintenance (spec closeMaintenance,
// installer): closes the active window immediately (the configured
// mode is back in force); 409 when none is active.
func (s *Server) handleMaintenanceClose(w http.ResponseWriter, _ *http.Request, tk Token) {
	if err := s.maint.Close(tk.Name); err != nil {
		if errors.Is(err, forward.ErrNotActive) {
			writeProblem(w, http.StatusConflict, problem{
				Title:  "Maintenance not active",
				Code:   "maintenance_not_active",
				Detail: err.Error(),
			})
			return
		}
		s.internal(w, err)
		return
	}
	if s.captures != nil {
		s.captures.Stop() // the window is over: so is its capture
	}
	s.writeMaintenance(w)
}
