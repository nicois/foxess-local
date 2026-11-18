package frame

import (
	"encoding/binary"
	"errors"
)

// 150 B telemetry field offsets (profile §4; mon150 print150).
const (
	teleSolOff       = 35 // s32 BE: PV generation, W
	teleGridOff      = 39 // s32 BE: grid at panel, W (+import / -export)
	teleHausOff      = 43 // s32 BE: household load, W
	teleAcportOff    = 59 // s32 BE: inverter AC port, W (+out / -in)
	teleHausDupOff   = 65 // u16 BE: duplicate of the household load
	teleBattOff      = 67 // s32 BE: battery, W (+charge / -discharge)
	teleChargeState  = 72 // u8: 02 idle / 04 charging
	teleChargeBitOff = 73 // u16 BE: bit 8 set while charging
	teleUnk76Off     = 76 // u16 BE: meaning open (constant 256 in the corpus)
	teleSocOff       = 79 // u8: battery SoC % (duplicated at 71)
	teleUnk83Off     = 83 // u8: meaning open (constant 0xFF in the corpus)
	teleUnk97Off     = 97 // u8: meaning open (constant 0xFF in the corpus)
)

// Tele is the 150 B power-telemetry frame (5 s cadence). PV bypasses the AC
// stage, so the identities hold exactly at the frame instant:
//
//	grid = haus + batt - sol  (panel balance)
//	acport = sol - batt       (the AC port carries only the PV/battery net)
type Tele struct {
	Sol, Grid, Haus, Acport, Batt int32 // s32 BE @35/39/43/59/67
	HausDup                       int   // u16 BE @65
	Soc                           byte  // @79
	ChargeState                   byte  // @72 (02 idle / 04 charging)
	ChargeBit                     int   // u16 BE @73
	Unk76                         int   // u16 BE @76
	Unk83                         byte  // @83
	Unk97                         byte  // @97
	IDGrid                        int   // grid-(haus+batt-sol); 0 = self-consistent
	IDAcport                      int   // acport-(sol-batt); 0 = self-consistent
	// Eps is the EPS (backup / UPS) port output, W — from the register
	// poll (KHRealtime); the 150 B frame has no such field (0).
	Eps int32
	// Temps are the register poll's temperatures and BMS charge limit;
	// nil for the 150 B frame (it carries none).
	Temps *Temps
	// InvState is the inverter state register (31027): 3 on grid, 4 off
	// grid / EPS (foxess_modbus); nil for the 150 B frame.
	InvState *int
}

// Temps are the inverter's temperature readings (0.1 °C) and the BMS
// charge-current limit (0.1 A), which the BMS derives from the battery
// temperature and SoC.
type Temps struct {
	BattDeciC, InvDeciC, AmbDeciC int
	BMSChargeDeciA                int
	// CellMinDeciC / CellMaxDeciC are the BMS's lowest / highest cell
	// temperature from the newest snap61 (nil when there is none recent
	// enough; the store fills them in).
	CellMinDeciC, CellMaxDeciC *int
}

// ParseTele decodes a 150 B tele payload (profile §4 field map).
func ParseTele(p []byte) (Tele, error) {
	if len(p) != teleLen {
		return Tele{}, errors.New("frame: ParseTele: want 150 B payload")
	}
	//nolint:gosec // G115: the fields are signed 32-bit protocol values.
	s32 := func(i int) int32 { return int32(binary.BigEndian.Uint32(p[i : i+4])) }
	sol, grid := s32(teleSolOff), s32(teleGridOff)
	haus, ac := s32(teleHausOff), s32(teleAcportOff)
	batt := s32(teleBattOff)
	u16 := func(i int) int { return int(binary.BigEndian.Uint16(p[i : i+2])) }
	return Tele{
		Sol:         sol,
		Grid:        grid,
		Haus:        haus,
		Acport:      ac,
		Batt:        batt,
		HausDup:     u16(teleHausDupOff),
		Soc:         p[teleSocOff],
		ChargeState: p[teleChargeState],
		ChargeBit:   u16(teleChargeBitOff),
		Unk76:       u16(teleUnk76Off),
		Unk83:       p[teleUnk83Off],
		Unk97:       p[teleUnk97Off],
		IDGrid:      int(grid) - int(haus) - int(batt) + int(sol),
		IDAcport:    int(ac) - int(sol) + int(batt),
	}, nil
}
