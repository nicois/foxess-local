package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// putWorkMode sends PUT /work-mode feed-in as a standard token.
func (h *harness) putWorkMode(t *testing.T) *problem {
	t.Helper()
	rr := h.do(t, httpPut, "/api/v1/work-mode", RoleStandard, map[string]string{jsonFieldWorkMode: "feed-in"})
	if rr.Code == http.StatusOK {
		return nil
	}
	var p problem
	if err := json.Unmarshal(rr.Body.Bytes(), &p); err != nil {
		t.Fatalf("work-mode %d: %s", rr.Code, rr.Body.String())
	}
	return &p
}

// TestWriteGateOutcomes (C-008): each refusal is its own status and code
// — not identified (503), untested (403, an installer can grant this
// exact device), impossible (501 with reason unmapped) — and the
// verified device writes.
func TestWriteGateOutcomes(t *testing.T) {
	h := newGatedHarness(t)
	if p := h.putWorkMode(t); p == nil || p.Status != http.StatusServiceUnavailable || p.Code != "device_not_identified" {
		t.Fatalf("unidentified: %+v", p)
	}
	h.seedReferenceSettings()
	if p := h.putWorkMode(t); p != nil {
		t.Fatalf("verified KH10: %+v", p)
	}
	if n := len(h.inv.injected()); n == 0 {
		t.Fatal("verified write sent nothing")
	}

	// firmware bump: untested
	h.setMaster(0x174)
	if p := h.putWorkMode(t); p == nil || p.Status != http.StatusForbidden || p.Code != "write_unverified" {
		t.Fatalf("fw 1.74: %+v", p)
	}

	// a model nobody maps: impossible, unmapped
	h2 := newGatedHarness(t)
	model := make([]uint16, 15)
	const h1 = "ZZ-UNKNOWN-9   "
	for i := range len(h1) {
		model[i] = uint16(h1[i])
	}
	h2.st.RecordRegisters(testBase, 30000, model)
	h2.st.Flush()
	p := h2.putWorkMode(t)
	if p == nil || p.Status != http.StatusNotImplemented || p.Code != "write_unsupported" || p.Reason != "unmapped" {
		t.Fatalf("unknown model: %+v", p)
	}
}

// ackRisk is the PUT /write-override body.
func ackRisk(v bool) map[string]bool { return map[string]bool{"acknowledge_risk": v} }

// refModel is the reference inverter's model string.
const refModel = "KH10"

// overrideState is the GET /write-override body (test view).
type overrideState struct {
	Granted bool `json:"granted"`
	Device  struct {
		Model    string `json:"model"`
		Firmware struct {
			Master string `json:"master"`
		} `json:"firmware"`
	} `json:"device"`
	Grant *struct {
		Model string `json:"model"`
		By    string `json:"by"`
	} `json:"grant"`
}

func (h *harness) overrideState(t *testing.T, method string, role Role, body any, want int) overrideState {
	t.Helper()
	rr := h.do(t, method, "/api/v1/write-override", role, body)
	var got overrideState
	if rr.Code != want || (want == http.StatusOK && json.Unmarshal(rr.Body.Bytes(), &got) != nil) {
		t.Fatalf("%s /write-override as %s: %d %s, want %d", method, role, rr.Code, rr.Body.String(), want)
	}
	return got
}

func (h *harness) setMaster(master uint16) {
	h.st.RecordRegisters(testBase, 30016, []uint16{master, 0x103, 0x171})
	h.st.Flush()
}

// TestWriteOverride: GET shows the device and whether writes are granted;
// only an installer can grant, only with the explicit risk
// acknowledgement, and the grant binds to the exact identity; DELETE
// revokes it. Each change is audited.
func TestWriteOverride(t *testing.T) {
	h := newGatedHarness(t)
	h.seedReferenceSettings()
	h.setMaster(0x174)

	if got := h.overrideState(t, httpGet, RoleReadOnly, nil, http.StatusOK); got.Granted || got.Device.Firmware.Master != "1.74" {
		t.Fatalf("GET before: %+v", got)
	}
	h.overrideState(t, httpPut, RoleExpert, ackRisk(true), http.StatusForbidden)
	h.overrideState(t, httpPut, RoleInstaller, ackRisk(false), http.StatusBadRequest)
	if got := h.overrideState(t, httpPut, RoleInstaller, ackRisk(true), http.StatusOK); !got.Granted || got.Grant == nil || got.Grant.Model != refModel {
		t.Fatalf("grant: %+v", got)
	}
	if p := h.putWorkMode(t); p != nil {
		t.Fatalf("write with the grant: %+v", p)
	}
	h.st.Flush()
	if ev, err := h.st.QueryAudit(testBase.Add(-24*time.Hour), "write-override"); err != nil || len(ev) != 1 {
		t.Fatalf("audit after grant: %v %v", ev, err)
	}

	// a different firmware is not covered by the grant
	h.setMaster(0x175)
	if p := h.putWorkMode(t); p == nil || p.Code != "write_unverified" {
		t.Fatalf("fw 1.75 under a 1.74 grant: %+v", p)
	}

	h.overrideState(t, httpDelete, RoleInstaller, nil, http.StatusNoContent)
	if got := h.overrideState(t, httpGet, RoleReadOnly, nil, http.StatusOK); got.Granted || got.Grant != nil {
		t.Fatalf("after revoke: %+v", got)
	}
}
