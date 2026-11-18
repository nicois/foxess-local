package compat

import (
	"sort"

	"foxesslocal/internal/fmdata"
	"foxesslocal/internal/frame"
	"foxesslocal/internal/identity"
)

// PVString is one PV input's measured values.
type PVString struct {
	V, A, W float64
	Present bool
}

// LiveValues are the live readings the physical checks need, in SI units
// with the API's sign conventions. A nil field was not available.
type LiveValues struct {
	PV          [4]PVString
	GridV       *float64
	GridHz      *float64
	SocPct      *float64
	LoadW       *float64
	InverterW   *float64 // AC output, + = out
	GridImportW *float64 // + = import
	BatteryW    *float64 // + = charging
}

// FromKH converts our own KH_PRE133 decode.
func FromKH(rt frame.KHRealtime) LiveValues {
	var v LiveValues
	for i, s := range rt.PV {
		v.PV[i] = PVString{V: float64(s.DeciV) / deci, A: float64(s.DeciA) / deci, W: float64(s.W), Present: true}
	}
	v.GridV = new(float64(rt.GridDeciV) / deci)
	v.GridHz = new(float64(rt.GridCentiHz) / centi)
	v.SocPct = new(float64(rt.SocPct))
	v.LoadW = new(float64(rt.LoadW))
	v.InverterW = new(float64(rt.InverterW))
	v.GridImportW = new(float64(rt.GridW))
	v.BatteryW = new(float64(rt.BatteryW))
	return v
}

const (
	kilo     = 1000.0
	u16Shift = 16
)

// mapValue reads one foxess_modbus entity: one register, or a 32-bit
// pair listed low word first; signed per the entity; scaled.
func mapValue(m fmdata.Map, key string, get func(int) (uint16, bool)) *float64 {
	e, ok := m.Entity(key)
	if !ok || len(e.Addresses) == 0 || len(e.Addresses) > 2 {
		return nil
	}
	signed := e.Signed == nil || *e.Signed
	var raw float64
	lo, ok := get(e.Addresses[0])
	if !ok {
		return nil
	}
	if len(e.Addresses) == 1 {
		raw = float64(lo)
		if signed {
			raw = float64(int16(lo)) //nolint:gosec // G115: signed reinterpretation
		}
	} else {
		hi, ok := get(e.Addresses[1])
		if !ok {
			return nil
		}
		u := uint32(hi)<<u16Shift | uint32(lo)
		raw = float64(u)
		if signed {
			raw = float64(int32(u)) //nolint:gosec // G115: signed reinterpretation
		}
	}
	if e.Scale != nil {
		raw *= *e.Scale
	}
	return &raw
}

// liveKeys are the foxess_modbus keys of each live value, in order of
// preference (single- and three-phase families name them differently).
var liveKeys = struct {
	gridV, freq, soc, load, inverter, battery []string
	gridCT, gridCTPhases                      []string
}{
	gridV:        []string{"rvolt", "grid_voltage_R", "grid_voltage_L1"},
	freq:         []string{"rfreq"},
	soc:          []string{"battery_soc", "battery_soc_1"},
	load:         []string{"load_power", "load_power_total"},
	inverter:     []string{"rpower"},
	battery:      []string{"invbatpower"},
	gridCT:       []string{"grid_ct"},
	gridCTPhases: []string{"grid_ct_R", "grid_ct_S", "grid_ct_T"},
}

var pvKeys = [4][3]string{
	{"pv1_voltage", "pv1_current", "pv1_power"},
	{"pv2_voltage", "pv2_current", "pv2_power"},
	{"pv3_voltage", "pv3_current", "pv3_power"},
	{"pv4_voltage", "pv4_current", "pv4_power"},
}

// firstKey is the first of keys the map has a register entity for.
func firstKey(m fmdata.Map, keys []string) (string, bool) {
	for _, k := range keys {
		if e, ok := m.Entity(k); ok && len(e.Addresses) > 0 {
			return k, true
		}
	}
	return "", false
}

// usedKeys are the keys FromMap reads from a map.
func usedKeys(m fmdata.Map) []string {
	var out []string
	for _, k := range pvKeys {
		out = append(out, k[:]...)
	}
	for _, ks := range [][]string{liveKeys.gridV, liveKeys.freq, liveKeys.soc, liveKeys.load, liveKeys.inverter, liveKeys.battery} {
		if k, ok := firstKey(m, ks); ok {
			out = append(out, k)
		}
	}
	if k, ok := firstKey(m, liveKeys.gridCT); ok {
		out = append(out, k)
	} else {
		out = append(out, liveKeys.gridCTPhases...)
	}
	return out
}

// LiveAddrs are the registers FromMap needs for a map.
func LiveAddrs(m fmdata.Map) []int {
	var out []int
	for _, k := range usedKeys(m) {
		if e, ok := m.Entity(k); ok {
			out = append(out, e.Addresses...)
		}
	}
	return out
}

// FromMap decodes live values with a foxess_modbus map (power sensors
// are kW there; grid_ct is + = export; invbatpower is + = discharge).
func FromMap(m fmdata.Map, get func(int) (uint16, bool)) LiveValues {
	var v LiveValues
	for i, k := range pvKeys {
		pv, pa, pw := mapValue(m, k[0], get), mapValue(m, k[1], get), mapValue(m, k[2], get)
		if pv != nil && pa != nil && pw != nil {
			v.PV[i] = PVString{V: *pv, A: *pa, W: *pw * kilo, Present: true}
		}
	}
	pick := func(keys []string) *float64 {
		if k, ok := firstKey(m, keys); ok {
			return mapValue(m, k, get)
		}
		return nil
	}
	kw := func(p *float64, sign float64) *float64 {
		if p == nil {
			return nil
		}
		return new(sign * *p * kilo)
	}
	v.GridV = pick(liveKeys.gridV)
	v.GridHz = pick(liveKeys.freq)
	v.SocPct = pick(liveKeys.soc)
	v.LoadW = kw(pick(liveKeys.load), 1)
	v.InverterW = kw(pick(liveKeys.inverter), 1)
	v.BatteryW = kw(pick(liveKeys.battery), -1)
	v.GridImportW = gridImportW(m, get)
	return v
}

// gridImportW is the grid import in W (+ = import): the map's grid CT
// (+ = export there) or, without one, the sum of all three phase CTs.
func gridImportW(m fmdata.Map, get func(int) (uint16, bool)) *float64 {
	if k, ok := firstKey(m, liveKeys.gridCT); ok {
		if p := mapValue(m, k, get); p != nil {
			return new(-*p * kilo)
		}
		return nil
	}
	var sum float64
	for _, k := range liveKeys.gridCTPhases {
		p := mapValue(m, k, get)
		if p == nil {
			return nil
		}
		sum += *p
	}
	return new(-sum * kilo)
}

// LiveChecks are the physical-consistency checks over live values.
func LiveChecks(v LiveValues, snapSoc *int) []Check {
	return []Check{
		pvProduct(v),
		rangeF("grid_voltage", v.GridV, gridMinV, gridMaxV, "%.1f V"),
		rangeF("grid_frequency", v.GridHz, gridMinHz, gridMaxHz, "%.2f Hz"),
		rangeF("soc_range", v.SocPct, 0, socMax, "%.0f %%"),
		socAgrees(v.SocPct, snapSoc),
		powerBalance(v),
		dcBalance(v),
	}
}

// AnyFailed reports whether any check failed.
func AnyFailed(cs []Check) bool {
	for _, c := range cs {
		if c.Status == StatusFail {
			return true
		}
	}
	return false
}

// Classifiable is the bar a candidate map must clear to become a
// device's layout: no check fails, grid voltage / frequency / SoC and a
// real power balance pass, and the PV product passes — or is skipped
// because no string is active (so a device can be classified at night;
// the power balance still needs real flow).
func Classifiable(cs []Check) bool {
	status := map[string]string{}
	for _, c := range cs {
		status[c.Name] = c.Status
	}
	if AnyFailed(cs) {
		return false
	}
	for _, n := range []string{"grid_voltage", "grid_frequency", "soc_range", "power_balance"} {
		if status[n] != StatusPass {
			return false
		}
	}
	pv := status["pv_power_product"]
	return pv == StatusPass || pv == StatusSkip
}

// Divergence is a register both sources define with different meanings.
type Divergence struct {
	Addr   int    `json:"addr"`
	Ours   string `json:"ours"`
	Theirs string `json:"theirs"`
}

// Divergences compares a verified profile's register meanings with a
// foxess_modbus map: every single-register entity at an address we have
// proven, whose key names something else.
func Divergences(p identity.Profile, m fmdata.Map) []Divergence {
	var out []Divergence
	seen := map[int]bool{}
	for _, e := range m {
		if len(e.Addresses) != 1 || seen[e.Addresses[0]] {
			continue
		}
		ours, ok := p.Registers[e.Addresses[0]]
		if !ok {
			continue
		}
		seen[e.Addresses[0]] = true
		if e.Key != ours {
			out = append(out, Divergence{Addr: e.Addresses[0], Ours: ours, Theirs: e.Key})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Addr < out[j].Addr })
	return out
}
