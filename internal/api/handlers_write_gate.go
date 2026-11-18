package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"foxesslocal/internal/device"
	"foxesslocal/internal/identity"
)

// writeGateResponse maps a C-008 refusal to its status and code (false
// when err is not one). Each outcome is distinct: impossible (501,
// reason unmapped / not_supported), untested (403), not identified
// (503), checks failing (409).
func writeGateResponse(err error) (commandResponse, bool) {
	p := problem{Detail: err.Error()}
	var status int
	switch {
	case errors.Is(err, device.ErrWriteNotSupported):
		status, p.Title, p.Code, p.Reason = http.StatusNotImplemented, "Not supported by this inverter", "write_unsupported", "not_supported"
	case errors.Is(err, device.ErrWriteUnmapped):
		status, p.Title, p.Code, p.Reason = http.StatusNotImplemented, "Not mapped for this inverter", "write_unsupported", "unmapped"
	case errors.Is(err, device.ErrWriteUnverified):
		status, p.Title, p.Code = http.StatusForbidden, "Untested on this inverter", "write_unverified"
	case errors.Is(err, device.ErrNotIdentified):
		status, p.Title, p.Code = http.StatusServiceUnavailable, "Inverter not identified yet", "device_not_identified"
	case errors.Is(err, device.ErrChecksFailing):
		status, p.Title, p.Code = http.StatusConflict, "Register map looks wrong", "checks_failing"
	default:
		return commandResponse{}, false
	}
	return commandResponse{status: status, body: p}, true
}

// writeOverrideBody is the spec's WriteOverride schema.
type writeOverrideBody struct {
	// Granted: untested writes are enabled for the current device.
	Granted bool             `json:"granted"`
	Device  overrideDevice   `json:"device"`
	Grant   *device.Override `json:"grant"`
}

type overrideDevice struct {
	Model    string            `json:"model"`
	Firmware identity.Firmware `json:"firmware"`
}

// auditKindWriteOverride is the audit kind of grant / revoke rows.
const auditKindWriteOverride = "write-override"

func (s *Server) writeOverrideState() (writeOverrideBody, error) {
	snap, err := device.Load(s.st)
	if err != nil {
		return writeOverrideBody{}, err
	}
	ov, err := device.LoadOverride(s.st)
	if err != nil {
		return writeOverrideBody{}, err
	}
	return writeOverrideBody{
		Granted: ov.Covers(snap.Model, snap.Firmware),
		Device:  overrideDevice{Model: snap.Model, Firmware: snap.Firmware},
		Grant:   ov,
	}, nil
}

// handleWriteOverrideGet is GET /write-override.
func (s *Server) handleWriteOverrideGet(w http.ResponseWriter, _ *http.Request, _ Token) {
	body, err := s.writeOverrideState()
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// handleWriteOverridePut is PUT /write-override (installer): grant
// untested writes for the current device's exact model and firmware.
func (s *Server) handleWriteOverridePut(w http.ResponseWriter, r *http.Request, tk Token) {
	var req struct {
		AcknowledgeRisk bool `json:"acknowledge_risk"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, badField("body", err.Error()))
		return
	}
	if !req.AcknowledgeRisk {
		writeProblem(w, http.StatusBadRequest, badField("acknowledge_risk",
			"must be true: untested writes can misconfigure the inverter"))
		return
	}
	snap, err := device.Load(s.st)
	if err != nil {
		s.internal(w, err)
		return
	}
	if snap.Model == "" || !snap.Firmware.Complete() {
		writeProblem(w, http.StatusConflict, problem{
			Title: "Inverter not identified yet", Code: "device_not_identified",
			Detail: "the grant binds to the exact model and firmware, which are not known yet",
		})
		return
	}
	ov := device.Override{Model: snap.Model, Firmware: snap.Firmware, By: tk.Name, At: s.clock()}
	if err := device.SaveOverride(s.st, ov); err != nil {
		s.internal(w, err)
		return
	}
	s.st.RecordAudit(s.clock(), auditKindWriteOverride, fmt.Sprintf("granted for %s fw %s/%s/%s by %s",
		ov.Model, ov.Firmware.Master, ov.Firmware.Slave, ov.Firmware.Manager, tk.Name))
	s.handleWriteOverrideGet(w, r, tk)
}

// handleWriteOverrideDelete is DELETE /write-override (installer).
func (s *Server) handleWriteOverrideDelete(w http.ResponseWriter, _ *http.Request, tk Token) {
	if err := device.ClearOverride(s.st); err != nil {
		s.internal(w, err)
		return
	}
	s.st.RecordAudit(s.clock(), auditKindWriteOverride, "revoked by "+tk.Name)
	w.WriteHeader(http.StatusNoContent)
}
