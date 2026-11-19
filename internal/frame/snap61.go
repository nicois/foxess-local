package frame

import (
	"encoding/binary"
	"errors"
)

// 626 B snap61 layout (profile §4).
const (
	snap61SNLen     = 15 // [0:15] inverter SN (ASCII)
	snap61HDREnd    = 40 // [15:40] header (near-zero)
	snap61TLVBase   = 40 // [40:292] main TLV table
	snap61TLVN      = 21 // 21 × 12 B entries: tags 1872-187b, 187f, 1900-1909
	snap61TLVStride = 12 // u16 BE tag + 10 B value
	snap61TLVValLen = 10
	snap61Pad1End   = 292 // [292:412] zero pad
	snap61ModBase   = 412 // [412:520] module table
	snap61ModN      = 9   // 9 × 12 B entries: tags 0c05-0c0d
	snap61Pad2End   = 520 // [520:626] zero pad
	snap61Total     = 626

	snap61SocTag       = 0x1905
	snap61SocValOff    = 6 // value [6:8] LE: battery SoC %
	snap61EnergyTag    = 0x1907
	snap61EnergyValOff = 0 // value [0:2] LE: remaining charge, ≈ 0.1 % of nominal capacity
	snap61VoltTagLo    = 0x0c05
	snap61VoltTagHi    = 0x0c0d
	snap61VoltValOff   = 6 // value [6:8] LE: pack DC bus, 0.01 V (scale TBC)
	snap61BMSSocTag    = 0x1908
	snap61BMSSocValOff = 4    // value [4:6] LE: the BMS's SoC, 0.1 %
	snap61BMSSocMax    = 1000 // 100.0 %
	snap61CellTempTag  = 0x1874
	snap61CellMaxOff   = 0 // value [0:2] LE s16: highest cell temp, 0.1 °C
	snap61CellMinOff   = 2 // value [2:4] LE s16: lowest cell temp, 0.1 °C
)

// Snap61 is the 626 B battery-status snapshot: a TLV register map. The 10 B
// TLV values carry little-endian u16 sub-fields.
type Snap61 struct {
	SN     string            // p[0:15], inverter serial
	Header []byte            // p[15:40], raw
	TLV    map[uint16][]byte // every TLV entry, tag → 10 B value; unregistered tags are carried raw
	// Confirmed fields (profile §4):
	SocPct uint16 // tag 0x1905, value [6:8] LE: battery SoC %
	// EnergyUnits is tag 0x1907 [0:2] LE: the BMS's remaining charge, in
	// the unit of its lifetime charge counters (tag 0x1879), ≈ 0.1 % of
	// nominal capacity (~990 at 100 % on the EQ4800-L9 pack; profile §4,
	// decoded 2026-11-20). It is not converted to energy; usable energy
	// is SoC × the configured capacity.
	EnergyUnits uint16
	BusVoltage  []uint16 // tags 0x0c05-0x0c0d, value [6:8] LE: pack DC bus, 0.01 V (scale TBC)
	// BMSSocDeciPct is the BMS's SoC in 0.1 % (tag 0x1908 [4:6] LE;
	// profile §4, 2026-11-20): SocPct is its floor. Valid when
	// HasBMSSoc — set only when it agrees with SocPct, so a model where
	// the tag means something else does not pass it on.
	BMSSocDeciPct uint16
	HasBMSSoc     bool
	// CellMinDeciC / CellMaxDeciC are the BMS's lowest and highest cell
	// temperature (tag 0x1874 [2:4] / [0:2], 0.1 °C signed); the minimum
	// is the portal's "BMS battery temperature", from which the BMS
	// derives the charge limit. Valid when HasCellTemps.
	CellMinDeciC, CellMaxDeciC int
	HasCellTemps               bool
	Remainder                  []byte // unstructured padding: p[292:412] + p[520:626]
}

// ParseSnap61 decodes a 626 B snap61 payload (profile §4).
func ParseSnap61(p []byte) (Snap61, error) {
	if len(p) != snap61Total {
		return Snap61{}, errors.New("frame: ParseSnap61: want 626 B payload")
	}
	s := Snap61{
		SN:        string(p[0:snap61SNLen]),
		Header:    append([]byte(nil), p[snap61SNLen:snap61HDREnd]...),
		TLV:       make(map[uint16][]byte, snap61TLVN+snap61ModN),
		Remainder: make([]byte, 0, snap61ModBase-snap61Pad1End+snap61Total-snap61Pad2End),
	}
	parse := func(base, n int) {
		for k := range n {
			e := base + snap61TLVStride*k
			tag := binary.BigEndian.Uint16(p[e : e+2])
			s.TLV[tag] = append([]byte(nil), p[e+2:e+2+snap61TLVValLen]...)
		}
	}
	parse(snap61TLVBase, snap61TLVN)
	parse(snap61ModBase, snap61ModN)
	s.Remainder = append(s.Remainder, p[snap61Pad1End:snap61ModBase]...)
	s.Remainder = append(s.Remainder, p[snap61Pad2End:snap61Total]...)

	if v, ok := s.TLV[snap61SocTag]; ok {
		s.SocPct = binary.LittleEndian.Uint16(v[snap61SocValOff : snap61SocValOff+2])
	}
	if v, ok := s.TLV[snap61CellTempTag]; ok {
		s.CellMaxDeciC = int(int16(binary.LittleEndian.Uint16(v[snap61CellMaxOff : snap61CellMaxOff+2]))) //nolint:gosec // G115: a signed 16-bit field
		s.CellMinDeciC = int(int16(binary.LittleEndian.Uint16(v[snap61CellMinOff : snap61CellMinOff+2]))) //nolint:gosec // G115: a signed 16-bit field
		s.HasCellTemps = true
	}
	if v, ok := s.TLV[snap61BMSSocTag]; ok {
		d := binary.LittleEndian.Uint16(v[snap61BMSSocValOff : snap61BMSSocValOff+2])
		if _, soc := s.TLV[snap61SocTag]; soc && d <= snap61BMSSocMax && d/10 == s.SocPct {
			s.BMSSocDeciPct, s.HasBMSSoc = d, true
		}
	}
	if v, ok := s.TLV[snap61EnergyTag]; ok {
		s.EnergyUnits = binary.LittleEndian.Uint16(v[snap61EnergyValOff : snap61EnergyValOff+2])
	}
	for tag := uint16(snap61VoltTagLo); tag <= snap61VoltTagHi; tag++ {
		if v, ok := s.TLV[tag]; ok {
			s.BusVoltage = append(s.BusVoltage, binary.LittleEndian.Uint16(v[snap61VoltValOff:snap61VoltValOff+2]))
		}
	}
	return s, nil
}
