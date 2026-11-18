package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"foxesslocal/internal/frame"
)

// seedFullState seeds a store exercising every /state source: the 5 s
// telemetry, the ~293 s snap61 burst, and the config registers (work
// mode, meter compensation, limits, the 48000 master, the 48010 table).
func (h *harness) seedFullState() {
	t0 := testBase
	h.seedTele(t0, 1676, 10601, 1278, -9323, 10999, 60, 4)
	h.seedSnap61(t0.Add(300*time.Second), 58, 984, []uint16{4000, 4001})
	h.seedMbWrite(t0.Add(600*time.Second), fn06Pdu(regWorkModeAddr, 1))
	h.seedMbWrite(t0.Add(700*time.Second), fn06Pdu(regMeterCompAddr, 0xFFCE))
	h.seedMbWrite(t0.Add(800*time.Second), fn10Pdu(regImportLimAddr, []int{0, 5000}))
	h.seedMbWrite(t0.Add(850*time.Second), fn10Pdu(regExportLimAddr, []int{0, 3000}))
	h.seedMbWrite(t0.Add(900*time.Second), fn10Pdu(regModeEnAddr, []int{1, 0, 0, 0, 0, 0, 0, 0, 0, 0}))
	// Group 1: 11:01–13:59 force-charge (max 80 / min 20, fd 90 @ 15000 W).
	// Group 2: 18:00–20:00 force-discharge (max 50 / min 10, fd 30 @ 8000 W).
	var groups [8][7]int
	groups[0] = [7]int{1, 11<<8 | 1, 13<<8 | 59, 6, 80<<8 | 20, 90, 15000}
	groups[1] = [7]int{1, 18 << 8, 20 << 8, 7, 50<<8 | 10, 30, 8000}
	h.seedMbWrite(t0.Add(1000*time.Second), fn10Pdu(regSlotTableAddr, slotRegisters(groups)))
}

// assertFullBase checks the spec's top-level 5 s fields (as_of, online,
// power, work mode, meter compensation, limits, forward mode).
func assertFullBase(t *testing.T, st stateBody) {
	t.Helper()
	if st.AsOf == nil || !st.AsOf.Equal(testBase.Add(1000*time.Second)) {
		t.Errorf("as_of: got %v, want %s", st.AsOf, testBase.Add(1000*time.Second))
	}
	if !st.Online {
		t.Errorf("online: got false, want true")
	}
	if got := st.Power; got.PVW != 1676 || got.GridW != 10601 || got.LoadW != 1278 ||
		got.AcPortW != -9323 || got.BatteryW != 10999 {
		t.Errorf("power: got %+v", got)
	}
	if st.WorkMode != workModeFeedIn {
		t.Errorf("work_mode: got %q, want feed-in", st.WorkMode)
	}
	if st.MeterCompensationW != -50 {
		t.Errorf("meter_compensation_w: got %d, want -50", st.MeterCompensationW)
	}
	if st.Limits.ImportW != 5000 || st.Limits.ExportW != 3000 {
		t.Errorf("limits: got %+v, want {5000 3000}", st.Limits)
	}
	if st.ForwardMode != forwardModeTelemetryOnly {
		t.Errorf("forward_mode: got %q, want telemetry_only (the seam)", st.ForwardMode)
	}
}

// assertFullBattery checks the spec's battery fields (the 5 s tele plus
// the burst-backed energy and pack voltage).
func assertFullBattery(t *testing.T, st stateBody) {
	t.Helper()
	if st.Battery.SocPct != 58 {
		t.Errorf("battery.soc_pct: got %d, want 58 (the snap61 is newer than the tele — newest source wins)", st.Battery.SocPct)
	}
	if st.Battery.State != "charging" {
		t.Errorf("battery.state: got %q, want charging", st.Battery.State)
	}
	// usable energy = SoC × capacity (58 % of the default 41 930 Wh);
	// the snap61 1907 counter's unit is undetermined, so it is not used.
	if st.Battery.StoredEnergyWh == nil || *st.Battery.StoredEnergyWh != 24319 {
		t.Errorf("battery.stored_energy_wh: got %v, want 24319 (58 %% × 41930 Wh)", st.Battery.StoredEnergyWh)
	}
	if st.Battery.CapacityWh == nil || *st.Battery.CapacityWh != 41930 {
		t.Errorf("battery.capacity_wh: got %v, want 41930", st.Battery.CapacityWh)
	}
	if st.Battery.PackVoltageV == nil || *st.Battery.PackVoltageV != 40.0 {
		t.Errorf("battery.pack_voltage_v: got %v, want 40.0 (4000 × 0.01 V)", st.Battery.PackVoltageV)
	}
}

// wantScheduleGroups is the full 8-group table the seeded state must
// return (the two seeded groups and the six zeroed ones).
func wantScheduleGroups() []scheduleGroup {
	groups := []scheduleGroup{
		{Index: 1, Enabled: true, Start: testTime1101, End: testTime1359, WorkMode: workModeForceCharge, MaxSocPct: 80, MinSocOnGridPct: 20, FdSocPct: 90, FdPwrW: 15000},
		{Index: 2, Enabled: true, Start: "18:00", End: "20:00", WorkMode: workModeForceDischarge, MaxSocPct: 50, MinSocOnGridPct: 10, FdSocPct: 30, FdPwrW: 8000},
	}
	for i := 2; i < 8; i++ {
		groups = append(groups, scheduleGroup{Index: i + 1, Start: hhmmZero, End: hhmmZero, WorkMode: workModeSelfUse})
	}
	return groups
}

// assertFullSchedule checks the spec's schedule (the 48000 master plus
// the 48010 groups: the two seeded groups, six zeroed).
func assertFullSchedule(t *testing.T, st stateBody) {
	t.Helper()
	want := wantScheduleGroups()
	if !st.Schedule.Enabled {
		t.Errorf("schedule.enabled: got false, want true (48000 r0)")
	}
	if len(st.Schedule.Groups) != len(want) {
		t.Fatalf("schedule.groups: got %d groups, want 8", len(st.Schedule.Groups))
	}
	for i := range want {
		if st.Schedule.Groups[i] != want[i] {
			t.Errorf("group %d: got %+v, want %+v", i+1, st.Schedule.Groups[i], want[i])
		}
	}
}

// TestStateFull: every field of the spec's State schema, field-for-field.
func TestStateFull(t *testing.T) {
	h := newHarness(t)
	h.srv.info.BatteryCapacityWh = 41930
	h.seedFullState()
	// The clock sits 5 s after the newest frame: online (the 15 s
	// silence threshold of the spec's /events guidance).
	h.clk.advance(5 * time.Second)

	rr := h.do(t, httpGet, "/api/v1/state", RoleReadOnly, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /state: got %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	var st stateBody
	if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
		t.Fatalf("state body: %v", err)
	}
	assertFullBase(t, st)
	assertFullBattery(t, st)
	assertFullSchedule(t, st)
}

// assertEmptyStateJSON checks the raw-JSON shape only a string scan can
// see (online false, as_of omitted — the struct cannot see an omitted
// field).
func assertEmptyStateJSON(t *testing.T, body string) {
	t.Helper()
	if !contains(body, `"online":false`) {
		t.Errorf("empty state: want online false, got %s", body)
	}
	// the top-level as_of (energy.as_of is its own field: null until read)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["as_of"]; ok {
		t.Errorf("empty state: as_of must be absent (no frame reflected), got %s", body)
	}
	if !contains(body, `"energy":{"grid_export_kwh":null`) {
		t.Errorf("empty state: energy counters must be null before a read, got %s", body)
	}
}

// assertEmptyState checks the spec's empty-state shape on a decoded body
// (5 s fields zero, burst fields null, 8 zeroed groups).
func assertEmptyState(t *testing.T, st stateBody) {
	t.Helper()
	if st.AsOf != nil {
		t.Errorf("as_of: got %v, want absent", st.AsOf)
	}
	if st.Battery.SocPct != 0 || st.Battery.State != batteryStateUnknown {
		t.Errorf("battery: got %+v, want {0 unknown nil nil}", st.Battery)
	}
	if st.Battery.StoredEnergyWh != nil || st.Battery.PackVoltageV != nil {
		t.Errorf("burst fields: got %+v, want null until the first burst", st.Battery)
	}
	if st.Power != (powerBody{}) {
		t.Errorf("power: got %+v, want zeros", st.Power)
	}
	if st.WorkMode != workModeSelfUse || st.MeterCompensationW != 0 {
		t.Errorf("config: got %q / %d, want self-use / 0", st.WorkMode, st.MeterCompensationW)
	}
	if st.Limits != (limitsBody{}) {
		t.Errorf("limits: got %+v, want zeros", st.Limits)
	}
	assertEmptySchedule(t, st)
}

// assertEmptySchedule checks the empty-state schedule (disabled with the
// eight zeroed groups).
func assertEmptySchedule(t *testing.T, st stateBody) {
	t.Helper()
	if st.Schedule.Enabled || len(st.Schedule.Groups) != 8 {
		t.Errorf("schedule: got %+v, want disabled with 8 groups", st.Schedule)
	}
	for i, g := range st.Schedule.Groups {
		if g.Enabled || g.Start != hhmmZero || g.End != hhmmZero {
			t.Errorf("group %d: got %+v, want zeroed", i+1, g)
		}
	}
}

// TestStateEmpty: the spec's empty-state shape on an empty store — the
// 5 s fields zero, the burst fields null, as_of omitted, offline.
func TestStateEmpty(t *testing.T) {
	h := newHarness(t)
	rr := h.do(t, httpGet, "/api/v1/state", RoleReadOnly, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /state: got %d, want 200", rr.Code)
	}
	assertEmptyStateJSON(t, rr.Body.String())
	var st stateBody
	if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
		t.Fatalf("state body: %v", err)
	}
	assertEmptyState(t, st)
}

// TestStateSocFromSnapshotOnly: the current uplink form carries no 5 s
// telemetry (profile §4 note) — only the ~5 min snap61 burst. Its SoC
// must reach /state instead of the zero of a missing tele row.
func TestStateSocFromSnapshotOnly(t *testing.T) {
	h := newHarness(t)
	h.seedSnap61(testBase, 100, 989, []uint16{55229})
	rr := h.do(t, httpGet, "/api/v1/state", RoleReadOnly, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /state: got %d, want 200", rr.Code)
	}
	var st stateBody
	if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
		t.Fatalf("state body: %v", err)
	}
	if st.Battery.SocPct != 100 {
		t.Errorf("battery.soc_pct: got %d, want 100 (from snap61)", st.Battery.SocPct)
	}
}

// TestStateOfflineStale: the same seed, but the clock 60 s past the
// newest frame: online flips to false (> 15 s of silence).
func TestStateOfflineStale(t *testing.T) {
	h := newHarness(t)
	h.seedFullState()
	h.clk.advance(60 * time.Second)
	rr := h.do(t, httpGet, "/api/v1/state", RoleReadOnly, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /state: got %d, want 200", rr.Code)
	}
	var st stateBody
	if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
		t.Fatalf("state body: %v", err)
	}
	if st.Online {
		t.Errorf("online: got true 60 s after the last frame, want false")
	}
}

// stateBody mirrors the spec's State schema (the /state response).
type stateBody struct {
	AsOf               *time.Time   `json:"as_of"`
	Online             bool         `json:"online"`
	Power              powerBody    `json:"power"`
	Battery            battery      `json:"battery"`
	WorkMode           string       `json:"work_mode"`
	MeterCompensationW int          `json:"meter_compensation_w"`
	Limits             limitsBody   `json:"limits"`
	Schedule           schedule     `json:"schedule"`
	ForwardMode        string       `json:"forward_mode"`
	Identity           identityBody `json:"identity"`
}

type identityBody struct {
	SN       string `json:"sn"`
	Model    string `json:"model"`
	Firmware struct {
		Master  string `json:"master"`
		Slave   string `json:"slave"`
		Manager string `json:"manager"`
	} `json:"firmware"`
	VerifiedProfile *string           `json:"verified_profile"`
	Compatibility   map[string]string `json:"compatibility"`
	Writes          map[string]string `json:"writes"`
}

type powerBody struct {
	PVW      int32 `json:"pv_w"`
	GridW    int32 `json:"grid_w"`
	LoadW    int32 `json:"load_w"`
	AcPortW  int32 `json:"ac_port_w"`
	BatteryW int32 `json:"battery_w"`
}

type battery struct {
	SocPct         int      `json:"soc_pct"`
	State          string   `json:"state"`
	StoredEnergyWh *int     `json:"stored_energy_wh"`
	CapacityWh     *int     `json:"capacity_wh"`
	PackVoltageV   *float64 `json:"pack_voltage_v"`
}

type limitsBody struct {
	ImportW        int      `json:"import_w"`
	ExportW        int      `json:"export_w"`
	ImportCurrentA *float64 `json:"import_current_a"`
	ExportCurrentA *float64 `json:"export_current_a"`
}

type schedule struct {
	Enabled bool            `json:"enabled"`
	Groups  []scheduleGroup `json:"groups"`
}

type scheduleGroup struct {
	Index           int    `json:"index"`
	Enabled         bool   `json:"enabled"`
	Start           string `json:"start"`
	End             string `json:"end"`
	WorkMode        string `json:"work_mode"`
	MaxSocPct       int    `json:"max_soc_pct"`
	MinSocOnGridPct int    `json:"min_soc_on_grid_pct"`
	FdSocPct        int    `json:"fd_soc_pct"`
	FdPwrW          int    `json:"fd_pwr_w"`
}

// contains is a substring check (JSON shape assertions).
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestStateOnlineFromTracker: with the host's liveness tracker wired
// (ServerInfo.Online), /state reports its answer — the same one the SSE
// connection events carry — not its own telemetry-age guess.
func TestStateOnlineFromTracker(t *testing.T) {
	h := newHarness(t)
	h.seedTele(testBase, 1, 2, 3, 4, 5, 50, 2) // fresh
	online := false
	h.srv.info.Online = func() bool { return online }
	var st stateBody
	decodeJSON(t, h.do(t, "GET", "/api/v1/state", RoleReadOnly, nil), &st)
	if st.Online {
		t.Fatal("online: true with the tracker saying offline (session closed)")
	}
	online = true
	decodeJSON(t, h.do(t, "GET", "/api/v1/state", RoleReadOnly, nil), &st)
	if !st.Online {
		t.Fatal("online: false with the tracker saying online")
	}
}

// TestStateEPS: the EPS (backup) port output is power.eps_w in /state
// and in the trailing-window average.
func TestStateEPS(t *testing.T) {
	h := newHarness(t)
	for i := range 12 {
		h.st.RecordTelemetry(testConnID, testBase.Add(-time.Duration(i)*5*time.Second), frame.Tele{Sol: 2000, Acport: 1500, Eps: 400})
	}
	h.st.Flush()
	var st struct {
		Power struct {
			EpsW int `json:"eps_w"`
		} `json:"power"`
	}
	decodeJSON(t, h.do(t, "GET", "/api/v1/state", RoleReadOnly, nil), &st)
	if st.Power.EpsW != 400 {
		t.Fatalf("state eps_w %d, want 400", st.Power.EpsW)
	}
	decodeJSON(t, h.do(t, "GET", "/api/v1/power/average?window=5m", RoleReadOnly, nil), &st)
	if st.Power.EpsW != 400 {
		t.Fatalf("average eps_w %d, want 400", st.Power.EpsW)
	}
}

// TestStateCapacityUnknown (F12, audit M6): with no battery capacity set
// the capacity and the stored energy are null, never a guessed pack.
func TestStateCapacityUnknown(t *testing.T) {
	h := newHarness(t)
	h.seedFullState()
	rr := h.do(t, "GET", "/api/v1/state", RoleReadOnly, nil)
	var body struct {
		Battery map[string]any `json:"battery"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Battery["capacity_wh"] != nil || body.Battery["stored_energy_wh"] != nil {
		t.Fatalf("battery with no capacity set: %v", body.Battery)
	}
}

// TestStateTemperatures (F3): /state carries the live temperatures and
// the BMS charge limit from the newest sample, and the cell
// temperatures from the newest battery snapshot while it is fresh;
// null without them.
func TestStateTemperatures(t *testing.T) {
	h := newHarness(t)
	var b struct {
		Temps   map[string]*float64 `json:"temperatures"`
		Battery struct {
			Limit *float64 `json:"bms_charge_limit_a"`
		} `json:"battery"`
	}
	rec := h.do(t, "GET", "/api/v1/state", RoleReadOnly, nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	if b.Temps["battery_c"] != nil || b.Battery.Limit != nil {
		t.Fatalf("empty store: %+v", b)
	}
	p := snap61Payload(50, 600, nil)
	p[64], p[65] = 0x18, 0x74
	copy(p[66:], []byte{0xfd, 0x00, 0xaf, 0x00}) // max 25.3, min 17.5 °C
	now := h.clk.get()
	h.seedFrame(now.Add(-time.Minute), dirC2S, classSnap61, decisionForwarded, p, nil)
	h.st.RecordTelemetry(testConnID, now.Add(-time.Second), frame.Tele{
		Soc:   50,
		Temps: &frame.Temps{BattDeciC: 242, InvDeciC: 290, AmbDeciC: 325, BMSChargeDeciA: 350},
	})
	h.st.Flush()
	rec = h.do(t, "GET", "/api/v1/state", RoleReadOnly, nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{"battery_c": 24.2, "inverter_c": 29, "ambient_c": 32.5, "cell_min_c": 17.5, "cell_max_c": 25.3}
	for k, v := range want {
		if b.Temps[k] == nil || *b.Temps[k] != v {
			t.Errorf("%s: %v, want %v", k, b.Temps[k], v)
		}
	}
	if b.Battery.Limit == nil || *b.Battery.Limit != 35 {
		t.Errorf("bms_charge_limit_a: %v", b.Battery.Limit)
	}
}

// TestStateInverterState: /state names the inverter's state from the
// live sample (foxess_modbus's names) and says whether the grid is
// connected; both are unknown without a reading.
func TestStateInverterState(t *testing.T) {
	h := newHarness(t)
	var b struct {
		State string `json:"inverter_state"`
		Grid  *bool  `json:"grid_connected"`
	}
	decode(t, h.do(t, httpGet, "/api/v1/state", RoleReadOnly, nil), &b)
	if b.State != "unknown" || b.Grid != nil {
		t.Fatalf("empty store: %+v", b)
	}
	for v, want := range map[int]struct {
		name string
		grid bool
	}{3: {"on-grid", true}, 4: {"off-grid", false}} {
		s := v
		h.st.RecordTelemetry(testConnID, h.clk.get(), frame.Tele{Soc: 50, InvState: &s})
		h.st.Flush()
		decode(t, h.do(t, httpGet, "/api/v1/state", RoleReadOnly, nil), &b)
		if b.State != want.name || b.Grid == nil || *b.Grid != want.grid {
			t.Fatalf("state %d: %+v", v, b)
		}
	}
}
