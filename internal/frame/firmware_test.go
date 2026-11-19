package frame

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// fwID is a transfer ID shaped like the 2026-11-19 one (5 B, random per
// transfer).
var fwID = []byte{0xa2, 0x35, 0x46, 0x7b, 0x4d}

// fwTestName names the synthetic test images.
const fwTestName = "K10_Test_V0.01"

func fwMetaPayload(size int, crc uint16, name string) []byte {
	p := append([]byte{}, fwID...)
	p = binary.BigEndian.AppendUint32(p, uint32(size)) //nolint:gosec // G115: a test size
	p = binary.BigEndian.AppendUint16(p, crc)
	p = append(p, byte(len(name))) //nolint:gosec // G115: test names are short
	return append(p, name...)
}

func fwChunkPayload(seq int, data []byte) []byte {
	p := append([]byte{}, fwID...)
	p = binary.BigEndian.AppendUint16(p, uint16(seq)) //nolint:gosec // G115: a test sequence
	return append(p, data...)
}

func fwAckPayload(seq int) []byte {
	return append(fwChunkPayload(seq, nil), 0x05)
}

// TestParseFwMeta: the announcement frame of the 2026-11-19 master push,
// byte for byte (28 B: ID, size 267078, CRC 0x6944, name length 16, the
// name).
func TestParseFwMeta(t *testing.T) {
	p := []byte{
		0xa2, 0x35, 0x46, 0x7b, 0x4d, 0x00, 0x04, 0x13, 0x46, 0x69, 0x44, 0x10,
		'K', '1', '0', '_', 'M', 'a', 's', 't', 'e', 'r', '_', 'V', '1', '.', '7', '3',
	}
	m, ok := ParseFwMeta(p)
	if !ok {
		t.Fatal("announcement not recognised")
	}
	if m.ID != [5]byte(fwID) || m.Size != 267078 || m.CRC != 26948 || m.Name != "K10_Master_V1.73" {
		t.Fatalf("got %+v", m)
	}
	for name, bad := range map[string][]byte{
		"length mismatch": p[:len(p)-1],
		"zero size":       fwMetaPayload(0, 1, "K10_Master_V1.73"),
		"binary name":     fwMetaPayload(10, 1, "K10\x00Master"),
		"empty name":      fwMetaPayload(10, 1, ""),
		"modbus read":     {0x01, 0x03, 0x79, 0x18, 0x00, 0x1d},
	} {
		if _, ok := ParseFwMeta(bad); ok {
			t.Errorf("%s: recognised as an announcement", name)
		}
	}
}

// TestFwTransfer: chunks in, acknowledgements and the end frame matched
// to the transfer, the image reassembled and checked against the
// announced size and CRC (the frame CRC-16: the task's crc field is the
// same Modbus CRC over the whole file).
func TestFwTransfer(t *testing.T) {
	img := make([]byte, 2*1024+300)
	for i := range img {
		img[i] = byte(i * 7)
	}
	tr := NewFwTransfer(FwMeta{ID: [5]byte(fwID), Size: len(img), CRC: CRC16(img), Name: fwTestName})
	for seq := 0; seq*1024 < len(img); seq++ {
		end := min((seq+1)*1024, len(img))
		got, ok := tr.Chunk(fwChunkPayload(seq, img[seq*1024:end]))
		if !ok || got != seq {
			t.Fatalf("chunk %d: got %d, %v", seq, got, ok)
		}
		if s, st, ok := tr.Ack(fwAckPayload(seq)); !ok || s != seq || st != 0x05 {
			t.Fatalf("ack %d: got %d %02x %v", seq, s, st, ok)
		}
	}
	if !tr.End(append([]byte{}, append(fwID, 0x00)...)) {
		t.Fatal("end frame not recognised")
	}
	out, err := tr.Image()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, img) || tr.Acks() != 3 || !tr.Ended() {
		t.Fatalf("image %d B (equal %v), acks %d, ended %v", len(out), bytes.Equal(out, img), tr.Acks(), tr.Ended())
	}
}

// TestFwTransferForeign: another transfer's frames, and other traffic,
// are not this one's.
func TestFwTransferForeign(t *testing.T) {
	tr := NewFwTransfer(FwMeta{ID: [5]byte(fwID), Size: 1, CRC: 0, Name: fwTestName})
	other := []byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x00, 0x01, 0xff}
	if _, ok := tr.Chunk(other); ok {
		t.Error("a foreign chunk was accepted")
	}
	if _, _, ok := tr.Ack(other); ok {
		t.Error("a foreign ack was accepted")
	}
	if _, ok := tr.Chunk([]byte{0x01, 0x03, 0x79, 0x18, 0x00, 0x1d}); ok {
		t.Error("a Modbus read was accepted as a chunk")
	}
}

// TestFwTransferIncomplete: a missing chunk or a CRC mismatch is
// reported, never a silently wrong image.
func TestFwTransferIncomplete(t *testing.T) {
	img := bytes.Repeat([]byte{0x5a}, 1500)
	m := FwMeta{ID: [5]byte(fwID), Size: len(img), CRC: CRC16(img), Name: fwTestName}

	gap := NewFwTransfer(m)
	gap.Chunk(fwChunkPayload(1, img[1024:]))
	if _, err := gap.Image(); err == nil {
		t.Error("a missing chunk 0 gave an image")
	}

	bad := NewFwTransfer(FwMeta{ID: m.ID, Size: m.Size, CRC: m.CRC ^ 1, Name: m.Name})
	bad.Chunk(fwChunkPayload(0, img[:1024]))
	bad.Chunk(fwChunkPayload(1, img[1024:]))
	if _, err := bad.Image(); err == nil {
		t.Error("a CRC mismatch gave an image")
	}
}

// TestParseJSONMsg: the cloud's upgrade task and the dongle's status
// notes are JSON behind a 3-byte head (meaning open).
func TestParseJSONMsg(t *testing.T) {
	task := []byte("\x17\x01\x3d" + `{"task_type":"upgrade","data":{"mcu_type":"A","url":"https://example.invalid/fw","filename":"10-300-11057-37_K10_Manager_V1.71.bin","crc":59102,"size":764904}}`)
	head, body, ok := ParseJSONMsg(task)
	if !ok || head != [3]byte{0x17, 0x01, 0x3d} || body[0] != '{' {
		t.Fatalf("task: %x %q %v", head, body, ok)
	}
	ft, ok := ParseFwTask(task)
	if !ok || ft.MCU != "A" || FwMCUName(ft.MCU) != "manager" || ft.Size != 764904 || ft.CRC != 59102 ||
		ft.Filename != "10-300-11057-37_K10_Manager_V1.71.bin" || ft.URL != "https://example.invalid/fw" {
		t.Fatalf("task: %+v %v", ft, ok)
	}
	note := []byte("\x0b\x01\x1d" + `{"msgs":"485_data_abnormal"}`)
	if _, _, ok := ParseJSONMsg(note); !ok {
		t.Error("status note not recognised")
	}
	if _, ok := ParseFwTask(note); ok {
		t.Error("a status note parsed as an upgrade task")
	}
	for _, bad := range [][]byte{[]byte("\x01\x03{x"), []byte("abc"), {0x01, 0x03, 0x79, 0x18}} {
		if _, _, ok := ParseJSONMsg(bad); ok {
			t.Errorf("%x recognised as JSON", bad)
		}
	}
}
