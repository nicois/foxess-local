package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

type readyBody struct {
	Status string `json:"status"`
	Checks []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Detail string `json:"detail"`
		Fix    string `json:"fix"`
	} `json:"checks"`
}

func decodeReady(t *testing.T, _ int, body []byte) readyBody {
	t.Helper()
	var b readyBody
	if err := json.Unmarshal(body, &b); err != nil {
		t.Fatalf("readyz body %s: %v", body, err)
	}
	return b
}

func (b readyBody) check(id string) (status, detail string, ok bool) {
	for _, c := range b.Checks {
		if c.ID == id {
			return c.Status, c.Detail, true
		}
	}
	return "", "", false
}

// TestReadyz (F3, audit R2): /readyz is a real readiness check — 503
// while any check fails (here: no inverter yet), 200 once the inverter's
// readings are fresh. Without a token it names each check and its
// status only; any token adds the detail and the fix.
func TestReadyz(t *testing.T) {
	h := newHarness(t)
	rec := h.do(t, "GET", "/readyz", "", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no inverter: status %d, want 503", rec.Code)
	}
	b := decodeReady(t, rec.Code, rec.Body.Bytes())
	if st, detail, ok := b.check("inverter_connected"); b.Status != readyNot || !ok || st != checkFail || detail != "" {
		t.Fatalf("anonymous body: %+v", b)
	}
	if st, _, ok := b.check("store"); !ok || st != checkOK {
		t.Fatalf("store check: %+v", b)
	}
	b = decodeReady(t, 0, h.do(t, "GET", "/readyz", RoleReadOnly, nil).Body.Bytes())
	if _, detail, _ := b.check("inverter_connected"); detail == "" {
		t.Fatalf("token body lacks the detail: %+v", b)
	}

	h.seedTele(h.clk.get().Add(-2*time.Second), 100, 0, 100, 100, 0, 50, 2)
	rec = h.do(t, "GET", "/readyz", "", nil)
	b = decodeReady(t, rec.Code, rec.Body.Bytes())
	if rec.Code != http.StatusOK || b.Status == readyNot {
		t.Fatalf("online: %d %+v", rec.Code, b)
	}
	if rec := h.do(t, "GET", "/api/v1/readyz", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("/api/v1/readyz: %d", rec.Code)
	}
}

// TestReadyzHostChecks: the host's own checks (the inverter listener,
// disk space) join the list; a failing one makes the service not ready,
// a warning only degraded.
func TestReadyzHostChecks(t *testing.T) {
	h := newHarness(t)
	h.seedTele(h.clk.get().Add(-2*time.Second), 100, 0, 100, 100, 0, 50, 2)
	listener := Check{ID: "listener", Title: "Inverter listener", Status: CheckFail, Detail: "port 14431 is not listening"}
	h.srv.readyChecks = func() []Check {
		return []Check{listener, {ID: "disk", Title: "Disk space", Status: CheckWarn, Detail: "low"}}
	}
	rec := h.do(t, "GET", "/readyz", "", nil)
	if b := decodeReady(t, rec.Code, rec.Body.Bytes()); rec.Code != http.StatusServiceUnavailable || b.Status != readyNot {
		t.Fatalf("listener down: %d %+v", rec.Code, b)
	}
	listener.Status = CheckOK
	rec = h.do(t, "GET", "/readyz", "", nil)
	if b := decodeReady(t, rec.Code, rec.Body.Bytes()); rec.Code != http.StatusOK || b.Status != readyDegraded {
		t.Fatalf("disk warning: %d %+v", rec.Code, b)
	}
}

// TestReadyzNoInverter (D-031): installed without an inverter address
// (installer --no-inverter, an OpenStick awaiting its wizard), the
// inverter check is a warning that says so, not a failure: the service
// is degraded, not broken. The setup checklist says the same.
func TestReadyzNoInverter(t *testing.T) {
	h := newHarness(t)
	h.srv.noInverter = func() bool { return true }
	rec := h.do(t, "GET", "/readyz", RoleReadOnly, nil)
	b := decodeReady(t, rec.Code, rec.Body.Bytes())
	st, detail, _ := b.check(ReadyInverterConnected)
	if rec.Code != http.StatusOK || b.Status != readyDegraded || st != checkWarn || !strings.Contains(detail, "no inverter address") {
		t.Fatalf("no inverter: %d %+v", rec.Code, b)
	}
	if c := h.srv.checkConnected(State{}); c.Status != checkWarn || c.Fix == "" {
		t.Fatalf("setup check: %+v", c)
	}
	// live readings win (the address was set since)
	h.seedTele(h.clk.get().Add(-2*time.Second), 100, 0, 100, 100, 0, 50, 2)
	b = decodeReady(t, 0, h.do(t, "GET", "/readyz", "", nil).Body.Bytes())
	if st, _, _ := b.check(ReadyInverterConnected); st != checkOK {
		t.Fatalf("online: %+v", b)
	}
}
