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
//   - the cloud's upgrade task: JSON behind 17 + its length u16 BE
//     (ParseFwTask).
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
	fwIDLen      = 5
	fwMetaHead   = fwIDLen + 4 + 2 + 1 // ID, size, crc, name length
	fwSeqHead    = fwIDLen + 2         // ID, seq
	fwAckLen     = fwSeqHead + 1       // + status
	fwEndLen     = fwIDLen + 1         // + 00
	fwMaxChunk   = 1024
	jsonTaskHead = 3 // 17 + length u16 BE
	jsonNoteHead = 4 // 0b 01 + length u8 + control
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

// ParseJSONMsg recognises the two JSON message forms, by the length its
// head gives:
//
//   - the cloud's (the upgrade task): a type byte (17) + the JSON's length
//     u16 BE (4 tasks, 315–328 B);
//   - the dongle's notes (profile §4): 0b 01 + the length of what follows
//     u8 + a control byte (03 a status note, 01 RSSI).
func ParseJSONMsg(p []byte) (head, body []byte, ok bool) {
	switch {
	case len(p) > jsonTaskHead && p[jsonTaskHead] == '{' &&
		int(binary.BigEndian.Uint16(p[1:jsonTaskHead])) == len(p)-jsonTaskHead:
		head, body = p[:jsonTaskHead], p[jsonTaskHead:]
	case len(p) > jsonNoteHead && p[jsonNoteHead] == '{' && int(p[2]) == len(p)-3:
		head, body = p[:jsonNoteHead], p[jsonNoteHead:]
	default:
		return nil, nil, false
	}
	if !json.Valid(body) {
		return nil, nil, false
	}
	return head, body, true
}

// JSONMsg is a message in the cloud's JSON form: the type byte, the
// body's length u16 BE, the body.
func JSONMsg(kind byte, body []byte) []byte {
	p := binary.BigEndian.AppendUint16([]byte{kind}, uint16(len(body))) //nolint:gosec // G115: bodies are a frame's size
	return append(p, body...)
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

// FwMCUName names an upgrade task's mcu_type (the KH10 components
// requested 2026-11-19: master 1.73, slave 1.03, manager 1.71, and the
// datalogger: the dongle's ESP32 application).
func FwMCUName(code string) string {
	switch code {
	case "M":
		return "master"
	case "S":
		return "slave"
	case "A":
		return "manager"
	case "X":
		return "datalogger"
	}
	return "unknown (" + code + ")"
}

// DescribeOther is the structured decode of an unclassified frame with a
// known layout, as JSON: {"firmware_task": …} (component, mcu_type,
// filename, size, crc, url), {"firmware_push": …} (name, size, crc) or
// {"json": …} (a JSON note). nil for the rest.
func DescribeOther(p []byte) json.RawMessage {
	var v any
	if t, ok := ParseFwTask(p); ok {
		v = map[string]any{"firmware_task": map[string]any{
			"component": FwMCUName(t.MCU), "mcu_type": t.MCU, "filename": t.Filename,
			"size": t.Size, "crc": t.CRC, "url": t.URL,
		}}
	} else if m, ok := ParseFwMeta(p); ok {
		v = map[string]any{"firmware_push": map[string]any{"name": m.Name, "size": m.Size, "crc": m.CRC}}
	} else if _, body, ok := ParseJSONMsg(p); ok {
		v = map[string]json.RawMessage{"json": body}
	} else {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}
