package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"foxesslocal/internal/frame"
)

// TestMetrics (F3): /metrics is the Prometheus text format — readiness
// per check, online state and data age, the power flows, SoC,
// temperatures and BMS charge limit, the forward mode and the window,
// and the build. It is read like the dashboard: a token, or the public
// session while public read-only access is on.
func TestMetrics(t *testing.T) {
	h := newHarness(t)
	now := h.clk.get()
	h.st.RecordTelemetry(testConnID, now.Add(-3*time.Second), frame.Tele{
		Sol: 2500, Grid: -400, Haus: 900, Acport: 1300, Batt: 1200, Soc: 64,
		Temps: &frame.Temps{BattDeciC: 242, InvDeciC: 290, AmbDeciC: 325, BMSChargeDeciA: 350},
	})
	h.st.Flush()
	rec := h.do(t, "GET", "/api/v1/metrics", RoleReadOnly, nil)
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatalf("status %d, type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	body := rec.Body.String()
	for _, want := range []string{
		"# TYPE foxess_ready gauge\nfoxess_ready 1\n",
		`foxess_check_status{check="inverter_connected"} 0`,
		`foxess_check_status{check="store"} 0`,
		"foxess_inverter_online 1\n",
		"foxess_data_age_seconds 3\n",
		`foxess_power_watts{flow="pv"} 2500`,
		`foxess_power_watts{flow="grid"} -400`,
		`foxess_power_watts{flow="battery"} 1200`,
		"foxess_battery_soc_percent 64\n",
		"foxess_bms_charge_limit_amperes 35\n",
		`foxess_temperature_celsius{sensor="battery"} 24.2`,
		`foxess_temperature_celsius{sensor="ambient"} 32.5`,
		`foxess_forward_mode{mode="telemetry_only"} 1`,
		`foxess_forward_mode{mode="off"} 0`,
		"foxess_maintenance_window_active 0\n",
		`foxess_build_info{version="`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
	if strings.Contains(body, `sensor="cell_min"`) {
		t.Errorf("a cell temperature without a snapshot:\n%s", body)
	}
	if rec := h.do(t, "GET", "/metrics", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("public /metrics: %d", rec.Code)
	}
	h.srv.info.PublicReadOnly = false
	if rec := h.do(t, "GET", "/metrics", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("/metrics without public access: %d, want 401", rec.Code)
	}
}
