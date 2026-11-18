package api

import (
	"net/http"

	"foxesslocal/internal/device"
)

// handleCompatibility is GET /compatibility: the per-feature confidence
// report (design D-020) the C-008 write gate decides on.
func (s *Server) handleCompatibility(w http.ResponseWriter, _ *http.Request, _ Token) {
	snap, err := device.Load(s.st)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, snap.Report)
}
