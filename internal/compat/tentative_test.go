package compat

import (
	"math"
	"testing"

	"foxesslocal/internal/fmdata"
)

// encode writes physical values into registers through a foxess_modbus
// map's own addresses and scales (the inverse of mapValue): a synthetic
// device of that layout.
func encode(t *testing.T, r regs, m fmdata.Map, vals map[string]float64) {
	_ = t
	for key, v := range vals {
		e, ok := m.Entity(key)
		if !ok || len(e.Addresses) == 0 {
			continue // a key this layout does not have (e.g. PV3/PV4)
		}
		if e.Scale != nil {
			v /= *e.Scale
		}
		raw := int64(math.Round(v))
		if len(e.Addresses) == 1 {
			r[e.Addresses[0]] = uint16(raw) //nolint:gosec // G115: test encoding
			continue
		}
		u := uint32(raw)              //nolint:gosec // G115: two's complement encoding
		r[e.Addresses[0]] = uint16(u) //nolint:gosec // G115: low word
		r[e.Addresses[1]] = uint16(u >> 16)
	}
}

// physics is a consistent operating point: 3.0 kW PV on two strings,
// battery charging 1.0 kW, house 1.5 kW, exporting 0.4 kW (inverter
// output 1.9 kW ≈ load + export; losses 100 W).
func physics() map[string]float64 {
	return map[string]float64{
		"pv1_voltage": 300, "pv1_current": 5, "pv1_power": 1.5,
		"pv2_voltage": 250, "pv2_current": 6, "pv2_power": 1.5,
		"pv3_voltage": 0, "pv3_current": 0, "pv3_power": 0,
		"pv4_voltage": 0, "pv4_current": 0, "pv4_power": 0,
		"rvolt": 240.5, "rfreq": 50.01, "battery_soc": 64,
		"load_power": 1.5, "rpower": 1.9, "grid_ct": 0.4, "invbatpower": -1.0,
	}
}

func modelRegs(r regs, model string) {
	for i := range 15 {
		r[30000+i] = 0x2020
	}
	for i := 0; i < len(model); i += 2 {
		hi := uint16(model[i])
		lo := uint16(' ')
		if i+1 < len(model) {
			lo = uint16(model[i+1])
		}
		r[30000+i/2] = hi<<8 | lo
	}
}

// TestTentativeH1: an H1-G2 (no verified profile, no KH layout) is
// classified by testing its foxess_modbus maps against the live
// registers: the first map that passes the physical checks is the
// layout; live values are tentative; settings it has no mapping for are
// unknown (not mapped), never decoded with the KH layout.
func TestTentativeH1(t *testing.T) {
	m, _ := fmdata.Load().Map("H1_G2_PRE144-HOLDING")
	r := regs{}
	modelRegs(r, "H1-5.0-E-G2")
	encode(t, r, m, physics())
	snap := 64
	rep := Evaluate(Input{Model: "H1-5.0-E-G2", Firmware: refFW, Regs: r.get, SnapshotSoc: &snap})
	if rep.Layout != "H1_G2_PRE144-HOLDING" {
		t.Fatalf("layout %q (family %q)", rep.Layout, rep.Family)
	}
	if f := level(t, rep, FeatureLiveValues); f.Level != LevelTentative {
		t.Fatalf("live values: %s %s %+v", f.Level, f.Reason, f.Checks)
	}
	if f := level(t, rep, FeaturePowerLimits); f.Level != LevelUnknown {
		t.Fatalf("power limits on H1: %s, want unknown (not mapped)", f.Level)
	}
	if rep.ThirdParty == nil || rep.ThirdParty.Map != "H1_G2_PRE144-HOLDING" {
		t.Fatalf("third party: %+v", rep.ThirdParty)
	}
}

// TestTentativeKH133: a KH whose 30016 version block does not answer
// (the later KH layout) is not the verified layout; the KH_133 map is
// tried and accepted.
func TestTentativeKH133(t *testing.T) {
	m, _ := fmdata.Load().Map("KH_133-HOLDING")
	r := regs{}
	modelRegs(r, refModel)
	encode(t, r, m, physics())
	rep := Evaluate(Input{Model: refModel, Firmware: refFW, Regs: r.get})
	if rep.Layout != "KH_133-HOLDING" {
		t.Fatalf("layout %q (family %q)", rep.Layout, rep.Family)
	}
	if f := level(t, rep, FeatureLiveValues); f.Level != LevelTentative {
		t.Fatalf("live values: %s %s", f.Level, f.Reason)
	}
}

// TestTentativeNotReadYet: a recognised family whose live registers
// have not been read is pending, not unknown.
func TestTentativeNotReadYet(t *testing.T) {
	r := regs{}
	modelRegs(r, "H1-5.0-E-G2")
	rep := Evaluate(Input{Model: "H1-5.0-E-G2", Regs: r.get})
	if f := level(t, rep, FeatureLiveValues); f.Level != LevelPending {
		t.Fatalf("H1 before any live read: %s %s", f.Level, f.Reason)
	}
}
