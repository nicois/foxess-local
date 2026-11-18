// Package driver is the per-layout abstraction (design D-020): for each
// inverter register layout we at least partially support, what the app
// can write and how sure it is of each encoding. Selection is by the
// layout the compatibility evaluation established from the registers
// that answer — never by a model name alone.
package driver

import "foxesslocal/internal/fmdata"

// Support is a write feature's status on a layout.
type Support string

// Write support levels. Unmapped and NotSupported are both
// "impossible", for different reasons: the first is our ignorance (no
// known register / encoding), the second the device's (the hardware or
// firmware lacks the feature).
const (
	Verified     Support = "verified"      // encoding proven on a verified profile
	Untested     Support = "untested"      // encoding known (e.g. a third-party map), unproven
	Unmapped     Support = "unmapped"      // impossible: no known register / encoding
	NotSupported Support = "not_supported" // impossible: the device lacks the feature
)

// Layout names (compat.Report.Layout).
const (
	LayoutKHPre133 = "KH_PRE133"
)

// Driver is one supported layout.
type Driver interface {
	Name() string
	Layout() string
	// WriteSupport is the status of one write feature (compat feature
	// names: work_mode, power_limits, meter_compensation, schedule, …).
	WriteSupport(feature string) Support
}

// table is a Driver defined by data.
type table struct {
	name, layout string
	writes       map[string]Support
}

func (t table) Name() string   { return t.name }
func (t table) Layout() string { return t.layout }

func (t table) WriteSupport(feature string) Support {
	if s, ok := t.writes[feature]; ok {
		return s
	}
	return Unmapped
}

// drivers is the registry.
var drivers = []Driver{
	table{
		name: "kh-pre133", layout: LayoutKHPre133,
		writes: map[string]Support{
			// the gateway's encodings, proven by captured cloud writes on
			// the KH10 fw 1.73 profile (protocol profile §5)
			"work_mode":          Verified,
			"power_limits":       Verified,
			"meter_compensation": Verified,
			"schedule":           Verified,
			// single-register fn06 writes, each captured from the cloud
			// with a verbatim echo (2026-11-16 write-through session)
			"current_limits":   Verified,
			"battery_currents": Verified,
			"power_limit":      Verified,
			"port_limits":      Verified,
			"eps_mode":         Verified,
			// fn06 41037 → Modbus exception 2 on fw 1.73 (2026-11-16)
			"eps_recover_soc": NotSupported,
		},
	},
}

// Select returns the driver for a layout: a hand-written one, else one
// derived from the foxess_modbus map of that name (tentative, D-021).
func Select(layout string) (Driver, bool) {
	if layout == "" {
		return nil, false
	}
	for _, d := range drivers {
		if d.Layout() == layout {
			return d, true
		}
	}
	m, ok := fmdata.Load().Map(layout)
	if !ok {
		return nil, false
	}
	writes := map[string]Support{}
	if workModeFits(m) {
		writes["work_mode"] = Untested
	}
	return table{name: "foxess_modbus:" + layout, layout: layout, writes: writes}, true
}

// Our work-mode encoding: fn06 @41000, 0 self-use / 1 feed-in.
const (
	workModeAddr = 41000
	selfUseLabel = "Self Use"
	feedInLabel  = "Feed-in First"
)

// workModeFits reports whether a map's work-mode select is the register
// and values the gateway writes.
func workModeFits(m fmdata.Map) bool {
	e, ok := m.Entity("work_mode")
	if !ok || e.Type != "select" {
		for _, x := range m {
			if x.Key == "work_mode" && x.Type == "select" {
				e, ok = x, true
			}
		}
	}
	return ok && len(e.Addresses) == 1 && e.Addresses[0] == workModeAddr &&
		e.Values["0"] == selfUseLabel && e.Values["1"] == feedInLabel
}
