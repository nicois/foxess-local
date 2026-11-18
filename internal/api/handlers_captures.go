package api

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"

	"foxesslocal/internal/wincap"
)

// contentTypeGzip is a .tar.gz download's type.
const contentTypeGzip = "application/gzip"

// auditKindCapture is the audit kind of a capture download or delete.
const auditKindCapture = "capture"

// WithCaptures offers maintenance-window captures (owner, 2026-11-18).
func WithCaptures(c *wincap.Capturer) ServerOption {
	return func(s *Server) { s.captures = c }
}

func (s *Server) captureRoutes(mux *http.ServeMux) {
	s.handle(mux, "GET /api/v1/captures", s.protect(RoleInstaller, s.handleCaptureList))
	s.handle(mux, "GET /api/v1/captures/{id}", s.protect(RoleInstaller, s.handleCaptureGet))
	s.handle(mux, "DELETE /api/v1/captures/{id}", s.protect(RoleInstaller, s.handleCaptureDelete))
}

// capturing reports whether a window is recording.
func (s *Server) capturing() bool { return s.captures != nil && s.captures.Active() }

// handleCaptureList is GET /captures (spec listCaptures, installer).
func (s *Server) handleCaptureList(w http.ResponseWriter, _ *http.Request, _ Token) {
	items := []wincap.Info{}
	if s.captures != nil {
		list, err := s.captures.List()
		if err != nil {
			s.internal(w, err)
			return
		}
		items = list
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// handleCaptureGet is GET /captures/{id} (spec getCapture, installer):
// the window's files as a .tar.gz, byte-exact.
func (s *Server) handleCaptureGet(w http.ResponseWriter, r *http.Request, tk Token) {
	id := r.PathValue("id")
	if s.captures == nil {
		writeNotFound(w, "no such capture")
		return
	}
	var buf bytes.Buffer
	if err := s.captures.Archive(&buf, id); err != nil {
		if errors.Is(err, wincap.ErrNotFound) {
			writeNotFound(w, "no such capture")
			return
		}
		s.internal(w, err)
		return
	}
	s.st.RecordAudit(s.clock(), auditKindCapture, fmt.Sprintf("capture %s downloaded by %s", id, tk.Name))
	w.Header().Set("Content-Type", contentTypeGzip)
	w.Header().Set("Content-Disposition", `attachment; filename="foxess-capture-`+id+`.tar.gz"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

// handleCaptureDelete is DELETE /captures/{id} (spec deleteCapture,
// installer); 409 while it is recording.
func (s *Server) handleCaptureDelete(w http.ResponseWriter, r *http.Request, tk Token) {
	id := r.PathValue("id")
	if s.captures == nil {
		writeNotFound(w, "no such capture")
		return
	}
	if err := s.captures.Delete(id); err != nil {
		if errors.Is(err, wincap.ErrNotFound) {
			writeNotFound(w, "no such capture")
			return
		}
		if errors.Is(err, wincap.ErrRecording) {
			writeProblem(w, http.StatusConflict, problem{Title: "Still recording", Code: "conflict", Detail: err.Error()})
			return
		}
		s.internal(w, err)
		return
	}
	s.st.RecordAudit(s.clock(), auditKindCapture, fmt.Sprintf("capture %s deleted by %s", id, tk.Name))
	w.WriteHeader(http.StatusNoContent)
}
