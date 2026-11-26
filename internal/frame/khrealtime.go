package frame

import "fmt"

// KH live-value register map — KH10 firmware 1.73 answers the
// "KH_PRE133" layout of the open-source foxess_modbus integration
// (version registers 30016–30018 = 0x173/0x103/0x171; the 11000-series
// input and 36001/37xxx/39xxx holding blocks answer Modbus exception 2).
// Verified live 2026-11-16 (profile §5): PV V×A = W per string, and
// load ≈ inverter AC output + grid. Two holding-register blocks.
const (
	KHRealtimeAAddr  = 31000 // 31000..31028
	KHRealtimeACount = 29
	KHRealtimeBAddr  = 31039 // 31039..31054
	KHRealtimeBCount = 16
)

// Block-A offsets (31000 + n).
const (
	khPV1V, khPV1A, khPV1W = 0, 1, 2
	khPV2V, khPV2A, khPV2W = 3, 4, 5
	khGridV                = 6
	khGridHz               = 9
	khInvTemp, khAmbTemp   = 18, 19
	khBattW                = 22 // int16 W, + = discharge (inverted below)
	khBattTemp             = 23
	khSoc                  = 24
	khBMSChargeA           = 25
	khBMSDischargeA        = 26
	khState                = 27
	khBMSConnected         = 28
)

// Block-B offsets (31039 + n); 32-bit values are high word first.
const (
	khPV3V, khPV3A, khPV3W = 0, 1, 2
	khPV4V, khPV4A, khPV4W = 3, 4, 5
	khInvWHi               = 6  // 31045/31046: inverter AC power, + = output
	khEpsWHi               = 8  // 31047/31048: EPS (backup) port power, + = output
	khGridWHi              = 10 // 31049/31050: grid CT, + = import
	khMeter2WHi            = 12 // 31051/31052: second CT (meter 2), − = into the site
	khLoadWHi              = 14 // 31053/31054: household load
)

// KHString is one PV string: raw deci-volts, deci-amps, and watts.
type KHString struct {
	DeciV, DeciA int
	W            int32
}

// KHRealtime is one poll of the KH live-value registers, with the API's
// sign conventions applied (grid + = import, battery + = charging).
// Fixed-point fields keep their register units (deci-/centi-).
type KHRealtime struct {
	PV        [4]KHString
	InverterW int32 // AC port, + = output
	EpsW      int32 // EPS (backup / UPS) port, + = output
	GridW     int32 // + = import, − = export
	LoadW     int32
	// Meter2W is the second CT (meter 2, the cloud's "Gen Load"), − =
	// power into the site (AC-coupled solar, a generator). The household
	// load (LoadW) already counts it (issue #2).
	Meter2W           int32
	BatteryW          int32 // + = charging, − = discharging
	SocPct            int
	GridDeciV         int
	GridCentiHz       int
	BatteryDeciC      int
	InverterDeciC     int
	AmbientDeciC      int
	BMSChargeDeciA    int // BMS charge-current limit
	BMSDischargeDeciA int // BMS discharge-current limit
	State             int // inverter state enum (meaning open; 3 observed on-grid)
	BMSConnected      bool
}

// ParseKHRealtime decodes the two poll blocks (KHRealtimeA*/B*).
func ParseKHRealtime(a, b []uint16) (KHRealtime, error) {
	if len(a) != KHRealtimeACount || len(b) != KHRealtimeBCount {
		return KHRealtime{}, fmt.Errorf("frame: ParseKHRealtime: want %d+%d registers, got %d+%d",
			KHRealtimeACount, KHRealtimeBCount, len(a), len(b))
	}
	s16 := func(v uint16) int { return int(int16(v)) }                             //nolint:gosec // G115: signed register reinterpretation
	s32 := func(hi, lo uint16) int32 { return int32(uint32(hi)<<16 | uint32(lo)) } //nolint:gosec // G115: signed register pair
	pv := func(regs []uint16, v, i, w int) KHString {
		watts := int32(s16(regs[w])) //nolint:gosec // G115: int16 range; the PV power registers are signed (foxess_modbus pv1_power)
		return KHString{DeciV: s16(regs[v]), DeciA: s16(regs[i]), W: watts}
	}
	return KHRealtime{
		PV: [4]KHString{
			pv(a, khPV1V, khPV1A, khPV1W), pv(a, khPV2V, khPV2A, khPV2W),
			pv(b, khPV3V, khPV3A, khPV3W), pv(b, khPV4V, khPV4A, khPV4W),
		},
		InverterW:         s32(b[khInvWHi], b[khInvWHi+1]),
		EpsW:              s32(b[khEpsWHi], b[khEpsWHi+1]),
		GridW:             s32(b[khGridWHi], b[khGridWHi+1]),
		LoadW:             s32(b[khLoadWHi], b[khLoadWHi+1]),
		Meter2W:           s32(b[khMeter2WHi], b[khMeter2WHi+1]),
		BatteryW:          -int32(s16(a[khBattW])), //nolint:gosec // G115: int16 range
		SocPct:            int(a[khSoc]),
		GridDeciV:         int(a[khGridV]),
		GridCentiHz:       int(a[khGridHz]),
		BatteryDeciC:      s16(a[khBattTemp]),
		InverterDeciC:     s16(a[khInvTemp]),
		AmbientDeciC:      s16(a[khAmbTemp]),
		BMSChargeDeciA:    int(a[khBMSChargeA]),
		BMSDischargeDeciA: int(a[khBMSDischargeA]),
		State:             int(a[khState]),
		BMSConnected:      a[khBMSConnected] != 0,
	}, nil
}

// PVW is the total PV generation (sum of the four strings).
func (r KHRealtime) PVW() int32 {
	var sum int32
	for _, s := range r.PV {
		sum += s.W
	}
	return sum
}

// Tele maps a poll onto the 150 B telemetry shape — the same store row,
// /state fields and SSE event as the cloud-era 5 s frame. These are
// measured values, so the identity deltas carry the conversion losses
// (~300 W at 4.7 kW PV) instead of the cloud model's ~0.
func (r KHRealtime) Tele() Tele {
	// a negative total is the strings' standby draw at dawn and dusk (a
	// few W), not generation: solar is reported as 0 then (the raw
	// per-string values stay in PV for diagnostics)
	sol := max(r.PVW(), 0)
	soc := r.SocPct
	st := r.State
	if soc > 100 || soc < 0 {
		soc = 0
	}
	m2 := r.Meter2W
	return Tele{
		Meter2: &m2,
		Sol:    sol, Grid: r.GridW, Haus: r.LoadW, Acport: r.InverterW, Batt: r.BatteryW, Eps: r.EpsW,
		InvState: &st,
		Temps: &Temps{
			BattDeciC: r.BatteryDeciC, InvDeciC: r.InverterDeciC, AmbDeciC: r.AmbientDeciC,
			BMSChargeDeciA: r.BMSChargeDeciA,
		},
		Soc:      byte(soc),
		IDGrid:   int(r.GridW - (r.LoadW + r.BatteryW - sol)),
		IDAcport: int(r.InverterW - (sol - r.BatteryW)),
	}
}
