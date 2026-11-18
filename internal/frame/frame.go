// Package frame is the codec for the Foxess KH10 inverter's proprietary
// cloud-channel protocol — framed binary over TCP/14431. It decodes every
// frame in both directions of the inverter's cloud uplink; it is the only
// component that knows the protocol.
//
// Frame layout (reference: mon150 scan, docs/recon/protocol-profile.md
// §3-§5):
//
//	[0:2]             start 7e7e (terminator e7e7) or 7f7f (terminator f7f7)
//	[2:6]             device (4 B)
//	[6]               fn (envelope func; per-session constant per direction)
//	[7:9]             payload length, u16 BE
//	[9:9+plen]        payload
//	[9+plen:11+plen]  CRC-16 over [2:9+plen], stored u16 LE
//	[11+plen:13+plen] terminator
//
// A frame is 13+plen bytes.
package frame

// Framing layout constants.
const (
	startE = 0x7e // start marker; terminator e7e7
	startF = 0x7f // start marker; terminator f7f7

	devOff        = 2  // device [2:6]
	fnOff         = 6  // fn [6]
	plenOff       = 7  // payload length [7:9]
	payloadOff    = 9  // payload starts at 9
	crcSpan       = 2  // CRC field width
	frameOverhead = 13 // frame = frameOverhead + plen
)

// start7e / start7f are the two start-marker byte pairs.
var (
	start7e = []byte{startE, startE}
	start7f = []byte{startF, startF}
)

// termByte is the terminator byte for a start byte: the two nibbles are
// swapped (0x7e -> 0xe7, 0x7f -> 0xf7).
func termByte(start byte) byte {
	return ((start & 0x0f) << 4) | (start >> 4)
}

const (
	byteMask  = 0xFF // low byte of a 16-bit field
	byteShift = 8    // shift to the high byte
)

// crcBytes is the wire form of a frame CRC: little-endian (low byte first).
func crcBytes(crc uint16) [2]byte {
	return [2]byte{byte(crc & byteMask), byte(crc >> byteShift)}
}

// plenBytes is the wire form of a payload length: big-endian u16.
func plenBytes(n int) [2]byte {
	//nolint:gosec // G115: the & byteMask keeps the value within a byte.
	return [2]byte{byte(n >> byteShift), byte(n & byteMask)}
}

// Frame is one decoded cloud-channel frame. Raw is a copy (not a slice of
// the input buffer), so frames outlive their input; Payload aliases Raw.
type Frame struct {
	Raw     []byte // verbatim on-wire bytes (start..terminator incl.)
	Off     int64  // offset of the start byte in the direction stream
	Device  [4]byte
	Fn      byte
	Payload []byte // = Raw[9 : 9+len(payload)]
}

// CRC16 is the frame checksum: init 0xFFFF, poly 0xA001, reflected
// (LSB-first).
func CRC16(b []byte) uint16 {
	const (
		crcInit = 0xFFFF
		crcPoly = 0xA001
		crcBits = 8
	)
	crc := uint16(crcInit)
	for _, c := range b {
		crc ^= uint16(c)
		for range crcBits {
			if crc&1 != 0 {
				crc = crc>>1 ^ crcPoly
			} else {
				crc >>= 1
			}
		}
	}
	return crc
}

// Split parses complete frames from buf and returns them together with the
// number of leading bytes fully consumed (up to the end of the last complete
// frame). A start marker whose declared frame extends past the end of buf
// stops the scan: the remainder is a trailing partial frame. A start marker
// whose CRC or terminator does not match is skipped (rescan from the next
// byte); a bad frame is never fatal.
//
// The scan deliberately resumes one byte after each accepted frame's start
// (not after its terminator): some frames embed a complete frame in their
// payload (the 451 B session-start frame wraps a 425 B snap02, verified on
// every corpus session), and the mon150 reference scanner counts the
// embedded frame too — parity requires it.
func Split(buf []byte, baseOff int64) ([]Frame, int) {
	var out []Frame
	consumed := 0
	for i := range buf {
		if i+frameOverhead > len(buf) {
			break
		}
		//nolint:gosec // G602: the frameOverhead guard above bounds i+1.
		s0, s1 := buf[i], buf[i+1]
		if (s0 != startE || s1 != startE) && (s0 != startF || s1 != startF) {
			continue
		}
		//nolint:gosec // G602: the frameOverhead guard above bounds i+8.
		plen := int(buf[i+plenOff])<<8 | int(buf[i+plenOff+1])
		if i+frameOverhead+plen > len(buf) {
			break // incomplete frame: wait for more bytes
		}
		body := buf[i+devOff : i+payloadOff+plen]
		crc := uint16(buf[i+payloadOff+plen]) | uint16(buf[i+payloadOff+plen+1])<<8
		term := termByte(s0)
		if CRC16(body) != crc || buf[i+payloadOff+plen+crcSpan] != term || buf[i+payloadOff+plen+crcSpan+1] != term {
			continue // bad CRC or terminator: rescan from the next byte
		}
		raw := make([]byte, frameOverhead+plen)
		copy(raw, buf[i:i+frameOverhead+plen])
		out = append(out, Frame{
			Raw:     raw,
			Off:     baseOff + int64(i),
			Device:  [4]byte(raw[devOff : devOff+4]),
			Fn:      raw[fnOff],
			Payload: raw[payloadOff : payloadOff+plen],
		})
		consumed = i + frameOverhead + plen
	}
	return out, consumed
}
