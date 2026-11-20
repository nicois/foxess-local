package gateway

import (
	"fmt"

	"foxesslocal/internal/frame"
)

// Register map and wire-shape constants (profile §5).
const (
	limitMax = 30000 // import/export limit W (installer range 0–30000)
	compMin  = -500  // meter compensation W (signed)
	compMax  = 500
	socMax   = 100 // SoC percent fields
	// Enabled scheduler groups: the installer schema's ranges
	// (k121__segmented_time_mode, dump 2026-11-16).
	slotSocMin   = 10    // max SoC / min SoC on grid / fd SoC
	slotFdPwrMax = 10500 // fd power W
	slotWMMax    = 7     // spec Table 3-11 work-mode enum (1 SelfUse .. 7 ForceDischarge)

	regWorkMode  = 41000 // 0xA028: 0 self-use / 1 feed-in priority
	regImportLim = 41021 // 0xA03D: import limit W (×2 registers)
	regExportLim = 41023 // 0xA03F: export limit W (×2 registers)
	regMeterComp = 41029 // 0xA045: meter compensation int16 W

	regModeEn     = 48000 // 0xBB80 ×10: r0 = mode-scheduler master flag
	regSlotTable  = 48010 // 0xBB8A ×80: eight time groups of 10 registers
	regMirrorBase = 48090 // first mirror ×80 block
	regMirrorTop  = 48890 // last mirror ×80 block (inclusive)

	blockModeEn  = 10 // registers in the 48000 block
	blockSlotCnt = 80 // registers in the 48010 / mirror blocks
	slotRegs     = 10 // registers per time group
	mirrorStep   = 80 // mirror block stride

	// Modbus function codes (profile §5).
	fnRead       = 0x03
	fnInputRead  = 0x04
	fnWriteOne   = 0x06
	fnWriteMulti = 0x10

	// Modbus PDU layout (mirrors internal/frame's parse offsets).
	pduFnOff     = 1
	pduAddrOff   = 2
	pduField2Off = 4 // read: count; fn06: value
	pduBCOff     = 6 // fn10: byte count
	pduDataOff   = 7 // fn10: register bytes
	readPDUlen   = 6
	fn10HdrLen   = 7

	// 7f7f envelope bytes (frame layout: 13+plen B).
	frameStart    = 0x7f
	frameTerm     = 0xf7
	frameOverhead = 13

	// Modbus register data width.
	limitRegs   = 2 // each power limit is a U32 (high word, low word)
	bytesPerReg = 2 // big-endian 16-bit register
)

// slotCount is the number of time groups in the 48010 table.
const slotCount = 8

const (
	// Group-of-10 offsets (profile §5; KH10 fw 1.73 packs max:min into +4
	// and shifts the fd fields left vs spec Table 3-11).
	slotEn        = 0
	slotStart     = 1
	slotEnd       = 2
	slotWorkMode  = 3
	slotPackedSoc = 4 // u8 MaxSoC : u8 MinSoCOnGrid
	slotFdSoc     = 5
	slotFdPwr     = 6
	slotReserved  = 7 // reserved (0)
	slotUndoc8    = 8 // preserved from the read; group-3 +8 = the r28 latch
	slotUndoc9    = 9 // preserved from the read

	latchBlockA = 48090 // r28 latch block 1
	latchBlockB = 48170 // r28 latch block 2

	fnUnit = 0x01 // Modbus unit id in every PDU
)

// The device field of built s2c frames: its first byte is the request
// type — readLead for a read (the inverter answers with lead|0x80, 0x91),
// writeLead for a write (echoed 0x92). Every read in the real cloud's
// traffic leads 0x11 and every write 0x12; the KH10 ignores a write led
// 0x11 (profile §5, 2026-11-20). Bytes [1:4] vary per frame in the
// cloud's traffic; the hook substitutes the session tail before
// injection and recomputes the CRC (frame.CRC16 over raw[2:9+plen]).
const (
	readLead  = 0x11
	writeLead = 0x12
)

// s2cDevice is the device field for a request PDU (by its function).
func s2cDevice(pdu []byte) [4]byte {
	if len(pdu) > pduFnOff && (pdu[pduFnOff] == fnWriteOne || pdu[pduFnOff] == fnWriteMulti) {
		return [4]byte{writeLead, 0, 0, 0}
	}
	return [4]byte{readLead, 0, 0, 0}
}

// byteMask / byteShift split a 16-bit value into wire bytes (gosec G115).
const (
	byteMask  = 0xFF
	byteShift = 8
)

// u16BE encodes v big-endian at p[off:off+2]. Callers pass validated
// 16-bit values (Modbus registers are 16 bits).
func u16BE(p []byte, off, v int) {
	//nolint:gosec // G115: callers pass validated 16-bit values.
	p[off] = byte(v >> byteShift)

	p[off+1] = byte(v & byteMask)
}

// regAddr converts a register address to the wire type. Callers stay within
// the schedule block set (48000..48969) and the targeted-write registers.
func regAddr(v int) uint16 {
	//nolint:gosec // G115: callers pass validated register addresses < 49000.
	return uint16(v)
}

// regVal converts a validated 0..30000 W limit to the wire type.
func regVal(v int) uint16 {
	//nolint:gosec // G115: callers pass validated 0..30000 W limits.
	return uint16(v)
}

// compWire is the two's-complement wire form of a signed compensation value.
func compWire(v int) int {
	//nolint:gosec // G115: v is validated to -500..500 (fits int16).
	return int(uint16(int16(v)))
}

// mirrorBlocks are the mirror ×80 bases 48090–48890 step 80 (eleven blocks;
// profile §5 resolved 11-06: partial copies of the 48010 table).
func mirrorBlocks() []int {
	var out []int
	for b := regMirrorBase; b <= regMirrorTop; b += mirrorStep {
		out = append(out, b)
	}
	return out
}

// BuildFrames builds the complete wire sequence for a command: for schedule
// the read phase (fn03 of the config block set) followed by the write phase
// (the read-modify-write fn10 push); for the other kinds the write frames
// directly. current supplies the preserved registers for schedule — the
// full 48010×80 block and every mirror ×80 block must be present, or the
// command errors (never fabricated register values).
func (g *Gateway) BuildFrames(c Command, current []RegisterValue, envFn byte) ([]frame.Frame, error) {
	if err := validate(c); err != nil {
		return nil, err
	}
	switch c.Kind {
	case KindSchedule:
		reads := readFrames(c, envFn)
		writes, _, err := scheduleWriteFrames(c, current, envFn)
		if err != nil {
			return nil, err
		}
		return append(reads, writes...), nil
	case KindWorkMode, KindLimits, KindMeterCompensation,
		KindCurrentLimits, KindBatteryCurrents, KindPowerLimit, KindPortLimits, KindEPSMode, KindClock:
		frs, _, err := writeFrames(c, nil, envFn)
		return frs, err
	case KindRead:
		//nolint:gosec // G115: fn validated to 3 or 4.
		return []frame.Frame{modbusFrame(envFn, readPDU(byte(c.Params[ParamFn]), c.Params[ParamAddr], c.Params[ParamCount]))}, nil
	}
	return nil, fmt.Errorf("gateway: BuildFrames: unknown kind %q", c.Kind)
}

// readReq / writeReq are the correlation units for the ACK state machine.
type (
	readReq  struct{ addr, cnt int }
	writeReq struct {
		fn   byte // 0x06 single write / 0x10 write-multiple
		addr int
		cnt  int // fn10: register count
		val  int // fn06: written value
	}
)

// scheduleReads is the read phase of the config read-modify-write push
// (profile §5: 48000×10, 48010×80, the eleven mirror ×80 blocks — the
// 53400/53100/41007/×50 blocks the cloud also reads are never
// schedule-written, so they are not read here).
func scheduleReads() []readReq {
	out := []readReq{{regModeEn, blockModeEn}, {regSlotTable, blockSlotCnt}}
	for _, b := range mirrorBlocks() {
		out = append(out, readReq{b, blockSlotCnt})
	}
	return out
}

// readFrames builds the fn03 read requests (schedule only).
func readFrames(c Command, envFn byte) []frame.Frame {
	if c.Kind != KindSchedule {
		return nil
	}
	var out []frame.Frame
	for _, r := range scheduleReads() {
		out = append(out, modbusFrame(envFn, fn03PDU(r.addr, r.cnt)))
	}
	return out
}

// writeFrames builds the write phase: workmode/metercompensation → one fn06;
// limits → one fn10 ×2 per present field (import first); schedule → the
// read-modify-write push.
func writeFrames(c Command, current []RegisterValue, envFn byte) ([]frame.Frame, []writeReq, error) {
	switch c.Kind {
	case KindRead:
		return nil, nil, nil // a read has no write phase
	case KindWorkMode:
		v := c.Params[ParamMode]
		return []frame.Frame{modbusFrame(envFn, fn06PDU(regWorkMode, v))},
			[]writeReq{{fn: fnWriteOne, addr: regWorkMode, val: v}}, nil
	case KindMeterCompensation:
		wire := compWire(c.Params[ParamValue])
		return []frame.Frame{modbusFrame(envFn, fn06PDU(regMeterComp, wire))},
			[]writeReq{{fn: fnWriteOne, addr: regMeterComp, val: wire}}, nil
	case KindLimits:
		return limitFrames(c, envFn)
	case KindSchedule:
		return scheduleWriteFrames(c, current, envFn)
	case KindCurrentLimits, KindBatteryCurrents, KindPowerLimit, KindPortLimits, KindEPSMode:
		frs, wrs := settingFrames(c, envFn)
		return frs, wrs, nil
	case KindClock:
		frs, wrs := clockFrames(c, envFn)
		return frs, wrs, nil
	}
	return nil, nil, fmt.Errorf("gateway: writeFrames: unknown kind %q", c.Kind)
}

// limitFrames builds one fn10 ×2 write per present limit field (import
// first). Each limit is a U32 across two registers, high word first.
func limitFrames(c Command, envFn byte) ([]frame.Frame, []writeReq, error) {
	var (
		frs []frame.Frame
		wrs []writeReq
	)
	add := func(reg, v int) {
		// U32, high word first: 0..30000 W always has a zero high word
		// (the cloud's own writes were r0=0 r1=W — profile §5)
		frs = append(frs, modbusFrame(envFn, fn10PDU(reg, []uint16{0, regVal(v)})))
		wrs = append(wrs, writeReq{fn: fnWriteMulti, addr: reg, cnt: limitRegs})
	}
	if v, ok := c.Params[ParamImport]; ok {
		add(regImportLim, v)
	}
	if v, ok := c.Params[ParamExport]; ok {
		add(regExportLim, v)
	}
	return frs, wrs, nil
}

// scheduleWriteFrames builds the read-modify-write push (profile §5):
// 48000×10 (r0 = enabled, rest 0), 48010×80 (slots 1–8 per the ×10 layout,
// +7 = 0, +8/+9 preserved from the read), and the eleven mirror blocks with
// the mirrored fields (+4/+5/+6) updated to match and everything else
// preserved from the read. The r28 latch (48090/48170, group-3 +8) is set to
// 1 when the slot-3 work mode changes and never reverts — replicating an
// unexplained cloud behaviour observed in the 11-05 capture.
func scheduleWriteFrames(c Command, current []RegisterValue, envFn byte) ([]frame.Frame, []writeReq, error) {
	cur := curMap(current)
	if err := requireBlock(cur, regSlotTable, blockSlotCnt, "48010"); err != nil {
		return nil, nil, err
	}
	slots := parseSlots(c)
	for k := range slots {
		slots[k] = unusedShape(slots[k])
	}

	var (
		frs []frame.Frame
		wrs []writeReq
	)
	push := func(addr int, block []uint16) {
		frs = append(frs, modbusFrame(envFn, fn10PDU(addr, block)))
		wrs = append(wrs, writeReq{fn: fnWriteMulti, addr: addr, cnt: len(block)})
	}

	// 48000×10: r0 = enabled, rest 0
	mode := make([]uint16, blockModeEn)
	//nolint:gosec // G115: enabled is validated 0/1.
	mode[0] = uint16(c.Params[ParamEnabled])
	push(regModeEn, mode)

	// 48010×80: slots 1–8 per the ×10 layout; +7 = 0; +8/+9 from the read
	push(regSlotTable, slotTableBlock(slots, cur))

	// mirror blocks: +4/+5/+6 mirrored, everything else preserved from the read
	curWM3 := cur[regAddr(regSlotTable+slotRegs*2+slotWorkMode)] // r23
	for _, base := range mirrorBlocks() {
		mb, err := mirrorBlock(base, slots, curWM3, cur)
		if err != nil {
			return nil, nil, err
		}
		push(base, mb)
	}
	return frs, wrs, nil
}

// unusedSlot is how the cloud writes an unused group (the 48010 reads of
// 2026-11-04 and 2026-11-16, groups 4-8): disabled, 00:00-00:00, work
// mode 1, max:min SoC 100:10, fd SoC 10, fd power 0.
var unusedSlot = slot{wm: 1, maxsoc: socMax, minsoc: slotSocMin, fdsoc: slotSocMin}

// unusedShape writes a cleared disabled slot (every field zero or absent)
// in the cloud's unused-group shape instead of all zeros, which are outside
// the installer ranges. A disabled slot carrying values (a GET /state →
// PUT round-trip of what the inverter holds) passes through unchanged.
func unusedShape(s slot) slot {
	if s == (slot{}) {
		return unusedSlot
	}
	return s
}

// requireBlock errors when any register of a ×cnt block at base is absent.
func requireBlock(cur map[uint16]uint16, base, cnt int, name string) error {
	for i := range cnt {
		a := regAddr(base + i)
		if _, ok := cur[a]; !ok {
			return fmt.Errorf("%w: %s r%d (0x%04X)", ErrMissingCurrent, name, i, a)
		}
	}
	return nil
}

// slotTableBlock builds the 48010×80 write: command fields per the ×10
// layout, +7 = 0, +8/+9 preserved from the read (presence pre-checked).
func slotTableBlock(slots []slot, cur map[uint16]uint16) []uint16 {
	table := make([]uint16, blockSlotCnt)
	for k := range slots {
		off := slotRegs * k
		s := slots[k]
		//nolint:gosec // G115: all fields validated by validateSchedule.
		table[off+slotEn] = uint16(s.en)
		//nolint:gosec // G115: validated packed HHMM.
		table[off+slotStart] = uint16(s.start)
		//nolint:gosec // G115: validated packed HHMM.
		table[off+slotEnd] = uint16(s.end)
		//nolint:gosec // G115: validated wire enum 1..7.
		table[off+slotWorkMode] = uint16(s.wm)
		//nolint:gosec // G115: validated 0..100 percent pairs.
		table[off+slotPackedSoc] = uint16(s.maxsoc)<<8 | uint16(s.minsoc)
		//nolint:gosec // G115: validated 0..100.
		table[off+slotFdSoc] = uint16(s.fdsoc)
		//nolint:gosec // G115: validated 0..30000.
		table[off+slotFdPwr] = uint16(s.fdpwr)
		table[off+slotReserved] = 0
		for _, o := range []int{slotUndoc8, slotUndoc9} {
			table[off+o] = cur[regAddr(regSlotTable+off+o)] // presence checked
		}
	}
	return table
}

// mirrorBlock builds one mirror ×80 write: all registers preserved from the
// read (missing data is an error, never fabricated), +4/+5/+6 of each group
// updated to match the command. The r28 latch (group-3 +8) is set on the two
// latch blocks when the slot-3 work mode changes and never reverts.
func mirrorBlock(base int, slots []slot, curWM3 uint16, cur map[uint16]uint16) ([]uint16, error) {
	mb := make([]uint16, blockSlotCnt)
	for i := range blockSlotCnt {
		a := regAddr(base + i)
		v, ok := cur[a]
		if !ok {
			return nil, fmt.Errorf("%w: %d r%d (0x%04X)", ErrMissingCurrent, base, i, a)
		}
		mb[i] = v
	}
	for k := range slots {
		off := slotRegs * k
		s := slots[k]
		//nolint:gosec // G115: validated 0..100 percent pairs.
		mb[off+slotPackedSoc] = uint16(s.maxsoc)<<8 | uint16(s.minsoc)
		//nolint:gosec // G115: validated 0..100.
		mb[off+slotFdSoc] = uint16(s.fdsoc)
		//nolint:gosec // G115: validated 0..30000.
		mb[off+slotFdPwr] = uint16(s.fdpwr)
	}
	//nolint:gosec // G115: validated wire enum.
	if (base == latchBlockA || base == latchBlockB) && uint16(slots[2].wm) != curWM3 {
		mb[slotRegs*2+slotUndoc8] = 1
	}
	return mb, nil
}

// curMap indexes a register list by address.
func curMap(cur []RegisterValue) map[uint16]uint16 {
	out := make(map[uint16]uint16, len(cur))
	for _, rv := range cur {
		out[rv.Addr] = rv.Value
	}
	return out
}

// modbusFrame wraps a Modbus PDU in the 7f7f cloud-channel envelope
// (13+plen B: start 7f7f, device, fn, plen BE, payload, CRC16 LE over
// [2:9+plen], terminator f7f7 — profile §3/§5).
func modbusFrame(envFn byte, pdu []byte) frame.Frame {
	dev := s2cDevice(pdu)
	raw := make([]byte, 0, frameOverhead+len(pdu))
	raw = append(raw, frameStart, frameStart)
	raw = append(raw, dev[:]...)
	//nolint:gosec // G115: payload length is bounded by the schedule block set.
	raw = append(raw, envFn, byte(len(pdu)>>byteShift), byte(len(pdu)&byteMask))
	raw = append(raw, pdu...)
	crc := frame.CRC16(raw[2:])
	raw = append(raw, byte(crc&byteMask), byte(crc>>byteShift))
	raw = append(raw, frameTerm, frameTerm)
	return frame.Frame{
		Raw:     raw,
		Device:  dev,
		Fn:      envFn,
		Payload: raw[9 : 9+len(pdu)],
	}
}

// fn03PDU is a read request: 01 03 <addr> <cnt>.
func fn03PDU(addr, cnt int) []byte { return readPDU(fnRead, addr, cnt) }

// readPDU is a read request: 01 <fn> <addr> <cnt> (fn 03 holding / 04 input).
func readPDU(fn byte, addr, cnt int) []byte {
	p := make([]byte, readPDUlen)
	p[0], p[pduFnOff] = fnUnit, fn
	u16BE(p, pduAddrOff, addr)
	u16BE(p, pduField2Off, cnt)
	return p
}

// fn06PDU is a single-register write: 01 06 <addr> <val>.
func fn06PDU(addr, val int) []byte {
	p := make([]byte, readPDUlen)
	p[0], p[pduFnOff] = fnUnit, fnWriteOne
	u16BE(p, pduAddrOff, addr)
	u16BE(p, pduField2Off, val)
	return p
}

// fn10PDU is a write-multiple: 01 10 <addr> <cnt> <bc> <data>.
func fn10PDU(addr int, vals []uint16) []byte {
	p := make([]byte, fn10HdrLen+bytesPerReg*len(vals))
	p[0], p[pduFnOff] = fnUnit, fnWriteMulti
	u16BE(p, pduAddrOff, addr)
	u16BE(p, pduField2Off, len(vals))
	//nolint:gosec // G115: byte count = 2*registers, bounded by blockSlotCnt.
	p[pduBCOff] = byte(bytesPerReg * len(vals))
	for i := range vals {
		u16BE(p, pduDataOff+bytesPerReg*i, int(vals[i]))
	}
	return p
}
