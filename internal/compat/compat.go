// Package compat grades how far the app's register meanings can be
// trusted on the identified inverter, per feature (design D-020). It is
// a pure function of the identity and the latest register reads; it
// reports, it does not gate (C-008 gating is design step 4).
package compat

import (
	"fmt"
	"regexp"
	"strings"

	"foxesslocal/internal/driver"
	"foxesslocal/internal/fmdata"
	"foxesslocal/internal/frame"
	"foxesslocal/internal/identity"
)

// Level is a feature's confidence.
type Level string

// Confidence levels (D-020). Tentative arrives with the foxess_modbus
// import (design step 3).
const (
	LevelExact      Level = "exact"      // verified profile, checks pass
	LevelCompatible Level = "compatible" // same family + generation, checks pass
	LevelTentative  Level = "tentative"  // a third-party (foxess_modbus) map, checks pass
	LevelUnknown    Level = "unknown"    // unrecognised device, or a check failed
	LevelPending    Level = "pending"    // not enough data read yet
)

// Check statuses.
const (
	StatusPass    = "pass"
	StatusFail    = "fail"
	StatusPending = "pending" // its registers have not been read yet
	StatusSkip    = "skip"    // not applicable now (e.g. an idle PV string)
)

// Feature names (the units C-008 gates).
const (
	FeatureLiveValues        = "live_values"
	FeatureSettingsRead      = "settings_read"
	FeatureWorkMode          = "work_mode"
	FeaturePowerLimits       = "power_limits"
	FeatureCurrentLimits     = "current_limits"
	FeatureMeterCompensation = "meter_compensation"
	FeatureSchedule          = "schedule"
	FeatureBatteryCurrents   = "battery_currents" // 41007 / 41008
	FeaturePowerLimit        = "power_limit"      // 41016
	FeaturePortLimits        = "port_limits"      // 41200 / 41201
	FeatureEPSMode           = "eps_mode"         // 41015
)

// Register addresses the checks read (protocol profile §5).
const (
	regWorkMode  = 41000
	regImportLim = 41021 // U32 41021:41022
	regExportLim = 41023 // U32 41023:41024
	regImportCur = 41027
	regExportCur = 41028
	regMeterComp = 41029
	// the installer settings (profile §5, 2026-11-16 cloud writes).
	regMaxChargeCur    = 41007
	regMaxDischargeCur = 41008
	regEPSMode         = 41015
	regPowerLimit      = 41016
	regLoadLimit       = 41200
	regGenLimit        = 41201
	regSchedMode       = 48000
	regSchedTable      = 48010
)

// Features is the evaluation order.
var Features = []string{
	FeatureLiveValues, FeatureSettingsRead, FeatureWorkMode, FeaturePowerLimits,
	FeatureCurrentLimits, FeatureMeterCompensation, FeatureSchedule,
	FeatureBatteryCurrents, FeaturePowerLimit, FeaturePortLimits, FeatureEPSMode,
}

// Input is what the evaluation needs.
type Input struct {
	Model    string
	Firmware identity.Firmware
	// Regs returns the latest read of one holding register.
	Regs func(addr int) (uint16, bool)
	// SnapshotSoc is the inverter's own snapshot SoC (snap61), if seen.
	SnapshotSoc *int
}

// Check is one plausibility check's outcome.
type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// Feature is one feature's grade.
type Feature struct {
	Name   string  `json:"name"`
	Level  Level   `json:"level"`
	Reason string  `json:"reason"`
	Checks []Check `json:"checks"`
}

// Report is the whole evaluation.
type Report struct {
	Profile string `json:"verified_profile,omitempty"`
	Family  string `json:"family"`
	// Layout is the register layout established from the registers that
	// answer ("" when not established) — the driver key.
	Layout   string    `json:"layout"`
	Features []Feature `json:"features"`
	// ThirdParty is the foxess_modbus map for the identified layout
	// (tentative source, D-021) and its disagreements with the verified
	// profile.
	ThirdParty *ThirdParty `json:"third_party,omitempty"`
}

// ThirdParty describes the foxess_modbus map behind a layout.
type ThirdParty struct {
	Source      string       `json:"source"`
	Commit      string       `json:"commit"`
	Map         string       `json:"map"`
	Divergences []Divergence `json:"divergences"`
}

// khModel is the foxess_modbus KH model pattern.
var khModel = regexp.MustCompile(`^KH([\d.]+)`)

// Evaluate grades every feature.
func Evaluate(in Input) Report {
	r := reader{get: in.Regs}
	kh := khModel.MatchString(in.Model)
	if kh && r.has(identity.VersionAddr, identity.VersionCount) {
		// the version registers at 30016 answer: the KH_PRE133 layout
		// (the 36001 block of the later layout does not, profile §5)
		return evaluateKHPre133(in, r)
	}
	if cands := Candidates(in.Model); len(cands) > 0 {
		return evaluateTentative(in, cands)
	}
	rep := Report{}
	for _, name := range Features {
		rep.Features = append(rep.Features, Feature{
			Name: name, Level: LevelUnknown,
			Reason: "model not recognised: no trusted or third-party mapping",
		})
	}
	return rep
}

// evaluateKHPre133 grades the verified-family layout.
func evaluateKHPre133(in Input, r reader) Report {
	profile, exact := identity.MatchVerified(in.Model, in.Firmware)
	rep := Report{Family: "KH (KH_PRE133 register layout)", Layout: driver.LayoutKHPre133}
	if exact {
		rep.Profile = profile.Name
	}
	rep.ThirdParty = thirdParty(khPre133Map, profile)
	checks := map[string][]Check{
		FeatureLiveValues:        liveChecks(r, in.SnapshotSoc),
		FeatureSettingsRead:      {settingsRanges(r)},
		FeatureWorkMode:          {workMode(r)},
		FeaturePowerLimits:       {u32Limit(r, "import_limit_range", regImportLim), u32Limit(r, "export_limit_range", regExportLim)},
		FeatureCurrentLimits:     {deciAmps(r, "import_current_range", regImportCur), deciAmps(r, "export_current_range", regExportCur)},
		FeatureMeterCompensation: {meterComp(r)},
		FeatureSchedule:          {scheduleTable(r)},
		FeatureBatteryCurrents: {
			inRange(r, "max_charge_current_range", regMaxChargeCur, 0, maxBattDeciAmps),
			inRange(r, "max_discharge_current_range", regMaxDischargeCur, 0, maxBattDeciAmps),
		},
		FeaturePowerLimit: {inRange(r, "power_limit_range", regPowerLimit, 0, socMax)},
		FeaturePortLimits: {
			inRange(r, "load_limit_read", regLoadLimit, 0, maxU16),
			inRange(r, "generation_limit_read", regGenLimit, 0, maxU16),
		},
		FeatureEPSMode: {inRange(r, "eps_mode_enum", regEPSMode, epsModeMin, epsModeMax)},
	}
	for _, name := range Features {
		f := Feature{Name: name, Checks: checks[name]}
		f.Level, f.Reason = grade(f.Checks, true, exact)
		rep.Features = append(rep.Features, f)
	}
	return rep
}

// evaluateTentative grades a device by its foxess_modbus candidate maps
// (design D-021): the first map whose live readings pass the physical
// checks is the layout. Only what that map covers is graded tentative;
// every other feature is unknown (not mapped) — the KH settings decode
// is never applied to another layout.
func evaluateTentative(in Input, cands []Candidate) Report {
	sel, ok, anyData := SelectMap(cands, in.Regs)
	rep := Report{}
	if !ok {
		lvl, reason := LevelPending, "register layout not established: the candidate maps' live registers are not read yet"
		rep.Family = cands[0].Model + " (layout not established)"
		if anyData {
			lvl = LevelUnknown
			reason = "no foxess_modbus map for this model passes the live checks"
			rep.Family = cands[0].Model + " (no map passes the live checks)"
		}
		for _, name := range Features {
			rep.Features = append(rep.Features, Feature{Name: name, Level: lvl, Reason: reason})
		}
		return rep
	}
	rep.Layout = sel.Key
	rep.Family = fmt.Sprintf("%s (foxess_modbus %s, tentative)", sel.Model, sel.Key)
	rep.ThirdParty = thirdParty(sel.Key, identity.Profile{})
	for _, name := range Features {
		f := Feature{Name: name}
		switch name {
		case FeatureLiveValues:
			f.Checks = LiveChecks(FromMap(sel.Map, in.Regs), in.SnapshotSoc)
			f.Level, f.Reason = tentativeGrade(f.Checks)
		case FeatureWorkMode:
			f.Checks = []Check{mapEnum(sel.Map, "work_mode", in.Regs)}
			f.Level, f.Reason = tentativeGrade(f.Checks)
		default:
			f.Level, f.Reason = LevelUnknown, "not mapped for this model (foxess_modbus "+sel.Key+")"
		}
		rep.Features = append(rep.Features, f)
	}
	return rep
}

// tentativeGrade is grade for a third-party mapping.
func tentativeGrade(cs []Check) (Level, string) {
	lvl, reason := grade(cs, true, false)
	if lvl == LevelCompatible {
		return LevelTentative, "foxess_modbus mapping (unverified); checks pass"
	}
	return lvl, reason
}

// mapEnum checks a select entity's value is one of its declared options.
func mapEnum(m fmdata.Map, key string, get func(int) (uint16, bool)) Check {
	name := key + "_enum"
	e, ok := m.Entity(key)
	if !ok || len(e.Addresses) != 1 {
		return Check{Name: name, Status: StatusSkip, Detail: "not in the map"}
	}
	v, ok := get(e.Addresses[0])
	if !ok {
		return pending(name)
	}
	if _, ok := e.Values[fmt.Sprint(v)]; !ok {
		return fail(name, "%d = %d, not one of the map's options", e.Addresses[0], v)
	}
	return pass(name)
}

// Candidate is one foxess_modbus map a model could be using.
type Candidate struct {
	Model      string
	Key        string
	Map        fmdata.Map
	Fn         int // 3 holding / 4 input
	Invalid    [][2]int
	Individual [][2]int
}

// Read function codes.
const (
	fnHolding = 3
	fnInput   = 4
	// maxReadCount is the Modbus fn03/04 register limit.
	maxReadCount = 125
)

// Candidates lists the maps a model's foxess_modbus profile allows, in
// upstream's order, each once.
func Candidates(model string) []Candidate {
	d := fmdata.Load()
	p, ok := d.MatchModel(model)
	if !ok {
		return nil
	}
	var out []Candidate
	seen := map[string]bool{}
	for _, c := range p.Connections {
		fn := fnHolding
		if c.RegisterType == "INPUT" {
			fn = fnInput
		}
		for _, v := range c.Versions {
			m, ok := d.Map(v.Map)
			if !ok || seen[v.Map] {
				continue
			}
			seen[v.Map] = true
			out = append(out, Candidate{
				Model: p.Model, Key: v.Map, Map: m, Fn: fn,
				Invalid: c.InvalidRanges, Individual: c.IndividualRanges,
			})
		}
	}
	return out
}

// SettingAddrs are the single settings registers a tentative layout's
// map covers and the app grades (the work-mode select), minus invalid.
func SettingAddrs(c Candidate) []int {
	var out []int
	for _, e := range c.Map {
		if e.Key == "work_mode" && e.Type == "select" && len(e.Addresses) == 1 &&
			!fmdata.Invalid(e.Addresses[0], c.Invalid) {
			out = append(out, e.Addresses[0])
		}
	}
	return out
}

// Plan is the candidate's live-value reads (special registers honoured).
func (c Candidate) Plan() []fmdata.Block {
	return fmdata.PlanReads(LiveAddrs(c.Map), c.Invalid, c.Individual, maxReadCount)
}

// SelectMap returns the first candidate whose live registers are all
// read and pass the core live checks; anyData reports whether any
// candidate's registers were complete (so "none passes" is a finding,
// not missing data).
func SelectMap(cands []Candidate, get func(int) (uint16, bool)) (Candidate, bool, bool) {
	anyData := false
	for _, c := range cands {
		complete := true
		for _, a := range LiveAddrs(c.Map) {
			if _, ok := get(a); !ok {
				complete = false
			}
		}
		if !complete {
			continue
		}
		anyData = true
		if Classifiable(LiveChecks(FromMap(c.Map, get), nil)) {
			return c, true, true
		}
	}
	return Candidate{}, false, anyData
}

// khPre133Map is the foxess_modbus map of the KH_PRE133 layout.
const khPre133Map = "KH_PRE133-HOLDING"

// thirdParty describes the foxess_modbus map for a layout.
func thirdParty(key string, p identity.Profile) *ThirdParty {
	d := fmdata.Load()
	m, ok := d.Map(key)
	if !ok {
		return nil
	}
	return &ThirdParty{Source: d.Source.Repo, Commit: d.Source.Commit, Map: key, Divergences: Divergences(p, m)}
}

// grade turns check outcomes into a level.
func grade(cs []Check, known, exact bool) (Level, string) {
	if !known {
		return LevelUnknown, "model / register layout not recognised: no trusted mapping"
	}
	var failed, pending []string
	for _, c := range cs {
		switch c.Status {
		case StatusFail:
			failed = append(failed, c.Name)
		case StatusPending:
			pending = append(pending, c.Name)
		}
	}
	switch {
	case len(failed) > 0:
		return LevelUnknown, "check failed: " + strings.Join(failed, ", ")
	case len(pending) > 0:
		return LevelPending, "not read yet: " + strings.Join(pending, ", ")
	case exact:
		return LevelExact, "verified profile; checks pass"
	}
	return LevelCompatible, "same family and register layout, unverified firmware; checks pass"
}

// reader wraps the register source.
type reader struct{ get func(int) (uint16, bool) }

func (r reader) has(addr, count int) bool {
	for i := range count {
		if _, ok := r.get(addr + i); !ok {
			return false
		}
	}
	return true
}

func (r reader) block(addr, count int) ([]uint16, bool) {
	out := make([]uint16, count)
	for i := range count {
		v, ok := r.get(addr + i)
		if !ok {
			return nil, false
		}
		out[i] = v
	}
	return out, true
}

func pass(name string) Check { return Check{Name: name, Status: StatusPass} }

func fail(name, format string, a ...any) Check {
	return Check{Name: name, Status: StatusFail, Detail: fmt.Sprintf(format, a...)}
}

func pending(name string) Check { return Check{Name: name, Status: StatusPending} }

// Live-value tolerances.
const (
	pvActiveW       = 100  // a string under this is idle: V×A not checked
	pvAbsTolW       = 50   // V×A vs W: absolute slack
	pvRelTol        = 0.10 // … or 10 %
	socMax          = 100
	socSnapTol      = 5
	balanceAbsTolW  = 300 // load vs inverter + grid
	balanceRelTol   = 0.10
	installerSocMin = 10
)

// liveChecks are the physical-consistency checks on the KH live values.
func liveChecks(r reader, snapSoc *int) []Check {
	a, okA := r.block(frame.KHRealtimeAAddr, frame.KHRealtimeACount)
	b, okB := r.block(frame.KHRealtimeBAddr, frame.KHRealtimeBCount)
	if !okA || !okB {
		return LiveChecks(LiveValues{}, snapSoc)
	}
	rt, err := frame.ParseKHRealtime(a, b)
	if err != nil {
		return []Check{fail("decode", "%v", err)}
	}
	return LiveChecks(FromKH(rt), snapSoc)
}

const (
	deci      = 10.0
	centi     = 100.0
	gridMinV  = 180.0
	gridMaxV  = 270.0
	gridMinHz = 45.0
	gridMaxHz = 65.0
)

// rangeF passes when lo <= v <= hi; nil is pending.
func rangeF(name string, v *float64, lo, hi float64, format string) Check {
	switch {
	case v == nil:
		return pending(name)
	case *v < lo || *v > hi:
		return fail(name, format, *v)
	}
	return pass(name)
}

// pvProduct: on every active string, V × A matches W.
func pvProduct(v LiveValues) Check {
	const name = "pv_power_product"
	present, active := 0, 0
	for i, s := range v.PV {
		if !s.Present {
			continue
		}
		present++
		va := s.V * s.A
		if s.W < pvActiveW && va < pvActiveW {
			continue
		}
		active++
		tol := max(pvAbsTolW, pvRelTol*s.W)
		if d := va - s.W; d > tol || d < -tol {
			return fail(name, "PV%d: %.0f V·A vs %.0f W", i+1, va, s.W)
		}
	}
	switch {
	case present == 0:
		return pending(name)
	case active == 0:
		return Check{Name: name, Status: StatusSkip, Detail: "no active string"}
	}
	return pass(name)
}

// socAgrees: the polled SoC matches the inverter's snapshot SoC.
func socAgrees(soc *float64, snapSoc *int) Check {
	const name = "soc_agrees_snapshot"
	switch {
	case soc == nil:
		return pending(name)
	case snapSoc == nil:
		return Check{Name: name, Status: StatusSkip, Detail: "no snapshot yet"}
	case fabs(*soc-float64(*snapSoc)) > socSnapTol:
		return fail(name, "polled %.0f %% vs snapshot %d %%", *soc, *snapSoc)
	}
	return pass(name)
}

// powerBalance: household load ≈ inverter AC output + grid import. A
// map without inverter output power estimates it as PV − battery
// charging, with a looser tolerance for the conversion losses.
func powerBalance(v LiveValues) Check {
	const name = "power_balance"
	if v.LoadW == nil || v.GridImportW == nil {
		return pending(name)
	}
	relTol := balanceRelTol
	var inv float64
	switch {
	case v.InverterW != nil:
		inv = *v.InverterW
	case v.BatteryW != nil:
		relTol = estimatedBalanceRelTol
		for _, s := range v.PV {
			if s.Present {
				inv += s.W
			}
		}
		inv -= *v.BatteryW
	default:
		return pending(name)
	}
	load, grid := *v.LoadW, *v.GridImportW
	if fabs(load) < noFlowW && fabs(inv) < noFlowW && fabs(grid) < noFlowW {
		// nothing flowing: 0 = 0 + 0 proves nothing about the mapping
		return Check{Name: name, Status: StatusSkip, Detail: "no power flow"}
	}
	tol := max(balanceAbsTolW, relTol*max(fabs(load), fabs(inv), fabs(grid)))
	if d := load - (inv + grid); d > tol || d < -tol {
		return fail(name, "load %.0f W vs inverter %.0f W + grid %.0f W", load, inv, grid)
	}
	return pass(name)
}

// dcBalance: the PV power goes somewhere — PV ≈ inverter AC output +
// battery charging, within the conversion losses (DC-coupled battery:
// it charges from PV or, through the inverter, from the grid).
func dcBalance(v LiveValues) Check {
	const name = "dc_balance"
	if v.InverterW == nil || v.BatteryW == nil {
		return Check{Name: name, Status: StatusSkip, Detail: "inverter or battery power not mapped"}
	}
	pv := 0.0
	for _, s := range v.PV {
		if s.Present {
			pv += s.W
		}
	}
	inv, batt := *v.InverterW, *v.BatteryW
	tol := max(dcAbsTolW, estimatedBalanceRelTol*max(fabs(pv), fabs(inv), fabs(batt)))
	if d := pv - (inv + batt); d > tol || d < -tol {
		return fail(name, "PV %.0f W vs inverter %.0f W + battery %.0f W", pv, inv, batt)
	}
	return pass(name)
}

// dcAbsTolW: the fixed conversion losses dominate at low power — the
// reference KH10 lost 254 W discharging 1245 W at night (2026-11-16).
const dcAbsTolW = 400

// noFlowW: below this on load, inverter and grid alike, nothing flows.
const noFlowW = 20

// estimatedBalanceRelTol covers DC→AC conversion losses when the
// inverter output is estimated (~6 % at 4.7 kW on the reference KH10).
const estimatedBalanceRelTol = 0.15

// settingRange is one installer range (profile §5 "Installer ranges").
type settingRange struct {
	addr, lo, hi int
	name         string
}

var settingRanges = []settingRange{
	{41007, 0, 500, "max charge current (0.1 A)"},
	{41008, 0, 500, "max discharge current (0.1 A)"},
	{41009, 0, socMax, "min SoC"},
	{41010, installerSocMin, socMax, "max SoC from grid"},
	{41011, 0, socMax, "min SoC on grid"},
	{41012, installerSocMin, socMax, "max SoC"},
	{41016, 0, socMax, "power limit %"},
	{41032, 20, 200, "standby threshold W"},
}

// settingsRanges checks the BasicParameters2 values against the
// installer ranges.
func settingsRanges(r reader) Check {
	const name = "settings_ranges"
	for _, s := range settingRanges {
		v, ok := r.get(s.addr)
		if !ok {
			return pending(name)
		}
		if int(v) < s.lo || int(v) > s.hi {
			return fail(name, "%d (%s) = %d outside %d..%d", s.addr, s.name, v, s.lo, s.hi)
		}
	}
	return pass(name)
}

// workModeMax is the last schema option (SelfUse, Feedin, Backup, PeakShaving).
const workModeMax = 3

func workMode(r reader) Check {
	const name = "work_mode_enum"
	v, ok := r.get(regWorkMode)
	switch {
	case !ok:
		return pending(name)
	case int(v) > workModeMax:
		return fail(name, "%d = %d", regWorkMode, v)
	}
	return pass(name)
}

const limitMaxW = 30000

// u32Limit: a power limit is a U32 high word first, 0..30000 W.
func u32Limit(r reader, name string, addr int) Check {
	hi, ok1 := r.get(addr)
	lo, ok2 := r.get(addr + 1)
	switch {
	case !ok1 || !ok2:
		return pending(name)
	case hi != 0 || int(lo) > limitMaxW:
		return fail(name, "%d:%d = %d:%d (want 0:0..%d)", addr, addr+1, hi, lo, limitMaxW)
	}
	return pass(name)
}

// deciAmps: a current limit, 2..100 A in 0.1 A.
func deciAmps(r reader, name string, addr int) Check {
	v, ok := r.get(addr)
	switch {
	case !ok:
		return pending(name)
	case v < 20 || v > 1000:
		return fail(name, "%d = %d (want 20..1000, 0.1 A)", addr, v)
	}
	return pass(name)
}

// Installer-settings ranges (wire units; the installer schema).
const (
	maxBattDeciAmps = 500 // 50 A
	maxU16          = 0xFFFF
	epsModeMin      = 1 // 1 disabled (presumed), 2 EPS, 3 UPS
	epsModeMax      = 3
)

// inRange: a register read and within lo..hi.
func inRange(r reader, name string, addr, lo, hi int) Check {
	v, ok := r.get(addr)
	switch {
	case !ok:
		return pending(name)
	case int(v) < lo || int(v) > hi:
		return fail(name, "%d = %d (want %d..%d)", addr, v, lo, hi)
	}
	return pass(name)
}

const compLimit = 500

func meterComp(r reader) Check {
	const name = "meter_compensation_range"
	v, ok := r.get(regMeterComp)
	if !ok {
		return pending(name)
	}
	if s := int(int16(v)); s < -compLimit || s > compLimit { //nolint:gosec // G115: signed reinterpretation
		return fail(name, "%d = %d W", regMeterComp, s)
	}
	return pass(name)
}

// Scheduler table layout (profile §5).
const (
	schedGroups      = 8
	schedRegs        = 10
	schedDataVersion = 2 // +9 on every group of the reference device
	fdPwrMax         = 10500
	wmMax            = 7
)

// scheduleTable checks the 48000 master and the 48010 ×80 table shape.
func scheduleTable(r reader) Check {
	const name = "schedule_table"
	m, ok1 := r.get(regSchedMode)
	t, ok2 := r.block(regSchedTable, schedGroups*schedRegs)
	switch {
	case !ok1 || !ok2:
		return pending(name)
	case m > 1:
		return fail(name, "%d = %d", regSchedMode, m)
	}
	for k := range schedGroups {
		if what, v, bad := groupProblem(t[k*schedRegs : (k+1)*schedRegs]); bad {
			return fail(name, "group %d %s = %#x", k+1, what, v)
		}
	}
	return pass(name)
}

// Group register offsets.
const (
	gEn, gStart, gEnd, gWM, gSoc, gFdSoc, gFdPwr, gVersion = 0, 1, 2, 3, 4, 5, 6, 9
	byteShift                                              = 8
	byteMask                                               = 0xFF
)

// groupProblem names the first implausible field of one time group.
func groupProblem(g []uint16) (string, uint16, bool) {
	checks := []struct {
		what string
		v    uint16
		bad  bool
	}{
		{"enable", g[gEn], g[gEn] > 1},
		{"start", g[gStart], !hhmm(g[gStart])},
		{"end", g[gEnd], !hhmm(g[gEnd])},
		{"work mode", g[gWM], g[gWM] > wmMax || (g[gEn] == 1 && g[gWM] == 0)},
		{"max:min SoC", g[gSoc], g[gSoc]>>byteShift > socMax || g[gSoc]&byteMask > socMax},
		{"fd SoC", g[gFdSoc], g[gFdSoc] > socMax},
		{"fd power", g[gFdPwr], g[gFdPwr] > fdPwrMax},
		{"data version (+9)", g[gVersion], g[gVersion] != schedDataVersion},
	}
	for _, c := range checks {
		if c.bad {
			return c.what, c.v, true
		}
	}
	return "", 0, false
}

func hhmm(v uint16) bool { return v>>byteShift <= 23 && v&byteMask <= 59 }

func fabs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
