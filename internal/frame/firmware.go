package frame

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"sort"
)

// Firmware update messages (profile §6; observed 2026-11-19 on a KH10,
// fw 1.73/1.03/1.71, during a maintenance window):
//
//   - the cloud's upgrade task: JSON behind a 3-byte head (ParseFwTask).
//     The dongle first tries the task's HTTPS url; failing that, the
//     cloud pushes the file over this channel, ~5 min later:
//   - announcement (s2c): ID(5) + size u32 BE + crc u16 BE + name length
//     u8 + name (ParseFwMeta)
//   - chunk (s2c): ID(5) + seq u16 BE + data (1024 B; the last one
//     shorter), seq from 0
//   - acknowledgement (c2s): ID(5) + seq u16 BE + status u8 (05 seen on
//     every chunk)
//   - end (c2s): ID(5) + 00, after the last chunk's acknowledgement
//
// The ID is random per transfer. The crc is the Modbus CRC-16 of the
// whole file: the same algorithm as the frame CRC (CRC16).
const (
	fwIDLen     = 5
	fwMetaHead  = fwIDLen + 4 + 2 + 1 // ID, size, crc, name length
	fwSeqHead   = fwIDLen + 2         // ID, seq
	fwAckLen    = fwSeqHead + 1       // + status
	fwEndLen    = fwIDLen + 1         // + 00
	fwMaxChunk  = 1024
	jsonHeadLen = 3
)

// FwMeta is an in-band transfer's announcement.
type FwMeta struct {
	ID   [fwIDLen]byte
	Size int
	CRC  uint16
	Name string
}

// ParseFwMeta recognises an announcement: the exact length its name
// length implies, a non-zero size and a printable name.
func ParseFwMeta(p []byte) (FwMeta, bool) {
	if len(p) <= fwMetaHead || len(p) != fwMetaHead+int(p[fwMetaHead-1]) {
		return FwMeta{}, false
	}
	name := p[fwMetaHead:]
	for _, c := range name {
		if c < ' ' || c > '~' {
			return FwMeta{}, false
		}
	}
	size := int(binary.BigEndian.Uint32(p[fwIDLen : fwIDLen+4]))
	if size == 0 {
		return FwMeta{}, false
	}
	return FwMeta{
		ID:   [fwIDLen]byte(p[:fwIDLen]),
		Size: size,
		CRC:  binary.BigEndian.Uint16(p[fwIDLen+4 : fwIDLen+6]),
		Name: string(name),
	}, true
}

// FwTransfer reassembles one in-band transfer from its frames.
type FwTransfer struct {
	Meta   FwMeta
	chunks map[int][]byte
	acks   int
	ended  bool
}

// NewFwTransfer starts reassembling the transfer an announcement names.
func NewFwTransfer(m FwMeta) *FwTransfer {
	return &FwTransfer{Meta: m, chunks: map[int][]byte{}}
}

func (t *FwTransfer) ours(p []byte) bool {
	return len(p) >= fwIDLen && [fwIDLen]byte(p[:fwIDLen]) == t.Meta.ID
}

// Chunk takes a chunk frame of this transfer and returns its sequence
// number.
func (t *FwTransfer) Chunk(p []byte) (int, bool) {
	if !t.ours(p) || len(p) <= fwSeqHead || len(p) > fwSeqHead+fwMaxChunk {
		return 0, false
	}
	seq := int(binary.BigEndian.Uint16(p[fwIDLen:fwSeqHead]))
	t.chunks[seq] = append([]byte(nil), p[fwSeqHead:]...)
	return seq, true
}

// Ack takes the inverter's acknowledgement of a chunk: its sequence
// number and status byte.
func (t *FwTransfer) Ack(p []byte) (seq int, status byte, ok bool) {
	if !t.ours(p) || len(p) != fwAckLen {
		return 0, 0, false
	}
	t.acks++
	return int(binary.BigEndian.Uint16(p[fwIDLen:fwSeqHead])), p[fwSeqHead], true
}

// End takes the inverter's end-of-transfer frame.
func (t *FwTransfer) End(p []byte) bool {
	if !t.ours(p) || len(p) != fwEndLen || p[fwIDLen] != 0 {
		return false
	}
	t.ended = true
	return true
}

// Acks is the number of acknowledgements seen.
func (t *FwTransfer) Acks() int { return t.acks }

// Ended reports whether the end frame was seen.
func (t *FwTransfer) Ended() bool { return t.ended }

// Received is the number of distinct chunks.
func (t *FwTransfer) Received() int { return len(t.chunks) }

// Image is the reassembled file: chunks 0..n without a gap, the
// announced size and CRC.
func (t *FwTransfer) Image() ([]byte, error) {
	seqs := make([]int, 0, len(t.chunks))
	for s := range t.chunks {
		seqs = append(seqs, s)
	}
	sort.Ints(seqs)
	img := make([]byte, 0, t.Meta.Size)
	for i, s := range seqs {
		if s != i {
			return nil, fmt.Errorf("frame: firmware %s: chunk %d missing", t.Meta.Name, i)
		}
		img = append(img, t.chunks[s]...)
	}
	if len(img) != t.Meta.Size {
		return nil, fmt.Errorf("frame: firmware %s: %d of %d bytes", t.Meta.Name, len(img), t.Meta.Size)
	}
	if c := CRC16(img); c != t.Meta.CRC {
		return nil, fmt.Errorf("frame: firmware %s: crc %d, announced %d", t.Meta.Name, c, t.Meta.CRC)
	}
	return img, nil
}

// ParseJSONMsg recognises a JSON message: a 3-byte head (meaning open:
// "17 01 3d" on the upgrade task, "0b 01 1d" on a status note) and a
// JSON object.
func ParseJSONMsg(p []byte) (head [jsonHeadLen]byte, body []byte, ok bool) {
	if len(p) <= jsonHeadLen || p[jsonHeadLen] != '{' || !json.Valid(p[jsonHeadLen:]) {
		return head, nil, false
	}
	return [jsonHeadLen]byte(p[:jsonHeadLen]), p[jsonHeadLen:], true
}

// FwTask is the cloud's upgrade task: which component, the file the
// dongle should fetch, its size and Modbus CRC-16. URL is tied to the
// owner's account: keep it out of anything published.
type FwTask struct {
	MCU      string
	Filename string
	URL      string
	Size     int
	CRC      int
}

// ParseFwTask recognises an upgrade task.
func ParseFwTask(p []byte) (FwTask, bool) {
	_, body, ok := ParseJSONMsg(p)
	if !ok {
		return FwTask{}, false
	}
	var m struct {
		TaskType string `json:"task_type"`
		Data     struct {
			MCU      string `json:"mcu_type"`
			URL      string `json:"url"`
			Filename string `json:"filename"`
			CRC      int    `json:"crc"`
			Size     int    `json:"size"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &m) != nil || m.TaskType != "upgrade" {
		return FwTask{}, false
	}
	d := m.Data
	return FwTask{MCU: d.MCU, Filename: d.Filename, URL: d.URL, Size: d.Size, CRC: d.CRC}, true
}

// FwMCUName names an upgrade task's mcu_type (the three KH10 components
// requested 2026-11-19: master 1.73, slave 1.03, manager 1.71).
func FwMCUName(code string) string {
	switch code {
	case "M":
		return "master"
	case "S":
		return "slave"
	case "A":
		return "manager"
	}
	return "unknown (" + code + ")"
}
