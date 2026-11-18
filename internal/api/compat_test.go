package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

type compatFeature struct {
	Name   string `json:"name"`
	Level  string `json:"level"`
	Reason string `json:"reason"`
	Checks []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		Detail string `json:"detail"`
	} `json:"checks"`
}

type compatBody struct {
	VerifiedProfile string          `json:"verified_profile"`
	Family          string          `json:"family"`
	Features        []compatFeature `json:"features"`
}

// seedReferenceSettings records the reference KH10's identity and
// settings reads (no live values yet).
func (h *harness) seedReferenceSettings() {
	model := make([]uint16, 15)
	const name = "KH10           "
	for i := range len(name) {
		model[i] = uint16(name[i])
	}
	h.st.RecordRegisters(testBase, 30000, model)
	h.st.RecordRegisters(testBase, 30016, []uint16{0x173, 0x103, 0x171})
	h.st.RecordRegisters(testBase, 41000, []uint16{1})
	block := make([]uint16, 40)
	for addr, v := range map[int]uint16{
		41007: 450, 41008: 450, 41010: 100, 41011: 11, 41012: 100, 41016: 100,
		41022: 12500, 41024: 5000, 41027: 560, 41028: 400, 41029: 0xFFCD, 41032: 100,
	} {
		block[addr-41007] = v
	}
	h.st.RecordRegisters(testBase, 41007, block)
	h.st.Flush()
}

// TestCompatibilityEndpoint: GET /compatibility grades each feature
// (D-020): with the reference identity and settings read, the settings
// features are exact; live values are pending until the poll's raw
// blocks exist; the summary map rides on /state.identity.
func TestCompatibilityEndpoint(t *testing.T) {
	h := newHarness(t)
	h.seedTele(testBase, 1000, -50, 900, 950, 0, 90, 2)
	h.seedReferenceSettings()

	rr := h.do(t, httpGet, "/api/v1/compatibility", RoleReadOnly, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /compatibility: %d %s", rr.Code, rr.Body.String())
	}
	var rep compatBody
	if err := json.Unmarshal(rr.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.VerifiedProfile != "kh10-fw1.73" {
		t.Fatalf("profile %q", rep.VerifiedProfile)
	}
	levels := map[string]string{}
	for _, f := range rep.Features {
		levels[f.Name] = f.Level
	}
	const exact, pending = "exact", "pending"
	for f, want := range map[string]string{
		jsonFieldWorkMode: exact, "power_limits": exact, "current_limits": exact,
		"meter_compensation": exact, "settings_read": exact,
		"live_values": pending, "schedule": pending,
	} {
		if levels[f] != want {
			t.Errorf("%s: %q, want %q (all: %v)", f, levels[f], want, levels)
		}
	}

	st := h.getState(t)
	if st.Identity.Writes[jsonFieldWorkMode] != supportVerified || st.Identity.Writes["schedule"] != supportVerified {
		t.Fatalf("/state writes: %v", st.Identity.Writes)
	}
	if st.Identity.Compatibility[jsonFieldWorkMode] != exact || st.Identity.Compatibility["live_values"] != pending {
		t.Fatalf("/state summary: %v", st.Identity.Compatibility)
	}
}
