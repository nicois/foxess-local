package api

import (
	"fmt"
	"net/http"

	"foxesslocal/internal/policy"
)

// forwardModeView is the spec's ForwardModeView (the /forward-mode
// body for GET and PUT).
type forwardModeView struct {
	Mode string `json:"mode"`
}

// handleGetForwardMode is GET /forward-mode (spec getForwardMode).
func (s *Server) handleGetForwardMode(w http.ResponseWriter, _ *http.Request, _ Token) {
	mode, _, err := s.fwd.Get()
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, forwardModeView{Mode: string(mode)})
}

// handlePutForwardMode is PUT /forward-mode (spec setForwardMode,
// installer): it changes the seam (which writes the audit row) and
// publishes the spec's system event.
func (s *Server) handlePutForwardMode(w http.ResponseWriter, r *http.Request, tk Token) {
	var body struct {
		Mode string `json:"mode"`
	}
	if err := readBody(r, &body); err != nil {
		writeProblem(w, http.StatusBadRequest, badField("body", err.Error()))
		return
	}
	mode, ok := parseForwardMode(body.Mode)
	if !ok {
		writeProblem(w, http.StatusBadRequest, badField("mode", "must be off, telemetry_only, or full"))
		return
	}
	if err := s.fwd.Set(mode, tk.Name); err != nil {
		s.internal(w, err)
		return
	}
	s.bus.PublishSystem(SystemEvent{
		Type:   SystemForwardModeChanged,
		Detail: fmt.Sprintf("forward mode → %s by %s", mode, tk.Name),
	})
	writeJSON(w, http.StatusOK, forwardModeView{Mode: string(mode)})
}

// Forward-mode names (the spec's ForwardMode enum).
const (
	forwardModeOff           = string(policy.ModeOff)
	forwardModeTelemetryOnly = string(policy.ModeTelemetryOnly)
	forwardModeFull          = string(policy.ModeFull)
)

// parseForwardMode maps the spec's forward-mode names to the policy
// modes.
func parseForwardMode(v string) (ForwardMode, bool) {
	switch v {
	case forwardModeOff:
		return policy.ModeOff, true
	case forwardModeTelemetryOnly:
		return policy.ModeTelemetryOnly, true
	case forwardModeFull:
		return policy.ModeFull, true
	}
	return "", false
}

// The ForwardModeState implementation is forward.State (plan 2 task 9):
// the API satisfies the seam structurally and republishes the
// forward-mode-changed bus event for the REST surface.
