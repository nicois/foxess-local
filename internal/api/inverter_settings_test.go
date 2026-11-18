package api

import (
	"net/http"
	"testing"
)

const (
	pathCurrentLimits = "/api/v1/current-limits"
	pathPowerLimit    = "/api/v1/power-limit"
	pathEPSMode       = "/api/v1/eps-mode"
	fieldLoadLimit    = "load_limit_w"
	fieldImportCur    = "import_current_a"
	fieldExportCur    = "export_current_a"
	fieldPowerLimit   = "power_limit_pct"
	fieldEPSMode      = "eps_mode"
)

// TestPutInverterSettings: each installer setting endpoint writes its
// registers with fn06 in wire units (amps → 0.1 A) and reads back what
// was set.
func TestPutInverterSettings(t *testing.T) {
	for _, tc := range []struct {
		path string
		body map[string]any
		want [][2]int // addr, wire value
	}{
		{pathCurrentLimits, map[string]any{fieldImportCur: 55.0, fieldExportCur: 30.5}, [][2]int{{41027, 550}, {41028, 305}}},
		{"/api/v1/battery-currents", map[string]any{"max_charge_current_a": 45}, [][2]int{{41007, 450}}},
		{pathPowerLimit, map[string]any{fieldPowerLimit: 80}, [][2]int{{41016, 80}}},
		{"/api/v1/port-limits", map[string]any{fieldLoadLimit: 10000, "generation_limit_w": 7000}, [][2]int{{41200, 10000}, {41201, 7000}}},
		{pathEPSMode, map[string]any{fieldEPSMode: "ups"}, [][2]int{{41015, 3}}},
		{pathEPSMode, map[string]any{fieldEPSMode: "eps"}, [][2]int{{41015, 2}}},
	} {
		h := newHarness(t)
		rr := h.do(t, httpPut, tc.path, RoleInstaller, tc.body)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s %v: %d %s", tc.path, tc.body, rr.Code, rr.Body.String())
		}
		frs := h.inv.injected()
		if len(frs) != len(tc.want) {
			t.Fatalf("%s: %d frames, want %d", tc.path, len(frs), len(tc.want))
		}
		for i, w := range tc.want {
			mb := checkMb(t, frs[i])
			if mb.Fn != 0x06 || mb.Addr != w[0] || mb.Value != w[1] {
				t.Errorf("%s frame %d: fn=%#x @%d = %d, want fn06 @%d = %d", tc.path, i, mb.Fn, mb.Addr, mb.Value, w[0], w[1])
			}
		}
	}
}

// TestPutInverterSettingsRefused: out-of-range values, unknown fields,
// an empty body and EPS "disabled" (never written on this device) are
// refused with the field named, and nothing is sent; a standard token is
// refused (installer only).
func TestPutInverterSettingsRefused(t *testing.T) {
	for _, tc := range []struct {
		path  string
		body  map[string]any
		field string
	}{
		{pathCurrentLimits, map[string]any{fieldImportCur: 1.5}, fieldImportCur},
		{pathCurrentLimits, map[string]any{fieldExportCur: 101}, fieldExportCur},
		{"/api/v1/battery-currents", map[string]any{"max_discharge_current_a": 51}, "max_discharge_current_a"},
		{pathPowerLimit, map[string]any{fieldPowerLimit: 101}, fieldPowerLimit},
		{"/api/v1/port-limits", map[string]any{fieldLoadLimit: -1}, fieldLoadLimit},
		{pathEPSMode, map[string]any{fieldEPSMode: "disabled"}, fieldEPSMode},
		{pathPowerLimit, map[string]any{}, "body"},
		{pathPowerLimit, map[string]any{fieldPowerLimit: 50, "spin": 1}, "spin"},
	} {
		h := newHarness(t)
		p := checkProblem(t, h.do(t, httpPut, tc.path, RoleInstaller, tc.body), http.StatusBadRequest, "invalid_value")
		if len(p.Errors) == 0 || p.Errors[0].Field != tc.field {
			t.Errorf("%s %v: errors %+v, want field %s", tc.path, tc.body, p.Errors, tc.field)
		}
		if n := len(h.inv.injected()); n != 0 {
			t.Errorf("%s %v: %d frames sent", tc.path, tc.body, n)
		}
	}
	h := newHarness(t)
	checkProblem(t, h.do(t, httpPut, pathPowerLimit, RoleExpert, map[string]any{fieldPowerLimit: 80}), http.StatusForbidden, "insufficient_role")
}
