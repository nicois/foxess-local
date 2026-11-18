// Package identity identifies the inverter from side-effect-free sources
// (design D-020): the device-info snapshots it pushes unsolicited, and
// the model / version holding registers (30000 ×15, 30016–30018) read
// the way the open-source foxess_modbus integration does. It also holds
// the verified-profile table: the exact devices whose register meanings
// have been proven by captured cloud writes.
package identity

import (
	"fmt"
	"strings"
)

// Firmware is the inverter's three firmware versions ("1.73" form).
type Firmware struct {
	Master  string `json:"master"`
	Slave   string `json:"slave"`
	Manager string `json:"manager"`
}

// Complete reports whether all three versions are known.
func (f Firmware) Complete() bool {
	return f.Master != "" && f.Slave != "" && f.Manager != ""
}

// Device is what the snapshots say about the inverter.
type Device struct {
	SN         string
	Model      string
	Firmware   Firmware
	Versions   []string // all four snap01 version strings, raw
	BatterySNs []string
}

// snap01 / snap60 layout (profile §4, mon150 devinfo).
const (
	snap01SNLo    = 0
	snap01SNHi    = 15
	snap01ModelLo = 93
	snap01ModelHi = 109 // "KH10" + 12 spaces on the reference device
	snap60SlotLo0 = 50
	snap60SlotLen = 15
	snap60Step    = 21
	snap60Slots   = 10
)

// snap01Versions are the four firmware-string fields: master, slave,
// manager, and a fourth ("0.00" on the reference device, meaning open).
var snap01Versions = [][2]int{{33, 37}, {39, 43}, {45, 49}, {51, 55}}

// FromSnapshots decodes the device-info (snap01) and battery-module
// (snap60) payloads; either may be nil.
func FromSnapshots(p01, p60 []byte) Device {
	var d Device
	if len(p01) >= snap01ModelHi {
		d.SN = field(p01, snap01SNLo, snap01SNHi)
		d.Model = field(p01, snap01ModelLo, snap01ModelHi)
		for _, r := range snap01Versions {
			d.Versions = append(d.Versions, field(p01, r[0], r[1]))
		}
		d.Firmware = Firmware{Master: d.Versions[0], Slave: d.Versions[1], Manager: d.Versions[2]}
	}
	for k := range snap60Slots {
		lo := snap60SlotLo0 + snap60Step*k
		if len(p60) < lo+snap60SlotLen {
			break
		}
		if sn := field(p60, lo, lo+snap60SlotLen); sn != "" {
			d.BatterySNs = append(d.BatterySNs, sn)
		}
	}
	return d
}

// field is the ASCII field [lo,hi): cut at the first NUL, spaces trimmed.
func field(p []byte, lo, hi int) string {
	s := string(p[lo:hi])
	if i := strings.IndexByte(s, 0); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// Model and version registers (foxess_modbus: _MODEL_START_ADDRESS,
// master/slave/manager_version for KH_PRE133).
const (
	ModelAddr    = 30000
	ModelCount   = 15
	VersionAddr  = 30016
	VersionCount = 3
)

const (
	asciiLo   = 0x20
	asciiHi   = 0x7E
	byteShift = 8
	byteMask  = 0xFF
)

// ModelFromRegisters decodes the 30000 ×15 model string the
// foxess_modbus way: a register whose high byte is 0 carries one ASCII
// character, otherwise two (high byte first); decoding stops at the
// first non-printable byte and spaces are trimmed.
func ModelFromRegisters(regs []uint16) string {
	var b strings.Builder
	for _, r := range regs {
		hi, lo := byte(r>>byteShift), byte(r&byteMask)
		chars := []byte{hi, lo}
		if hi == 0 {
			chars = []byte{lo}
		}
		for _, c := range chars {
			if c < asciiLo || c > asciiHi {
				return strings.TrimSpace(b.String())
			}
			b.WriteByte(c)
		}
	}
	return strings.TrimSpace(b.String())
}

// VersionString renders a version register: the value's hex digits with
// the last two as the minor part (0x173 → "1.73"); 0 is unknown ("").
func VersionString(v uint16) string {
	if v == 0 {
		return ""
	}
	h := fmt.Sprintf("%03x", v)
	return h[:len(h)-2] + "." + h[len(h)-2:]
}

// Profile is a verified device: model and exact firmware whose register
// meanings were proven by captured cloud writes and read-backs.
type Profile struct {
	Name     string
	Model    string
	Firmware Firmware
	Evidence string
	// Registers are the register meanings proven on this profile
	// (address → name), compared against third-party maps (D-021).
	Registers map[int]string
}

// verified is the verified-profile table (D-020 "exact"). Unexported:
// it decides which devices may be written (C-008), so no other package
// can change it.
var verified = []Profile{{
	Name:     "kh10-fw1.73",
	Model:    "KH10",
	Firmware: Firmware{Master: "1.73", Slave: "1.03", Manager: "1.71"},
	Evidence: "protocol-profile §5 (2026-11-16 cloud write-through session)",
	Registers: map[int]string{
		41000: "work_mode",
		41007: "max_charge_current",
		41008: "max_discharge_current",
		41009: "min_soc",
		41010: "max_soc_from_grid",
		41011: "min_soc_on_grid",
		41012: "max_soc",
		41015: "eps_output",
		41016: "power_limit_pct",
		41021: "import_limit",
		41023: "export_limit",
		41027: "import_current_limit",
		41028: "export_current_limit",
		41029: "meter_compensation",
		41032: "standby_threshold",
	},
}}

// MatchVerified returns the verified profile for an exact model and
// firmware match.
func MatchVerified(model string, fw Firmware) (Profile, bool) {
	for _, p := range verified {
		if model != "" && model == p.Model && fw == p.Firmware {
			return p, true
		}
	}
	return Profile{}, false
}
