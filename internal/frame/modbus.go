package frame

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Modbus PDU layout (profile §5).
const (
	pduFnOff     = 1
	pduAddrOff   = 2
	pduField2Off = 4 // read: count; fn06: value
	pduBCOff     = 6 // fn10: byte count
	pduDataOff   = 7 // fn10: register bytes
	readPDUlen   = 6 // fn03/04/06 request length
	fn10MinLen   = 7
	bytesPerReg  = 2 // Modbus registers are 16 bits
)

// Modbus is a cloud-command Modbus PDU; the payload's first byte is the
// unit id 0x01.
type Modbus struct {
	Fn    byte   // 0x03/0x04 read; 0x06 single write; 0x10 write-multiple
	Addr  int    // start register
	Count int    // fn03/04/0x10: register count
	Value int    // fn06: value
	Data  []byte // fn10 only: 2*Count register bytes
}

// ParseModbus parses a request/command PDU: fn03/04 read = addr+count,
// fn06 write = addr+value, fn10 write-multiple = addr+count+bytecount+data.
func ParseModbus(p []byte) (Modbus, error) {
	if len(p) < modbusHdr || p[0] != modbusUnit {
		return Modbus{}, errors.New("frame: ParseModbus: payload must start with unit 0x01")
	}
	fn := p[pduFnOff]
	be16 := func(i int) int { return int(binary.BigEndian.Uint16(p[i : i+2])) }
	switch fn {
	case 0x03, 0x04:
		if len(p) != readPDUlen {
			return Modbus{}, fmt.Errorf("frame: ParseModbus: fn%02x: want %d B, got %d", fn, readPDUlen, len(p))
		}
		return Modbus{Fn: fn, Addr: be16(pduAddrOff), Count: be16(pduField2Off)}, nil
	case 0x06:
		if len(p) != readPDUlen {
			return Modbus{}, fmt.Errorf("frame: ParseModbus: fn06: want %d B, got %d", readPDUlen, len(p))
		}
		return Modbus{Fn: fn, Addr: be16(pduAddrOff), Value: be16(pduField2Off)}, nil
	case 0x10:
		if len(p) < fn10MinLen {
			return Modbus{}, fmt.Errorf("frame: ParseModbus: fn10: want >= %d B, got %d", fn10MinLen, len(p))
		}
		addr, cnt := be16(pduAddrOff), be16(pduField2Off)
		bc := int(p[pduBCOff])
		if bc != bytesPerReg*cnt || len(p) != pduDataOff+bc {
			return Modbus{}, fmt.Errorf("frame: ParseModbus: fn10: bytecount %d != %d*count %d or wrong length %d",
				bc, bytesPerReg, cnt, len(p))
		}
		return Modbus{Fn: fn, Addr: addr, Count: cnt, Data: append([]byte(nil), p[pduDataOff:pduDataOff+bc]...)}, nil
	default:
		return Modbus{}, fmt.Errorf("frame: ParseModbus: unsupported fn 0x%02x", fn)
	}
}

// RegName is the validated register map (profile §5). It returns "" for
// unregistered addresses.
func RegName(addr int) string {
	switch addr {
	case 48000:
		return "mode-scheduler block (r0=enable)"
	case 48010:
		return "time groups 1-8 (per group: en, start, end, workmode, maxsoc:minsoc, fdsoc, fdpwr, res x3)"
	case 10110:
		return "ro @10110"
	case 41000:
		return "work mode (0 self-use / 1 feed-in)"
	case 41021:
		return "import limit W"
	case 41023:
		return "export limit W"
	case 41029:
		return "meter compensation int16 W"
	case 57000, 57050, 57100, 57150, 57200:
		return "schedule group params (r1:r2 import W, r3:r4 export W, r6 PV power)"
	}
	return ""
}
