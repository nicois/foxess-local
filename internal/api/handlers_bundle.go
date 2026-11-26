package api

import (
	"bytes"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"foxesslocal/internal/bundle"
	"foxesslocal/internal/device"
)

// Bundle window bounds (hours).
const (
	bundleDefaultHours = 24
	bundleMaxHours     = 168
	maxRegisterAddr    = 0xFFFF
)

// auditKindBundle is the audit kind of an export.
const auditKindBundle = "bundle-export"

// unsafeName strips anything but [A-Za-z0-9.-] from a filename part.
var unsafeName = regexp.MustCompile(`[^A-Za-z0-9.-]+`)

// handleBundle is GET /bundle (installer): the de-identified diagnostic
// archive (design D-022) for the last `hours` hours.
func (s *Server) handleBundle(w http.ResponseWriter, r *http.Request, tk Token) {
	hours := bundleDefaultHours
	if v := r.URL.Query().Get("hours"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > bundleMaxHours {
			writeProblem(w, http.StatusBadRequest, badField("hours", fmt.Sprintf("must be 1..%d", bundleMaxHours)))
			return
		}
		hours = n
	}
	in, err := s.bundleInput(hours)
	if err != nil {
		s.internal(w, err)
		return
	}
	s.sendBundle(w, in, "foxess-bundle", auditKindBundle, fmt.Sprintf("%dh", hours), tk)
}

// bundleInput gathers the device view, the frames of the last hours and
// the register table.
func (s *Server) bundleInput(hours int) (bundle.Input, error) {
	now := s.clock()
	snap, err := device.Load(s.st)
	if err != nil {
		return bundle.Input{}, err
	}
	frames, err := s.st.FramesBetween(now.Add(-time.Duration(hours)*time.Hour), now.Add(time.Second))
	if err != nil {
		return bundle.Input{}, err
	}
	regs, err := s.st.RegistersRange(0, maxRegisterAddr)
	if err != nil {
		return bundle.Input{}, err
	}
	return bundle.Input{Snapshot: snap, Frames: frames, Registers: regs, Version: s.info.Version, Now: now, RawHours: s.info.RawHours}, nil
}

// sendBundle builds the archive, audits the export and streams it, named
// prefix-<model>-fw<master>.tar.gz.
func (s *Server) sendBundle(w http.ResponseWriter, in bundle.Input, prefix, kind, what string, tk Token) {
	var buf bytes.Buffer
	m, err := bundle.Build(&buf, in)
	if err != nil {
		s.internal(w, err)
		return
	}
	name := prefix
	if in.Snapshot.Model != "" {
		name += "-" + unsafeName.ReplaceAllString(in.Snapshot.Model, "_")
	}
	if fw := in.Snapshot.Firmware.Master; fw != "" {
		name += "-fw" + unsafeName.ReplaceAllString(fw, "_")
	}
	s.st.RecordAudit(in.Now, kind, fmt.Sprintf("%s, %d frames, %d identifiers redacted, by %s",
		what, m.Frames, m.Redacted.Identifiers, tk.Name))
	w.Header().Set("Content-Type", contentTypeGzip)
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.tar.gz"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}
