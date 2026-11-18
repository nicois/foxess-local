package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"foxesslocal/internal/update"
)

// UpdateService is the self-upgrade surface (design D-023;
// update.Service in production).
type UpdateService interface {
	View() update.View
	CheckNow(ctx context.Context) update.View
	Apply(ctx context.Context) error
	ClearFailed() error
}

// WithUpdate wires the self-upgrade service.
func WithUpdate(u UpdateService) ServerOption {
	return func(s *Server) { s.upd = u }
}

// auditKindUpdate is the audit kind of an applied update.
const auditKindUpdate = "update"

func (s *Server) noUpdates() update.View {
	return update.View{Current: s.info.Version, Reason: "self-upgrade is not available in this host"}
}

// handleUpdateGet is GET /update.
func (s *Server) handleUpdateGet(w http.ResponseWriter, _ *http.Request, _ Token) {
	if s.upd == nil {
		writeJSON(w, http.StatusOK, s.noUpdates())
		return
	}
	writeJSON(w, http.StatusOK, s.upd.View())
}

// handleUpdateCheck is POST /update/check (installer).
func (s *Server) handleUpdateCheck(w http.ResponseWriter, r *http.Request, _ Token) {
	if s.upd == nil {
		writeJSON(w, http.StatusOK, s.noUpdates())
		return
	}
	writeJSON(w, http.StatusOK, s.upd.CheckNow(r.Context()))
}

// handleUpdateApply is POST /update/apply (installer): install the
// latest release and restart into it (202; the restart follows the
// response).
func (s *Server) handleUpdateApply(w http.ResponseWriter, r *http.Request, tk Token) {
	if s.upd == nil {
		writeProblem(w, http.StatusConflict, problem{Title: "Update not possible", Code: "update_not_possible", Detail: s.noUpdates().Reason})
		return
	}
	v := s.upd.View()
	err := s.upd.Apply(r.Context())
	var reason *update.ReasonError
	switch {
	case err == nil:
		s.st.RecordAudit(s.clock(), auditKindUpdate, fmt.Sprintf("%s → %s by %s; restarting", v.Current, v.Latest, tk.Name))
		writeJSON(w, http.StatusAccepted, map[string]any{"restarting": true, "from": v.Current, "to": v.Latest})
	case errors.As(err, &reason):
		writeProblem(w, http.StatusConflict, problem{Title: "Update not possible", Code: "update_not_possible", Detail: reason.Reason})
	case errors.Is(err, update.ErrNoSpace):
		writeProblem(w, http.StatusInsufficientStorage, problem{Title: "Not enough disk space", Code: "insufficient_space", Detail: err.Error()})
	case errors.Is(err, update.ErrUpToDate):
		writeProblem(w, http.StatusConflict, problem{Title: "Already up to date", Code: "update_not_available", Detail: err.Error()})
	case errors.Is(err, update.ErrBadSignature), errors.Is(err, update.ErrChecksum), errors.Is(err, update.ErrSelfTest):
		s.st.RecordAudit(s.clock(), auditKindUpdate, "refused: "+err.Error())
		writeProblem(w, http.StatusBadGateway, problem{Title: "Update failed verification", Code: "update_verification_failed", Detail: err.Error()})
	default:
		writeProblem(w, http.StatusBadGateway, problem{Title: "Update failed", Code: "update_failed", Detail: err.Error()})
	}
}

// handleUpdateDismiss is DELETE /update/rolled-back (installer).
func (s *Server) handleUpdateDismiss(w http.ResponseWriter, _ *http.Request, _ Token) {
	if s.upd != nil {
		if err := s.upd.ClearFailed(); err != nil {
			s.internal(w, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}
