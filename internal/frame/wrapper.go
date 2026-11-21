package frame

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Push-mode wrapper frame (cutover doc §3.3, 11-15 corpus): the
// inverter's ~3 s-cadence frame during the initial burst. The payload
// (451 B) = 13 B prefix (clock top byte + inner S + 7×00 + 01 b6) + one complete
// 438 B inner frame (typically the 425 B snap02 TLV). The outer frame's
// dev0 is 0x37; the envelope fn varies (0x64/0x83 observed) and is not
// a discriminator — the classifier key is plen 451 + dev0 0x37.
const (
	wrapperLen    = 451  // wrapper payload length
	wrapperDev0   = 0x37 // wrapper outer device lead byte
	wrapperPrefix = 13   // clock top byte (0x6a, 0x6b…) + inner S + 7×00 + 01 b6
)

// Wrapper is the decoded wrapper frame: the identity and class of the
// inner frame embedded in the payload.
type Wrapper struct {
	InnerDev   [4]byte
	InnerFn    byte
	InnerPlen  int
	InnerClass Kind
}

// ParseWrapper validates the wrapper payload (inner frame markers, plen
// fill, inner CRC and terminator) and reports the embedded inner frame's
// identity and class. The prefix head is the session clock's top byte
// (decode.go), not a constant, so it is not checked.
func ParseWrapper(p []byte) (Wrapper, error) {
	if len(p) != wrapperLen {
		return Wrapper{}, fmt.Errorf("frame: ParseWrapper: want %d B payload, got %d", wrapperLen, len(p))
	}
	inner := p[wrapperPrefix:]
	if (inner[0] != startE && inner[0] != startF) || inner[0] != inner[1] {
		return Wrapper{}, errors.New("frame: ParseWrapper: no inner frame markers")
	}
	plen := int(binary.BigEndian.Uint16(inner[plenOff : plenOff+2]))
	if wrapperPrefix+frameOverhead+plen != wrapperLen {
		return Wrapper{}, fmt.Errorf("frame: ParseWrapper: inner plen %d does not fill the payload", plen)
	}
	crc := binary.LittleEndian.Uint16(inner[9+plen : 11+plen])
	if got := CRC16(inner[2 : 9+plen]); got != crc {
		return Wrapper{}, fmt.Errorf("frame: ParseWrapper: inner crc 0x%04x, want 0x%04x", got, crc)
	}
	term := termByte(inner[0])
	if inner[11+plen] != term || inner[12+plen] != term {
		return Wrapper{}, errors.New("frame: ParseWrapper: bad inner terminator")
	}
	kind, _ := Decode(Frame{
		Device:  [4]byte(inner[2 : 2+4]),
		Fn:      inner[fnOff],
		Payload: inner[9 : 9+plen],
	})
	return Wrapper{
		InnerDev:   [4]byte(inner[2 : 2+4]),
		InnerFn:    inner[fnOff],
		InnerPlen:  plen,
		InnerClass: kind,
	}, nil
}
