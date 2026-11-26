package frame

import "testing"

// probe1452 is the live KH10 (fw 1.73, KH_PRE133 map) answer of
// 2026-11-16 14:52:57 AEDT — sunny afternoon, battery full
// (docs/recon/protocol-profile.md §5 live-value map).
var (
	probe1452A = []uint16{ // holding 31000..31028
		1261, 158, 2003, 2958, 92, 2723, 2426, 181, 4398, 4999,
		2418, 5, 0, 5000, 65, 0, 0, 0, 418, 446,
		4221, 0, 65508, 276, 100, 0, 500, 3, 1,
	}
	probe1452B = []uint16{ // holding 31039..31054
		13, 0, 0, 11, 0, 0, 0, 4398, 0, 0, 65535, 65460, 0, 0, 0, 4326,
	}
)

func TestParseKHRealtimeProbe(t *testing.T) {
	r, err := ParseKHRealtime(probe1452A, probe1452B)
	if err != nil {
		t.Fatal(err)
	}
	if r.PV[0].W != 2003 || r.PV[1].W != 2723 || r.PV[2].W != 0 || r.PV[3].W != 0 {
		t.Errorf("PV W: %+v", r.PV)
	}
	if r.PV[0].DeciV != 1261 || r.PV[0].DeciA != 158 || r.PV[1].DeciV != 2958 || r.PV[1].DeciA != 92 {
		t.Errorf("PV V/A: %+v", r.PV)
	}
	checks := []struct {
		name      string
		got, want int
	}{
		{"pv total", int(r.PVW()), 4726},
		{"inverter W", int(r.InverterW), 4398},
		{"load W", int(r.LoadW), 4326},
		{"grid W (+import)", int(r.GridW), -76},
		{"battery W (+charge)", int(r.BatteryW), 28},
		{"soc", r.SocPct, 100},
		{"grid deci-V", r.GridDeciV, 2426},
		{"grid centi-Hz", r.GridCentiHz, 4999},
		{"battery deci-C", r.BatteryDeciC, 276},
		{"inverter deci-C", r.InverterDeciC, 418},
		{"ambient deci-C", r.AmbientDeciC, 446},
		{"bms charge deci-A", r.BMSChargeDeciA, 0},
		{"bms discharge deci-A", r.BMSDischargeDeciA, 500},
		{"state", r.State, 3},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, c.got, c.want)
		}
	}
	if !r.BMSConnected {
		t.Error("BMS connected: got false")
	}
}

// TestKHRealtimeTele: the poll maps onto the 150 B Tele shape (same
// store row, same /state, same SSE) with the API sign conventions.
func TestKHRealtimeTele(t *testing.T) {
	r, err := ParseKHRealtime(probe1452A, probe1452B)
	if err != nil {
		t.Fatal(err)
	}
	tl := r.Tele()
	if tl.Sol != 4726 || tl.Grid != -76 || tl.Haus != 4326 || tl.Acport != 4398 || tl.Batt != 28 || tl.Soc != 100 {
		t.Fatalf("tele: %+v", tl)
	}
	// Measured values, not the cloud model: the identity deltas carry the
	// conversion losses instead of ~0.
	if tl.IDGrid != -76-(4326+28-4726) || tl.IDAcport != 4398-(4726-28) {
		t.Errorf("identity deltas: grid %d acport %d", tl.IDGrid, tl.IDAcport)
	}
}

func TestParseKHRealtimeRejectsShortBlocks(t *testing.T) {
	if _, err := ParseKHRealtime(probe1452A[:28], probe1452B); err == nil {
		t.Error("short block A accepted")
	}
	if _, err := ParseKHRealtime(probe1452A, probe1452B[:15]); err == nil {
		t.Error("short block B accepted")
	}
}

// TestKHRealtimeEPS: the EPS (backup) port power is 31047:31048 (s32,
// + = output) and reaches the telemetry shape.
func TestKHRealtimeEPS(t *testing.T) {
	b := append([]uint16(nil), probe1452B...)
	b[31047-KHRealtimeBAddr], b[31048-KHRealtimeBAddr] = 0, 412
	r, err := ParseKHRealtime(probe1452A, b)
	if err != nil {
		t.Fatal(err)
	}
	if r.EpsW != 412 || r.Tele().Eps != 412 {
		t.Fatalf("eps: %d / tele %d, want 412", r.EpsW, r.Tele().Eps)
	}
	if r0, _ := ParseKHRealtime(probe1452A, probe1452B); r0.EpsW != 0 {
		t.Fatalf("probe eps %d, want 0 (nothing on the port)", r0.EpsW)
	}
}

// TestKHRealtimePVSigned: the PV power registers are signed (foxess_modbus
// marks pv1_power signed). At dawn and dusk a string reads a few watts
// negative; read unsigned, −1 W became 65 535 W — the 27–42 kW night-time
// "solar" spikes of 2026-11-17 05:15 and 20:25.
func TestKHRealtimePVSigned(t *testing.T) {
	a := append([]uint16(nil), probe1452A...)
	a[khPV1W], a[khPV2W] = 0xFFFF, 0xFFFE // −1 W, −2 W
	r, err := ParseKHRealtime(a, probe1452B)
	if err != nil {
		t.Fatal(err)
	}
	if r.PV[0].W != -1 || r.PV[1].W != -2 || r.PVW() != -3 {
		t.Fatalf("PV W %d / %d, total %d; want -1 / -2, -3", r.PV[0].W, r.PV[1].W, r.PVW())
	}
	// reported solar: standby draw is not generation
	if tl := r.Tele(); tl.Sol != 0 {
		t.Fatalf("Tele sol %d, want 0", tl.Sol)
	}
}

// TestKHRealtimeMeter2 (issue #2): 31051:31052 is the second CT (meter 2),
// s32 hi:lo, − = power into the site. A KH10 with AC-coupled solar
// (fw 1.60 and 1.73, the reporter's bundles) read 0xFFFF:0xFAD1 =
// −1327 W while the cloud showed "Gen Load" 1330 W; the household load
// the inverter reports (31053:31054) counts it. It reaches the telemetry
// shape; the 150 B frame has no such field (nil).
func TestKHRealtimeMeter2(t *testing.T) {
	b := append([]uint16(nil), probe1452B...)
	b[31051-KHRealtimeBAddr], b[31052-KHRealtimeBAddr] = 0xFFFF, 0xFAD1
	r, err := ParseKHRealtime(probe1452A, b)
	if err != nil {
		t.Fatal(err)
	}
	if r.Meter2W != -1327 {
		t.Fatalf("meter 2: %d, want -1327", r.Meter2W)
	}
	if m := r.Tele().Meter2; m == nil || *m != -1327 {
		t.Fatalf("tele meter 2: %v, want -1327", m)
	}
	if r0, _ := ParseKHRealtime(probe1452A, probe1452B); r0.Meter2W != 0 {
		t.Fatalf("probe meter 2 %d, want 0 (no second CT)", r0.Meter2W)
	}
}
