package api

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"foxesslocal/internal/policy"
)

// metricsContentType is the Prometheus text exposition format.
const metricsContentType = "text/plain; version=0.0.4; charset=utf-8"

// checkStatusOrder encodes a check status as foxess_check_status's
// value: its index (0 ok, 1 pending, 2 warning, 3 failing).
var checkStatusOrder = []string{checkOK, checkPending, checkWarn, checkFail}

// labelEPS is the EPS (backup) port's flow label.
const labelEPS = "eps"

// metricsWriter writes the exposition format: one HELP/TYPE header per
// family, then its samples.
type metricsWriter struct{ b strings.Builder }

func (m *metricsWriter) family(name, help string) {
	fmt.Fprintf(&m.b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
}

// sample writes one sample; labels are name/value pairs (values escaped).
func (m *metricsWriter) sample(name string, v float64, labels ...string) {
	m.b.WriteString(name)
	if len(labels) > 0 {
		m.b.WriteByte('{')
		for i := 0; i+1 < len(labels); i += 2 {
			if i > 0 {
				m.b.WriteByte(',')
			}
			fmt.Fprintf(&m.b, "%s=%s", labels[i], strconv.Quote(labels[i+1]))
		}
		m.b.WriteByte('}')
	}
	m.b.WriteByte(' ')
	m.b.WriteString(strconv.FormatFloat(v, 'f', -1, 64))
	m.b.WriteByte('\n')
}

func (m *metricsWriter) gauge(name, help string, v float64) {
	m.family(name, help)
	m.sample(name, v)
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// handleMetrics is GET /metrics (F3): the Prometheus text format, read
// like the dashboard (a token, or the public session while public
// read-only access is on). Values absent from the state (no reading
// yet) are omitted, never reported as 0.
func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request, _ Token) {
	checks, err := s.readiness()
	if err != nil {
		s.internal(w, err)
		return
	}
	qs, err := s.st.QueryState()
	if err != nil {
		s.internal(w, err)
		return
	}
	st, err := s.buildState(qs)
	if err != nil {
		s.internal(w, err)
		return
	}
	mode, _, err := s.fwd.Get()
	if err != nil {
		s.internal(w, err)
		return
	}
	var m metricsWriter
	s.writeMetrics(&m, checks, st, mode)
	w.Header().Set("Content-Type", metricsContentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(m.b.String()))
}

// writeMetrics writes every family.
func (s *Server) writeMetrics(m *metricsWriter, checks []Check, st State, mode policy.ForwardMode) {
	m.family("foxess_build_info", "The running build (value 1).")
	m.sample("foxess_build_info", 1, "version", s.info.Version)
	ready := true
	m.family("foxess_check_status", "Readiness check status: 0 ok, 1 pending, 2 warning, 3 failing (GET /readyz has the detail).")
	for _, c := range checks {
		m.sample("foxess_check_status", float64(slices.Index(checkStatusOrder, c.Status)), "check", c.ID)
		ready = ready && c.Status != checkFail
	}
	m.gauge("foxess_ready", "1 while no readiness check fails.", boolValue(ready))
	m.gauge("foxess_inverter_online", "1 while a session is up with fresh readings.", boolValue(st.Online))
	if st.AsOf != nil {
		m.gauge("foxess_data_age_seconds", "Seconds since the newest reading.", s.clock().Sub(*st.AsOf).Round(time.Second).Seconds())
		p := st.Power
		m.family("foxess_power_watts", "Power by flow, W: pv generation; grid + importing; load; battery + charging; ac_port + output; eps backup-port output.")
		for _, f := range []struct {
			flow string
			w    int
		}{{"pv", p.PVW}, {"grid", p.GridW}, {"load", p.LoadW}, {"battery", p.BatteryW}, {"ac_port", p.AcPortW}, {labelEPS, p.EpsW}} {
			m.sample("foxess_power_watts", float64(f.w), "flow", f.flow)
		}
		m.gauge("foxess_battery_soc_percent", "Battery state of charge, %.", float64(st.Battery.SocPct))
		if e := st.Battery.SocEstimatePct; e != nil {
			m.gauge("foxess_battery_soc_estimate_percent", "Battery state of charge between the whole-percent readings (from the battery power), %.", *e)
		}
		writeMeasured(m, st.Battery.MeasuredCapacity)
	}
	if e := st.Battery.StoredEnergyWh; e != nil {
		m.gauge("foxess_battery_stored_energy_wh", "Usable stored energy (SoC × capacity), Wh.", float64(*e))
	}
	if l := st.Battery.BMSChargeLimitA; l != nil {
		m.gauge("foxess_bms_charge_limit_amperes", "The BMS's charge-current limit, A (follows the coldest cell's temperature and the SoC).", *l)
	}
	s.writeTemps(m, st.Temperatures)
	m.family("foxess_forward_mode", "What the FoxESS cloud may receive: 1 for the configured mode.")
	for _, fm := range []policy.ForwardMode{policy.ModeOff, policy.ModeTelemetryOnly, policy.ModeFull} {
		m.sample("foxess_forward_mode", boolValue(fm == mode), "mode", string(fm))
	}
	m.gauge("foxess_maintenance_window_active", "1 while a maintenance window lets the cloud through.", boolValue(s.maint.Active()))
}

// writeTemps writes the temperatures that are reported.
func (s *Server) writeTemps(m *metricsWriter, t Temperatures) {
	sensors := []struct {
		name string
		v    *float64
	}{{"battery", t.BatteryC}, {"inverter", t.InverterC}, {"ambient", t.AmbientC}, {"cell_min", t.CellMinC}, {"cell_max", t.CellMaxC}}
	first := true
	for _, x := range sensors {
		if x.v == nil {
			continue
		}
		if first {
			m.family("foxess_temperature_celsius", "Temperatures, °C: battery, inverter and ambient as the inverter reads them; cell_min / cell_max from the BMS (the coldest cell drives the charge limit).")
			first = false
		}
		m.sample("foxess_temperature_celsius", *x.v, "sensor", x.name)
	}
}

// writeMeasured exports the measured capacity, per direction once known.
func writeMeasured(m *metricsWriter, c *MeasuredCapacity) {
	if c == nil || (c.ChargingWh == nil && c.DischargingWh == nil) {
		return
	}
	m.family("foxess_battery_measured_capacity_wh", "The pack's capacity measured between SoC ticks, Wh, by direction (D-028).")
	for _, d := range []struct {
		dir string
		wh  *int
	}{{batteryStateCharging, c.ChargingWh}, {batteryStateDischarging, c.DischargingWh}} {
		if d.wh != nil {
			m.sample("foxess_battery_measured_capacity_wh", float64(*d.wh), "direction", d.dir)
		}
	}
}
