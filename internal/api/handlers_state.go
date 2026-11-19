package api

import (
	"fmt"
	"math"
	"net/http"
	"time"

	"foxesslocal/internal/frame"
	"foxesslocal/internal/socest"
	"foxesslocal/internal/store"
)

// Frame classes the handlers filter on (frame.Kind; the store persists
// it as text).
const (
	classTele       = frame.KindTele
	classKeepalive  = frame.KindKeepalive
	classRSSI       = frame.KindRSSI
	classMbread     = frame.KindMbread
	classMbreadResp = frame.KindMbreadResp
	classMbWrite    = frame.KindMbWrite
	classMbecho     = frame.KindMbecho
	classCfg91      = frame.KindCfg91
	classRegister   = frame.KindRegister
	classOther      = frame.KindOther
)

// Register addresses (the profile §5 map; the command gateway and the
// state view share them).
const (
	regWorkModeAddr  = 41000 // work mode: 0 self-use, 1 feed-in (fn06)
	regImportLimAddr = 41021 // import limit W (fn10, ×2 regs)
	regExportLimAddr = 41023 // export limit W (fn10, ×2 regs)
	regMeterCompAddr = 41029 // meter compensation W (fn06, int16)
	// grid current limits, 0.1 A — confirmed by cloud writes 2026-11-16
	// (import 55.0 A → fn06 41027 = 550; export 39.0 A → 41028 = 390).
	// 41025 also reads 560 but is not the import limit.
	regImportCurAddr = 41027
	regExportCurAddr = 41028
	regModeEnAddr    = 48000 // scheduler master enable (fn10 r0, ×10)
	regMaxChgCurAddr = 41007 // max charge current, battery side (0.1 A)
	regMaxDisCurAddr = 41008 // max discharge current (0.1 A)
	regEpsOutputAddr = 41015 // EPS output: 3 UPS / 2 EPS / (1) disabled
	regPowerLimAddr  = 41016 // power limit %
	regLoadLimAddr   = 41200 // LoadLimitPower W
	regGenLimAddr    = 41201 // GenerationLimitPower W
	regSlotTableAddr = 48010 // scheduler slot table (fn10, ×80)
)

// Charge-state values (the 150 B telemetry battery-state field).
const (
	chargeStateIdle     = 2 // 0x02: idle (the inverter's own enum)
	chargeStateCharging = 4 // 0x04: charging
)

// Modbus function codes (the command channel, profile §5).
const (
	fn03 = 0x03 // read holding registers
	fn06 = 0x06 // write single register
	fn10 = 0x10 // write multiple registers
)

// Work-mode names (the spec's WorkMode enum).
const (
	workModeSelfUse        = "self-use"
	workModeFeedIn         = "feed-in"
	workModeBackup         = "backup"
	workModePeakShaving    = "peak-shaving"
	workModePowerStation   = "power-station"
	workModeForceCharge    = "force-charge"
	workModeForceDischarge = "force-discharge"
)

// Power is the spec's Power schema (the 150 B telemetry, signed W).
type Power struct {
	PVW      int `json:"pv_w"`
	GridW    int `json:"grid_w"`
	LoadW    int `json:"load_w"`
	AcPortW  int `json:"ac_port_w"`
	BatteryW int `json:"battery_w"`
	// EpsW is the EPS (backup / UPS) port output (0 when the port is off
	// or unloaded, and on telemetry without it).
	EpsW int `json:"eps_w"`
}

// Battery is the spec's Battery schema.
type Battery struct {
	SocPct int `json:"soc_pct"`
	// SocEstimatePct is the SoC between the inverter's whole-percent
	// ticks (socest, D-028): within [soc_pct − 0.5, soc_pct + 0.44], two
	// decimals. Null without an estimate for the reported tick.
	SocEstimatePct *float64 `json:"soc_estimate_pct"`
	// MeasuredCapacity is the pack's capacity measured between SoC ticks
	// (D-028); null without an estimator.
	MeasuredCapacity *MeasuredCapacity `json:"measured_capacity"`
	State            string            `json:"state"`
	StoredEnergyWh   *int              `json:"stored_energy_wh"`
	CapacityWh       *int              `json:"capacity_wh"`
	PackVoltageV     *float64          `json:"pack_voltage_v"`
	// BMSChargeLimitA is the BMS's charge-current limit (31025): it
	// follows the coldest cell's temperature and the SoC. Null without a
	// register reading.
	BMSChargeLimitA *float64 `json:"bms_charge_limit_a"`
}

// Temperatures are the live temperatures, °C (null when not reported):
// the inverter's own readings with each sample, the BMS's cell extremes
// from the battery snapshot (about every 5 minutes; null once it is
// more than store.CellTempMaxAge old).
type Temperatures struct {
	BatteryC  *float64 `json:"battery_c"`
	InverterC *float64 `json:"inverter_c"`
	AmbientC  *float64 `json:"ambient_c"`
	CellMinC  *float64 `json:"cell_min_c"`
	CellMaxC  *float64 `json:"cell_max_c"`
}

// deci converts a tenth-unit reading (0.1 °C / 0.1 A) to the unit.
func deci(v int) *float64 {
	f := float64(v) / tenthsPerUnit
	return &f
}

// temperaturesOf is the state's Temperatures.
func (s *Server) temperaturesOf(st store.State) Temperatures {
	var out Temperatures
	if t := st.Temps; t != nil {
		out.BatteryC, out.InverterC, out.AmbientC = deci(t.BattDeciC), deci(t.InvDeciC), deci(t.AmbDeciC)
	}
	if st.CellMinDeciC != nil && s.clock().Sub(st.SnapTS) <= store.CellTempMaxAge {
		out.CellMinC, out.CellMaxC = deci(*st.CellMinDeciC), deci(*st.CellMaxDeciC)
	}
	return out
}

// Limits is the spec's Limits schema: the power limits (41021/41023,
// U32 pairs) and the grid current limits (41027 import / 41028 export,
// 0.1 A — read-only; null until the first settings read).
type Limits struct {
	// the grid connection (import / export limits, enforced at the CT —
	// the FoxESS description; not verified here)
	ImportW        int      `json:"import_w"`
	ExportW        int      `json:"export_w"`
	ImportCurrentA *float64 `json:"import_current_a"`
	ExportCurrentA *float64 `json:"export_current_a"`
	// the inverter's grid port: LoadLimitPower (41200) caps its draw —
	// charging plus EPS pass-through, measured 2026-11-17 — and
	// GenerationLimitPower (41201) presumably its output (untested)
	LoadLimitW       *int `json:"load_limit_w"`
	GenerationLimitW *int `json:"generation_limit_w"`
	// the battery side (41007 / 41008, 0.1 A)
	MaxChargeCurrentA    *float64 `json:"max_charge_current_a"`
	MaxDischargeCurrentA *float64 `json:"max_discharge_current_a"`
	// PowerLimitPct (41016): the inverter's output as % of its rating
	PowerLimitPct *int `json:"power_limit_pct"`
}

// ScheduleGroup is the spec's ScheduleGroup (one of the 8 groups).
type ScheduleGroup struct {
	Index           int    `json:"index"`
	Enabled         bool   `json:"enabled"`
	Start           string `json:"start"`
	End             string `json:"end"`
	WorkMode        string `json:"work_mode"`
	MaxSocPct       int    `json:"max_soc_pct"`
	MinSocOnGridPct int    `json:"min_soc_on_grid_pct"`
	FdSocPct        int    `json:"fd_soc_pct"`
	FdPwrW          int    `json:"fd_pwr_w"`

	// Read-only detail (GET /state; absent from PUT /schedule's
	// read-back). Default: the enabled 00:00–23:59 group — the cloud's
	// "remaining time" mode, what runs outside every other window.
	Default bool `json:"default,omitempty"`
	// AfterCutoff: a force window's action once it reaches its cut-off
	// SoC ("standby" | "self-use"; absent when not a force window or the
	// value is not one we know).
	AfterCutoff *string `json:"after_cutoff,omitempty"`
	// From the group's parameter block (57000 + 50·(k−1), read with the
	// settings): the window's grid import / export limit (W) and whether
	// it uses PV. Absent until read.
	ImportLimitW *int  `json:"import_limit_w,omitempty"`
	ExportLimitW *int  `json:"export_limit_w,omitempty"`
	PV           *bool `json:"pv,omitempty"`
}

// afterCutoffs names the +8 values (2026-11-18: the force charge's
// "after charging cutoff" standby → self-use wrote 1 → 3; the force
// discharge's self-use reads 3).
var afterCutoffs = map[int]string{1: "standby", 3: workModeSelfUse}

// The force modes' schedule names, and the last minute of the day (the
// default group's end).
const (
	schedForceCharge    = "force-charge"
	schedForceDischarge = "force-discharge"
	hhmmDayEnd          = "23:59"
)

// Group parameter blocks (2026-11-18): group k's at 57000 + 50·(k−1);
// r1:r2 import limit, r3:r4 export limit (U32 W, high word first), r6
// the from-PV power (0 = PV off; 20000 when on).
const (
	groupParamBase   = 57000
	groupParamStride = 50
	gpImportHi       = 1
	gpExportHi       = 3
	gpPV             = 6
)

// Schedule is the spec's Schedule (the 48000/48010 table).
type Schedule struct {
	Enabled bool            `json:"enabled"`
	Groups  []ScheduleGroup `json:"groups"`
}

// State is the spec's State schema (the /state body).
type State struct {
	TS       time.Time  `json:"ts"`
	AsOf     *time.Time `json:"as_of,omitempty"`
	Online   bool       `json:"online"`
	Power    Power      `json:"power"`
	Battery  Battery    `json:"battery"`
	WorkMode string     `json:"work_mode"`
	// ActiveMode is what runs now: a schedule group's mode, or WorkMode
	// (null while the inverter's wall time is unknown: no fresh clock
	// reading and no site time zone).
	ActiveMode *ActiveMode `json:"active_mode"`
	// EpsMode is the EPS (backup) port setting: ups | eps | disabled
	// ("" unknown)
	EpsMode     string   `json:"eps_mode"`
	MeterCompW  int      `json:"meter_compensation_w"`
	Limits      Limits   `json:"limits"`
	Schedule    Schedule `json:"schedule"`
	ForwardMode string   `json:"forward_mode"`
	Identity    Identity `json:"identity"`
	// InverterState is the inverter's state (31027, foxess_modbus's
	// names): self-test | waiting | checking | on-grid | off-grid |
	// recoverable-fault | unrecoverable-fault | unknown.
	InverterState string `json:"inverter_state"`
	// GridConnected: on grid (true), off grid / EPS (false), else null.
	GridConnected *bool `json:"grid_connected"`
	// Temperatures are the live temperatures (°C).
	Temperatures Temperatures `json:"temperatures"`
	// Energy is the lifetime energy counters (kWh).
	Energy Energy `json:"energy"`
	// WriteBudget is each budgeted setting's write budget (WriteBudget).
	WriteBudget map[string]WriteBudget `json:"write_budget"`
}

// handleState is GET /state (spec getState): the normalized snapshot.
func (s *Server) handleState(w http.ResponseWriter, _ *http.Request, _ Token) {
	st, err := s.st.QueryState()
	if err != nil {
		s.internal(w, err)
		return
	}
	body, err := s.buildState(st)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// buildState maps the store's State to the spec's State schema (plus
// the config registers, scanned from the confirmed writes).
// batteryOf is the state's Battery: SoC, state, capacity, stored energy
// and the pack voltage.
func (s *Server) batteryOf(st store.State) Battery {
	b := Battery{SocPct: socPct(st), State: batteryState(st)}
	capWh := s.info.capacityWh()
	if capWh > 0 {
		b.CapacityWh = &capWh
	}
	b.SocEstimatePct = s.socEstimate(b.SocPct)
	b.MeasuredCapacity = s.measuredCapacity()
	if capWh > 0 && (!st.SnapTS.IsZero() || !st.TeleTS.IsZero()) {
		// usable energy = SoC × capacity (the snap61 1907 counter's unit
		// is undetermined — profile §4), from the estimate when there is one
		wh := (b.SocPct*capWh + 50) / 100
		if e := b.SocEstimatePct; e != nil {
			wh = int(math.Round(*e * float64(capWh) / percent))
		}
		b.StoredEnergyWh = &wh
	}
	if !st.SnapTS.IsZero() && len(st.BusVoltage) > 0 {
		v := float64(st.BusVoltage[0]) / 100.0
		b.PackVoltageV = &v
	}
	return b
}

// socEstimate is the SoC estimate for the reported tick, rounded to two
// decimals; nil without one (no estimator, no capacity, or an estimate
// of another tick — the snap61 burst is newer than the telemetry).
func (s *Server) socEstimate(socPct int) *float64 {
	if s.socEst == nil {
		return nil
	}
	r, ok := s.socEst.Latest()
	if !ok || r.Raw != socPct {
		return nil
	}
	v := RoundPct(r.Pct)
	return &v
}

// percent is 100 %; hundredths rounds to two decimals.
const (
	percent    = 100
	hundredths = 100
)

// MeasuredCapacity is the energy between two consecutive single-step
// SoC ticks, × 100, per direction (D-028): the pack's capacity as the
// inverter's battery power sees it — less than the nominal on a pack
// that has lost health, and it includes the losses between the
// measurement and the cells. A direction is null until enough clean
// intervals agree (the estimate applies it from then on).
type MeasuredCapacity struct {
	ChargingWh           *int `json:"charging_wh"`
	DischargingWh        *int `json:"discharging_wh"`
	ChargingIntervals    int  `json:"charging_intervals"`
	DischargingIntervals int  `json:"discharging_intervals"`
}

// measuredCapacity is the learnt capacity; nil without an estimator.
func (s *Server) measuredCapacity() *MeasuredCapacity {
	if s.socEst == nil {
		return nil
	}
	l := s.socEst.Learned()
	wh := func(d socest.Dir) *int {
		if !d.Ready() {
			return nil
		}
		v := int(math.Round(d.WhPerPct * percent))
		return &v
	}
	return &MeasuredCapacity{
		ChargingWh: wh(l.Charging), DischargingWh: wh(l.Discharging),
		ChargingIntervals: l.Charging.N, DischargingIntervals: l.Discharging.N,
	}
}

// RoundPct rounds an estimated percentage to the two decimals the API
// carries.
func RoundPct(v float64) float64 { return math.Round(v*hundredths) / hundredths }

func (s *Server) buildState(st store.State) (State, error) {
	body := State{TS: s.clock()}
	id, err := s.stateIdentity(st)
	if err != nil {
		return State{}, err
	}
	body.Identity = id

	body.Power = Power{
		PVW: int(st.Sol), GridW: int(st.Grid), LoadW: int(st.Haus),
		AcPortW: int(st.Acport), BatteryW: int(st.Batt), EpsW: int(st.Eps),
	}
	body.Battery = s.batteryOf(st)
	if st.Temps != nil {
		body.Battery.BMSChargeLimitA = deci(st.Temps.BMSChargeDeciA)
	}
	body.Temperatures = s.temperaturesOf(st)
	body.InverterState, body.GridConnected = inverterState(st.InvState)
	s.hostParts(&body)

	var newest time.Time
	for _, ts := range []time.Time{st.TeleTS, st.SnapTS, st.SchedTS} {
		if ts.After(newest) {
			newest = ts
		}
	}
	cfg := s.scanConfigRegs(newest)
	newest = cfg.newest

	body.WorkMode = workModeSelfUse
	if cfg.mode == 1 {
		body.WorkMode = workModeFeedIn
	}
	body.MeterCompW = cfg.comp
	body.Limits = cfg.limits()
	body.EpsMode = cfg.epsMode
	body.Schedule = scheduleOf(st.Schedule, cfg.schedOn)
	s.groupParams(body.Schedule.Groups)
	body.ActiveMode = s.activeModeNow(body.Schedule, body.WorkMode)

	mode, _, err := s.fwd.Get()
	if err != nil {
		return State{}, err
	}
	body.ForwardMode = string(mode)

	// as_of: the newest reflected frame (any source); nil when the store
	// is empty (the field is then omitted).
	if !newest.IsZero() {
		body.AsOf = &newest
	}
	// online: the 5 s tele heartbeat (the snap burst is ~293 s and the
	// config writes are sporadic, so the tele is the liveness signal;
	// the spec: treat > 15 s of silence as offline).
	switch {
	case s.info.Online != nil:
		body.Online = s.info.Online()
	case !st.TeleTS.IsZero():
		body.Online = s.clock().Sub(st.TeleTS) <= OnlineWindow
	}
	return body, nil
}

// StateFor builds the /state snapshot from the live store + forward
// state. Task 10's host observer adapter needs the full State in its
// config events (the spec's ConfigEvent carries the new snapshot), and
// the seam is the one state builder — the handler is its other caller.
func StateFor(st *store.Store, fwd ForwardModeState, clock func() time.Time, info ServerInfo) (State, error) {
	sst, err := st.QueryState()
	if err != nil {
		return State{}, err
	}
	return (&Server{st: st, fwd: fwd, clock: clock, info: info}).buildState(sst)
}

// configRegs is the confirmed-write scan result for the state view's
// config registers.
type configRegs struct {
	mode    int
	comp    int
	imp     int
	exp     int
	schedOn bool
	impCur  *float64
	expCur  *float64
	loadLim *int
	genLim  *int
	chgCur  *float64
	disCur  *float64
	powPct  *int
	epsMode string
	newest  time.Time
}

// limits is the state's Limits from the scanned registers.
func (c configRegs) limits() Limits {
	return Limits{
		ImportW: c.imp, ExportW: c.exp,
		ImportCurrentA: c.impCur, ExportCurrentA: c.expCur,
		LoadLimitW: c.loadLim, GenerationLimitW: c.genLim,
		MaxChargeCurrentA: c.chgCur, MaxDischargeCurrentA: c.disCur,
		PowerLimitPct: c.powPct,
	}
}

// scanConfigRegs takes, per config register (work mode, meter
// compensation, the limits pair, the scheduler master), the newer of the
// latest confirmed write and the settings poll's latest read
// and folds their timestamps into the newest reflected frame (a
// register scan failure is not a state failure — the store rows it
// reads are the whole input).
func (s *Server) scanConfigRegs(newest time.Time) configRegs {
	var out configRegs
	out.newest = newest
	// the store keeps each register's latest known value: the settings
	// poll's reads and every write that reached the inverter (in memory)
	scan := func(addr int, set func(int)) {
		v, ts, ok, err := s.st.RegisterRead(addr)
		if err != nil || !ok {
			return
		}
		if ts.After(out.newest) {
			out.newest = ts
		}
		set(v)
	}
	scan(regWorkModeAddr, func(v int) { out.mode = v })
	scan(regMeterCompAddr, func(v int) { out.comp = int16Value(v) })
	// the limits are U32 pairs, high word first (each word from its own
	// newest source)
	scan(regImportLimAddr, func(v int) { out.imp |= v << u16Bits })
	scan(regImportLimAddr+1, func(v int) { out.imp |= v })
	scan(regExportLimAddr, func(v int) { out.exp |= v << u16Bits })
	scan(regExportLimAddr+1, func(v int) { out.exp |= v })
	scan(regModeEnAddr, func(v int) { out.schedOn = v != 0 })
	scan(regImportCurAddr, func(v int) { out.impCur = deciAmps(v) })
	scan(regExportCurAddr, func(v int) { out.expCur = deciAmps(v) })
	intp := func(v int) *int { return &v }
	scan(regLoadLimAddr, func(v int) { out.loadLim = intp(v) })
	scan(regGenLimAddr, func(v int) { out.genLim = intp(v) })
	scan(regMaxChgCurAddr, func(v int) { out.chgCur = deciAmps(v) })
	scan(regMaxDisCurAddr, func(v int) { out.disCur = deciAmps(v) })
	scan(regPowerLimAddr, func(v int) { out.powPct = intp(v) })
	scan(regEpsOutputAddr, func(v int) { out.epsMode = epsModes[v] })
	return out
}

// batteryState is the spec's BatteryState from the 150 B telemetry
// (the 5 s cadence — the snap61 burst is not the live source; the
// charge-state byte is the inverter's own enum, with the battery W
// sign as the fallback for an unknown code).
// batteryStateUnknown is the spec's BatteryState for a store with no
// telemetry yet.
// The battery states (the spec's BatteryState).
const (
	batteryStateUnknown     = "unknown"
	batteryStateCharging    = "charging"
	batteryStateDischarging = "discharging"
)

func batteryState(st store.State) string {
	if st.TeleTS.IsZero() {
		return batteryStateUnknown
	}
	switch st.ChargeState {
	case chargeStateCharging:
		return batteryStateCharging
	case chargeStateIdle:
		return "idle"
	}
	switch {
	case st.Batt > 0:
		return batteryStateCharging
	case st.Batt < 0:
		return batteryStateDischarging
	}
	return "idle"
}

// scheduleWorkModes maps the wire enum (1..7) to the spec's Schedule
// work-mode names (0/unknown → self-use, the disabled-group default).
var scheduleWorkModes = map[int]string{
	1: workModeSelfUse,
	2: workModeFeedIn,
	3: workModeBackup,
	4: workModePeakShaving,
	5: workModePowerStation,
	6: workModeForceCharge,
	7: workModeForceDischarge,
}

// scheduleOf maps the store's slot table to the spec's Schedule
// (always 8 groups; an absent group is a disabled zero group).
func scheduleOf(slots []store.Slot, enabled bool) Schedule {
	groups := make([]ScheduleGroup, scheduleGroups)
	for i := range scheduleGroups {
		g := ScheduleGroup{
			Index: i + 1, Start: hhmmZero, End: hhmmZero,
			WorkMode: workModeSelfUse,
		}
		if i < len(slots) {
			sl := slots[i]
			g.Enabled = sl.Enabled
			g.Start = hhmm(sl.StartH, sl.StartM)
			g.End = hhmm(sl.EndH, sl.EndM)
			g.WorkMode = scheduleWorkMode(sl.WorkMode)
			g.MaxSocPct = sl.MaxSoc
			g.MinSocOnGridPct = sl.MinSoc
			g.FdSocPct = sl.FDSoc
			g.FdPwrW = sl.FDPower
			g.Default = sl.Enabled && g.Start == hhmmZero && g.End == hhmmDayEnd
			if g.WorkMode == schedForceCharge || g.WorkMode == schedForceDischarge {
				if name, ok := afterCutoffs[sl.AfterCutoff]; ok {
					g.AfterCutoff = &name
				}
			}
		}
		groups[i] = g
	}
	return Schedule{Enabled: enabled, Groups: groups}
}

// groupParams fills each group's import / export limit and PV use from
// its parameter block, where the settings poll has read it.
func (s *Server) groupParams(groups []ScheduleGroup) {
	reg := func(addr int) (int, bool) {
		v, _, ok, err := s.st.RegisterRead(addr)
		return v, ok && err == nil
	}
	u32 := func(addr int) *int {
		hi, ok1 := reg(addr)
		lo, ok2 := reg(addr + 1)
		if !ok1 || !ok2 {
			return nil
		}
		v := hi<<u16Bits | lo
		return &v
	}
	for i := range groups {
		base := groupParamBase + groupParamStride*i
		groups[i].ImportLimitW = u32(base + gpImportHi)
		groups[i].ExportLimitW = u32(base + gpExportHi)
		if v, ok := reg(base + gpPV); ok {
			pv := v != 0
			groups[i].PV = &pv
		}
	}
}

// scheduleWorkMode maps one wire work-mode value.
func scheduleWorkMode(wire int) string {
	if name, ok := scheduleWorkModes[wire]; ok {
		return name
	}
	return workModeSelfUse
}

// hhmm formats one register HH:MM pair.
func hhmm(h, m int) string {
	return fmt.Sprintf("%02d:%02d", h, m)
}

// int16Modulus is the int16 two's-complement modulus (the meter
// compensation register is signed on the wire).
const int16Modulus = 0x10000

// deciAmps converts a 0.1 A register to amps.
func deciAmps(v int) *float64 {
	a := float64(v) / deciPerUnit
	return &a
}

// epsModes names the 41015 EPS output values (3 / 2 confirmed by cloud
// writes; 1 = disabled presumed — the schema lists UpsMode, Disable,
// EpsMode).
var epsModes = map[int]string{1: "disabled", 2: "eps", 3: "ups"}

// deciPerUnit is the 0.1-unit register scale.
const deciPerUnit = 10.0

// u16Bits is the width of one register (the high word of a U32 pair).
const u16Bits = 16

// int16Value decodes one Modbus register as signed int16 (the meter
// compensation is two's complement on the wire).
func int16Value(v int) int {
	if v > int16Modulus/2-1 {
		return v - int16Modulus
	}
	return v
}

// socPct is the battery SoC from the newest source that carries one:
// the 5 s telemetry or the ~5 min snap61 burst (both report the
// inverter's own SoC; the current uplink form sends only the snap61 —
// profile §4 note).
func socPct(st store.State) int {
	if !st.SnapTS.IsZero() && st.SnapTS.After(st.TeleTS) {
		return st.SocPct
	}
	return st.Soc
}

// inverterStates names the inverter state register (31027; foxess_modbus).
var inverterStates = []string{"self-test", "waiting", "checking", "on-grid", "off-grid", "recoverable-fault", "unrecoverable-fault"}

// Inverter states with a grid meaning.
const (
	invOnGrid  = 3
	invOffGrid = 4
)

// inverterState names a state and says whether the grid is connected.
func inverterState(v *int) (string, *bool) {
	if v == nil || *v < 0 || *v >= len(inverterStates) {
		return "unknown", nil
	}
	var grid *bool
	switch *v {
	case invOnGrid:
		grid = new(true)
	case invOffGrid:
		grid = new(false)
	}
	return inverterStates[*v], grid
}

// hostParts fills what the host provides: the energy counters and the
// write budget.
func (s *Server) hostParts(body *State) {
	body.WriteBudget = s.writeBudget()
	body.Energy = Energy{Sources: map[string]string{}}
	if s.info.Energy != nil {
		body.Energy = s.info.Energy()
	}
}
