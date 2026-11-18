package frame

// Class-signature constants (profile §4/§5; mon150 classify).
const (
	teleLen       = 150 // 150 B power telemetry
	keepaliveLen  = 135 // K12104 carrier
	keepaliveDev0 = 0x06
	rssiLen       = 16 // 4 B head + JSON {"RSSI":N}
	cfg40Len      = 40 // serial + 00 10 at [32:34]
	registerLen   = 23 // t0 logger/gateway SN registration
	echoLen       = 6  // Modbus write/read echo

	modbusUnit     = 0x01 // PDU unit id
	modbusHdr      = 2    // PDU head length (unit + fn)
	respBit        = 0x80 // response bit: device[0] & respBit != 0
	cfg91Dev0      = 0x91 // K12104 config / Modbus read responses
	serialQueryLen = 57   // per-inverter status query
	serialQuerySN0 = 0x36 // "6"
	serialQuerySN1 = 0x30 // "0"

	// SessionDev1 is the constant device[1] byte of every session frame:
	// the inverter's c2s session-pattern frames (keepalive 06 6a S ctr,
	// snapshots 5a 6a S ctr) and the s2c bootstrap responses (profile
	// §3/§4).
	SessionDev1 = 0x6a
)

// Kind is the message class of a decoded frame.
type Kind string

// The frame classes (plan Task 1 class table).
const (
	KindTele          Kind = "tele"
	KindKeepalive     Kind = "keepalive"
	KindRSSI          Kind = "rssi"
	KindCfg40         Kind = "cfg40"
	KindRegister      Kind = "register"
	KindMbecho        Kind = "mbecho"
	KindCfg91         Kind = "cfg91"
	KindSerialQuery   Kind = "serialquery"
	KindMbread        Kind = "mbread"
	KindMbreadResp    Kind = "mbreadresp"
	KindMbWrite       Kind = "mbwrite"
	KindBootstrapResp Kind = "bootstrapresp"
	KindWrapper       Kind = "wrapper"
	KindOther         Kind = "other"
)

// The snapshot classes the app reads (SnapKind names any lead byte; these
// are the observed ones — profile §4).
const (
	KindSnap01 Kind = "snap01" // 141 B: serial, firmware, model
	KindSnap02 Kind = "snap02" // 425 B
	KindSnap04 Kind = "snap04" // 202 B
	KindSnap60 Kind = "snap60" // 280 B: battery module serials
	KindSnap61 Kind = "snap61" // 626 B: battery status (SoC, cell temps)
	KindSnap65 Kind = "snap65" // 165 B
)

// KindResidue is not a decoded class: it records the unframed bytes left
// in a direction's reassembly buffer when a stream ends.
const KindResidue Kind = "residue"

// Snapshot chunk payload sizes (profile §4). The lead device byte selects
// the chunk class (SnapKind); observed pairs: 01/141 B, 02/425 B,
// 04/202 B, 60/280 B, 61/626 B, 65/165 B.
const (
	snapSize141 = 141
	snapSize165 = 165
	snapSize202 = 202
	snapSize280 = 280
	snapSize425 = 425
	snapSize626 = 626
)

// SnapKind is the snapshot class for a lead device byte (the payload size
// must already match; see Decode).
func SnapKind(dev0 byte) Kind {
	const hex = "0123456789abcdef"
	return Kind("snap" + string(hex[dev0>>4]) + string(hex[dev0&0x0f]))
}

// Modbus PDU functions (also used by the class table below).
const (
	fn03 = 0x03
	fn04 = 0x04
	fn06 = 0x06
	fn10 = 0x10
)

// classCheck is one row of the class table: it matches a frame and reports
// its class.
type classCheck func(Frame) (Kind, bool)

// classTable is the plan's class table (port of mon150 classify, extended
// for s2c). Order matters:
//   - cfg91 (device 0x91) precedes the Modbus-response classes, so
//     inverter read responses classify as cfg91 (mon150 parity);
//   - mbecho requires the response bit (device[0]&0x80), which keeps the 6 B
//     cloud fn06/fn10 write requests (mbwrite) apart from the inverter's
//     6 B write-ack echoes (mbecho);
//   - serialquery uses the size + SN prefix, not the envelope fn: the
//     envelope func is a per-session constant (0x10/0xc7/0x1a observed), so
//     fn is not a discriminator.
var classTable = []classCheck{
	classTele, classKeepalive, classRSSI, classCfg40, classRegister,
	classMbecho, classCfg91, classSnap, classSerialQuery,
	classMbusRead, classMbusWrite, classBootstrapResp, classWrapper,
}

// Decode classifies a frame per the class table and returns the class plus
// its fields — the frame payload; use ParseTele/ParseModbus/ParseSnap61 for
// structured decodes.
func Decode(f Frame) (Kind, []byte) {
	for _, c := range classTable {
		if k, ok := c(f); ok {
			return k, f.Payload
		}
	}
	return KindOther, f.Payload
}

func classTele(f Frame) (Kind, bool) {
	return KindTele, len(f.Payload) == teleLen
}

func classKeepalive(f Frame) (Kind, bool) {
	return KindKeepalive, len(f.Payload) == keepaliveLen && f.Device[0] == keepaliveDev0
}

func classRSSI(f Frame) (Kind, bool) {
	return KindRSSI, len(f.Payload) == rssiLen
}

func classCfg40(f Frame) (Kind, bool) {
	return KindCfg40, len(f.Payload) == cfg40Len
}

func classRegister(f Frame) (Kind, bool) {
	p := f.Payload
	return KindRegister, len(p) == registerLen && p[0] == modbusUnit && p[1] == 0x00
}

func classMbecho(f Frame) (Kind, bool) {
	p := f.Payload
	return KindMbecho, len(p) == echoLen && p[0] == modbusUnit &&
		(p[1] == fn06 || p[1] == fn10) && f.Device[0]&respBit != 0
}

func classCfg91(f Frame) (Kind, bool) {
	return KindCfg91, f.Device[0] == cfg91Dev0
}

func classSnap(f Frame) (Kind, bool) {
	return SnapKind(f.Device[0]), isSnapLen(len(f.Payload))
}

func classSerialQuery(f Frame) (Kind, bool) {
	p := f.Payload
	return KindSerialQuery, len(p) == serialQueryLen &&
		p[0] == serialQuerySN0 && p[1] == serialQuerySN1
}

func classMbusRead(f Frame) (Kind, bool) {
	p := f.Payload
	if len(p) < modbusHdr || p[0] != modbusUnit || (p[1] != fn03 && p[1] != fn04) {
		return KindOther, false
	}
	if f.Device[0]&respBit != 0 {
		return KindMbreadResp, true
	}
	return KindMbread, true
}

func classMbusWrite(f Frame) (Kind, bool) {
	p := f.Payload
	return KindMbWrite, len(p) >= modbusHdr && p[0] == modbusUnit &&
		(p[1] == fn06 || p[1] == fn10)
}

func classBootstrapResp(f Frame) (Kind, bool) {
	return KindBootstrapResp, f.Device[0]&respBit != 0 &&
		f.Device[1] == SessionDev1 && isTrioPayload(f.Payload)
}

// classWrapper keys on the wrapper payload length + dev0 — the envelope
// fn varies (0x64/0x83 observed) and is not a discriminator (cutover
// doc §3.3).
func classWrapper(f Frame) (Kind, bool) {
	return KindWrapper, len(f.Payload) == wrapperLen && f.Device[0] == wrapperDev0
}

func isSnapLen(n int) bool {
	switch n {
	case snapSize141, snapSize165, snapSize202, snapSize280, snapSize425, snapSize626:
		return true
	}
	return false
}

// trioPayloads are the fixed bootstrap response payloads r1..r3
// (profile §3, 2026-11-07 structure).
var trioPayloads = [][]byte{
	{0x01, 0x01, 0x01, 0x00, 0x00},
	{},
	{0x01},
}

func isTrioPayload(p []byte) bool {
	for _, pl := range trioPayloads {
		if len(p) == len(pl) {
			eq := true
			for i := range pl {
				if p[i] != pl[i] {
					eq = false
					break
				}
			}
			if eq {
				return true
			}
		}
	}
	return false
}
