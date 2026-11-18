package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// durationField is the POST /maintenance body key (the spec's
// MaintenanceRequest.duration).
const durationField = "duration"

// maintenanceBody mirrors the spec's MaintenanceState schema.
type maintenanceBody struct {
	Active         bool       `json:"active"`
	OpenedAt       *time.Time `json:"opened_at"`
	ExpiresAt      *time.Time `json:"expires_at"`
	RemainingS     int64      `json:"remaining_s"`
	Rearms         int        `json:"rearms"`
	OverriddenFrom *string    `json:"overridden_from"`
}

// TestMaintenanceInactive: the window starts closed — active false, the
// identity fields null (the spec's null pattern).
func TestMaintenanceInactive(t *testing.T) {
	h := newHarness(t)
	rr := h.do(t, httpGet, "/api/v1/maintenance", RoleReadOnly, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /maintenance: got %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	var body maintenanceBody
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body.Active || body.OpenedAt != nil || body.ExpiresAt != nil ||
		body.OverriddenFrom != nil || body.RemainingS != 0 || body.Rearms != 0 {
		t.Fatalf("inactive body: got %+v, want all-null", body)
	}
}

// TestMaintenanceOpenRearm: the installer opens the window (the view
// names the overridden mode and the deadline), a second POST re-arms
// (rearms 1, the deadline moves), and the handlers publish nothing to
// the bus (the machine's sink owns the transition events).
func TestMaintenanceOpenRearm(t *testing.T) {
	h := newHarness(t)
	sub := h.bus.Subscribe()
	defer sub.Close()

	rr := h.do(t, httpPost, "/api/v1/maintenance", RoleInstaller, map[string]string{durationField: "45m"})
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /maintenance: got %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	var body maintenanceBody
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if !body.Active || body.OpenedAt == nil || body.ExpiresAt == nil || body.OverriddenFrom == nil {
		t.Fatalf("open body: got %+v, want active with the identity fields", body)
	}
	if *body.OverriddenFrom != forwardModeTelemetryOnly {
		t.Fatalf("overridden_from: got %q, want telemetry_only", *body.OverriddenFrom)
	}
	if body.RemainingS != 45*60 {
		t.Fatalf("remaining_s: got %d, want 2700", body.RemainingS)
	}

	// Re-arm: the deadline moves, the rearm is counted.
	rr = h.do(t, httpPost, "/api/v1/maintenance", RoleInstaller, map[string]string{durationField: "1h"})
	if rr.Code != http.StatusOK {
		t.Fatalf("re-arm: got %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body.Rearms != 1 || body.RemainingS != 60*60 {
		t.Fatalf("re-armed body: got rearms=%d remaining=%d, want 1 / 3600", body.Rearms, body.RemainingS)
	}

	// The handlers publish nothing: the machine's sink owns the
	// transition events (the bus stays empty).
	select {
	case ev := <-sub.Events():
		t.Fatalf("bus: unexpected event %+v — the handlers must not publish", ev)
	case <-time.After(10 * time.Millisecond):
	}
}

// TestMaintenanceDurationErrors: a bad duration or one over the per-call
// cap is 400 invalid_value on the duration field (no state change).
func TestMaintenanceDurationErrors(t *testing.T) {
	h := newHarness(t)
	for _, bad := range []string{"nonsense", "5h"} {
		rr := h.do(t, httpPost, "/api/v1/maintenance", RoleInstaller, map[string]string{durationField: bad})
		p := checkProblem(t, rr, http.StatusBadRequest, "invalid_value")
		if len(p.Errors) == 0 || p.Errors[0].Field != durationField {
			t.Fatalf("duration %q: got errors %+v, want field duration", bad, p.Errors)
		}
	}
	rr := h.do(t, httpGet, "/api/v1/maintenance", RoleReadOnly, nil)
	var body maintenanceBody
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body.Active {
		t.Fatal("a refused open must not change the state")
	}
}

// TestMaintenanceClose: DELETE closes the active window (the view goes
// inactive again); a second DELETE is 409 maintenance_not_active.
func TestMaintenanceClose(t *testing.T) {
	h := newHarness(t)
	rr := h.do(t, httpDelete, "/api/v1/maintenance", RoleInstaller, nil)
	checkProblem(t, rr, http.StatusConflict, "maintenance_not_active")

	rr = h.do(t, httpPost, "/api/v1/maintenance", RoleInstaller, nil) // default duration
	if rr.Code != http.StatusOK {
		t.Fatalf("open: got %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	rr = h.do(t, httpDelete, "/api/v1/maintenance", RoleInstaller, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("close: got %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	var body maintenanceBody
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body.Active || body.OpenedAt != nil || body.ExpiresAt != nil || body.OverriddenFrom != nil {
		t.Fatalf("closed body: got %+v, want inactive all-null", body)
	}
}
