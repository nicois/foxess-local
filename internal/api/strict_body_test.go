package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestStrictBodies (audit 2026-11-18): a request body with a field the
// endpoint does not know, or with data after the JSON value, is a 400 —
// a misspelt field must not be dropped silently while the rest applies
// (an automation would never notice). Nothing is written.
func TestStrictBodies(t *testing.T) {
	h := newHarness(t)
	for _, body := range []string{
		`{"import_w": 5000, "exprot_w": 0}`,
		`{"import_w": 5000} {"export_w": 0}`,
	} {
		req := httptest.NewRequest(http.MethodPut, "/api/v1/limits", bytes.NewBufferString(body))
		req.Header.Set("Authorization", "Bearer "+h.tokens[RoleInstaller])
		rec := httptest.NewRecorder()
		h.h.ServeHTTP(rec, req)
		checkProblem(t, rec, http.StatusBadRequest, "invalid_value")
	}
	h.inv.mu.Lock()
	n := len(h.inv.frames)
	h.inv.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d frames reached the inverter", n)
	}
}

// TestMaintenanceBody: an absent body opens the window for the default
// duration, but a malformed one is a 400 — it must never open the
// cloud-write window by default.
func TestMaintenanceBody(t *testing.T) {
	h := newHarness(t)
	for _, body := range []string{`{"duraton": "10m"}`, `{"duration": 600}`, `not json`} {
		req := httptest.NewRequest(http.MethodPost, pathMaintenance, bytes.NewBufferString(body))
		req.Header.Set("Authorization", "Bearer "+h.tokens[RoleInstaller])
		rec := httptest.NewRecorder()
		h.h.ServeHTTP(rec, req)
		checkProblem(t, rec, http.StatusBadRequest, "invalid_value")
		if h.mnt.Active() {
			t.Fatalf("%s opened the window", body)
		}
	}
	req := httptest.NewRequest(http.MethodPost, pathMaintenance, nil)
	req.Header.Set("Authorization", "Bearer "+h.tokens[RoleInstaller])
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)
	if rec.Code/100 != 2 || !h.mnt.Active() {
		t.Fatalf("no body: %d, active %v", rec.Code, h.mnt.Active())
	}
}
