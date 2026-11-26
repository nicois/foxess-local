package compat

import (
	"testing"

	"foxesslocal/internal/identity"
)

// refModel / refFW are the verified profile's model and firmware.
const refModel = "KH10"

// refFW is the verified profile's firmware.
var refFW = identity.Firmware{Master: "1.73", Slave: "1.03", Manager: "1.71"}

// regs is a register map test source.
type regs map[int]uint16

func (r regs) get(addr int) (uint16, bool) { v, ok := r[addr]; return v, ok }

// reference returns a register set shaped like the live KH10 on
// 2026-11-16: the probe's live values, the 41007 settings block, the
// scheduler master and table, the identity blocks.
func reference() regs {
	r := regs{}
	// identity: "KH10" one char per register, space-padded; fw 1.73/1.03/1.71
	model := "KH10           "
	for i := range len(model) {
		r[30000+i] = uint16(model[i])
	}
	r[30016], r[30017], r[30018] = 0x173, 0x103, 0x171
	// live block A (31000 ×29): PV1 126.1 V 15.8 A 2003 W (V×A = 1992),
	// PV2 off, grid 241.0 V 50.00 Hz, battery 0 W, SoC 90 %
	a := make([]uint16, 29)
	a[0], a[1], a[2] = 1261, 158, 2003
	a[6], a[9] = 2410, 5000
	a[24] = 90
	for i, v := range a {
		r[31000+i] = v
	}
	// live block B (31039 ×16): inverter 1950 W out, grid −1000 W
	// (export), load 950 W — load ≈ inverter + grid
	b := make([]uint16, 16)
	b[7] = 1950
	b[10], b[11] = 0xFFFF, uint16(0x10000-1000)
	b[15] = 950
	for i, v := range b {
		r[31039+i] = v
	}
	// settings block 41007 ×40 (the live read)
	set := map[int]uint16{
		41007: 450, 41008: 450, 41009: 0, 41010: 100, 41011: 11, 41012: 100,
		41013: 1, 41014: 1, 41015: 3, 41016: 100, 41017: 158, 41019: 2300, 41020: 1,
		41021: 0, 41022: 12500, 41023: 0, 41024: 5000, 41025: 560, 41027: 560,
		41028: 400, 41029: 0xFFCD, 41032: 100,
	}
	for i := range 40 {
		r[41007+i] = set[41007+i]
	}
	r[41000] = 1
	// the clock 40000 ×6: 2026-11-19 16:17:26
	for i, v := range []uint16{2026, 11, 19, 16, 17, 26} {
		r[40000+i] = v
	}
	// grid-port limits 41200 ×2 (the 2026-11-17 values)
	r[41200], r[41201] = 10000, 7000
	// scheduler: master on; group 1 force charge 11:02-13:58, groups 2-8
	// in the cloud's unused shape; +9 = 2 everywhere
	for i := range 10 {
		r[48000+i] = 0
	}
	r[48000] = 1
	for k := range 8 {
		g := []uint16{0, 0, 0, 1, 0x640A, 10, 0, 0, 0, 2}
		if k == 0 {
			g = []uint16{1, 0x0B02, 0x0D3A, 6, 0x640A, 80, 9948, 0, 1, 2}
		}
		for i, v := range g {
			r[48010+10*k+i] = v
		}
	}
	return r
}

func ref() Input {
	snap := 90
	return Input{Model: refModel, Firmware: refFW, Regs: reference().get, SnapshotSoc: &snap}
}

func level(t *testing.T, rep Report, feature string) Feature {
	t.Helper()
	for _, f := range rep.Features {
		if f.Name == feature {
			return f
		}
	}
	t.Fatalf("feature %q missing from %+v", feature, rep)
	return Feature{}
}

// TestReferenceIsExact: the verified device with plausible data is exact
// for every feature.
func TestReferenceIsExact(t *testing.T) {
	rep := Evaluate(ref())
	if len(rep.Features) != len(Features) {
		t.Fatalf("%d features, want %d", len(rep.Features), len(Features))
	}
	for _, f := range rep.Features {
		if f.Level != LevelExact {
			t.Errorf("%s: %s (%s), want exact; checks %+v", f.Name, f.Level, f.Reason, f.Checks)
		}
	}
}

// TestFirmwareBumpIsCompatible: same model and generation, other firmware,
// checks pass → compatible (a trivial variation does not lock anything
// out).
func TestFirmwareBumpIsCompatible(t *testing.T) {
	in := ref()
	in.Firmware.Master = "1.74"
	for _, f := range Evaluate(in).Features {
		if f.Level != LevelCompatible {
			t.Errorf("%s: %s (%s), want compatible", f.Name, f.Level, f.Reason)
		}
	}
}

// TestOtherFamilyIsUnknown: a model outside the known family is unknown
// even when its numbers look plausible (no mapping is trusted yet).
func TestOtherFamilyIsUnknown(t *testing.T) {
	in := ref()
	in.Model = "H1-5.0-E"
	if f := level(t, Evaluate(in), FeatureLiveValues); f.Level != LevelUnknown {
		t.Fatalf("H1 live values: %s, want unknown", f.Level)
	}
}

// TestFailedCheckIsUnknown: each kind of implausible data drops only the
// feature that depends on it (per-feature grading), with the failing
// check named.
func TestFailedCheckIsUnknown(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(r regs)
		feature string
		check   string
	}{
		{"PV V×A ≠ W", func(r regs) { r[31002] = 3500 }, FeatureLiveValues, "pv_power_product"},
		{"grid voltage", func(r regs) { r[31006] = 900 }, FeatureLiveValues, "grid_voltage"},
		{"grid frequency", func(r regs) { r[31009] = 7000 }, FeatureLiveValues, "grid_frequency"},
		{"SoC over 100", func(r regs) { r[31024] = 140 }, FeatureLiveValues, "soc_range"},
		{"power balance", func(r regs) { r[31054] = 9000 }, FeatureLiveValues, CheckPowerBalance},
		{"limit high word", func(r regs) { r[41023] = 1 }, FeaturePowerLimits, "export_limit_range"},
		{"current limit", func(r regs) { r[41028] = 5000 }, FeatureCurrentLimits, "export_current_range"},
		{"meter compensation", func(r regs) { r[41029] = 0x1000 }, FeatureMeterCompensation, "meter_compensation_range"},
		{"work mode", func(r regs) { r[41000] = 9 }, FeatureWorkMode, "work_mode_enum"},
		{"schedule HHMM", func(r regs) { r[48011] = 0x1A00 }, FeatureSchedule, "schedule_table"},
		{"schedule data version", func(r regs) { r[48019] = 3 }, FeatureSchedule, "schedule_table"},
		{"max SoC range", func(r regs) { r[41012] = 5 }, FeatureSettingsRead, "settings_ranges"},
		{"charge current", func(r regs) { r[41007] = 600 }, FeatureBatteryCurrents, "max_charge_current_range"},
		{"power limit", func(r regs) { r[41016] = 150 }, FeaturePowerLimit, "power_limit_range"},
		{"EPS mode", func(r regs) { r[41015] = 7 }, FeatureEPSMode, "eps_mode_enum"},
		{"clock month", func(r regs) { r[40001] = 13 }, FeatureClock, checkClockPlausible},
		{"clock year", func(r regs) { r[40000] = 1970 }, FeatureClock, checkClockPlausible},
		{"clock 31 Nov", func(r regs) { r[40002] = 31 }, FeatureClock, checkClockPlausible},
	}
	for _, c := range cases {
		in := ref()
		r := reference()
		c.mutate(r)
		in.Regs = r.get
		rep := Evaluate(in)
		f := level(t, rep, c.feature)
		if f.Level != LevelUnknown || !failed(f, c.check) {
			t.Errorf("%s: %s %s, want unknown with %s failing; checks %+v", c.name, c.feature, f.Level, c.check, f.Checks)
		}
		// another feature is unaffected
		other := FeatureWorkMode
		if c.feature == FeatureWorkMode {
			other = FeatureSchedule
		}
		if g := level(t, rep, other); g.Level != LevelExact {
			t.Errorf("%s: unrelated %s dropped to %s", c.name, other, g.Level)
		}
	}
}

// TestSnapshotSocDisagreement: the polled SoC must agree with the
// inverter's own snapshot SoC (±5).
func TestSnapshotSocDisagreement(t *testing.T) {
	in := ref()
	far := 60
	in.SnapshotSoc = &far
	if f := level(t, Evaluate(in), FeatureLiveValues); f.Level != LevelUnknown || !failed(f, "soc_agrees_snapshot") {
		t.Fatalf("SoC 90 vs snapshot 60: %s %+v", f.Level, f.Checks)
	}
}

// TestNoDataIsPending: before the first reads, a feature is pending, not
// unknown (nothing is wrong yet).
func TestNoDataIsPending(t *testing.T) {
	in := Input{Model: refModel, Firmware: refFW, Regs: regs{}.get}
	for _, f := range Evaluate(in).Features {
		if f.Level != LevelPending {
			t.Errorf("%s: %s, want pending", f.Name, f.Level)
		}
	}
}

// TestIdlePVSkipsProduct: a string producing under the threshold (night,
// unused input) is not held to V×A = W.
func TestIdlePVSkipsProduct(t *testing.T) {
	in := ref()
	r := reference()
	r[31003], r[31004], r[31005] = 50, 3, 0 // 5.0 V 0.3 A 0 W: noise
	in.Regs = r.get
	if f := level(t, Evaluate(in), FeatureLiveValues); f.Level != LevelExact {
		t.Fatalf("idle string: %s %+v", f.Level, f.Checks)
	}
}

// checkClockPlausible is the clock feature's check.
const checkClockPlausible = "clock_plausible"

func failed(f Feature, check string) bool {
	for _, c := range f.Checks {
		if c.Name == check && c.Status == StatusFail {
			return true
		}
	}
	return false
}

// TestPortLimitsPending: the grid-port limits are graded only once 41200
// has been read (no reading = pending, never exact).
func TestPortLimitsPending(t *testing.T) {
	in := ref()
	r := reference()
	delete(r, 41200)
	in.Regs = r.get
	if f := level(t, Evaluate(in), FeaturePortLimits); f.Level != LevelPending {
		t.Fatalf("port limits without 41200: %s", f.Level)
	}
}

// TestPowerBalanceMeter2 (issue #2): with AC-coupled solar on the second
// CT (31051:31052), the inverter's household load counts it — load ≈
// inverter + grid − meter 2. The reporter's KH10 (fw 1.60): inverter
// −904 W (charging), grid −139 W, meter 2 −1327 W, load 354 W. Without
// the meter-2 term the balance is 1.4 kW out and live values would drop.
func TestPowerBalanceMeter2(t *testing.T) {
	in := ref()
	r := reference()
	s32 := func(lo int, v int32) { u := uint32(v); r[lo-1], r[lo] = uint16(u>>16), uint16(u) } //nolint:gosec // G115: test pair
	s32(31046, -904)
	s32(31050, -139)
	s32(31052, -1327)
	s32(31054, 354)
	r[31022] = uint16(0x10000 - 769) // battery 769 W charging (+ = discharge on the wire)
	r[31002] = 0                     // no DC solar
	in.Regs = r.get
	f := level(t, Evaluate(in), FeatureLiveValues)
	for _, c := range f.Checks {
		if c.Name == CheckPowerBalance && c.Status != StatusPass {
			t.Fatalf("power_balance %s (%s), want pass", c.Status, c.Detail)
		}
	}
}
