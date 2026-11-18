package frame

import (
	"bytes"
	"testing"
)

// buildFrame assembles a raw cloud-channel frame: start|start|dev|fn|plen
// BE|payload|crc16 BE|term|term.
func buildFrame(t *testing.T, start byte, dev [4]byte, fn byte, payload []byte) []byte {
	t.Helper()
	plen := plenBytes(len(payload))
	body := make([]byte, 0, frameOverhead+len(payload))
	body = append(body, dev[:]...)
	body = append(body, fn, plen[0], plen[1])
	body = append(body, payload...)
	crc := crcBytes(CRC16(body))
	raw := make([]byte, 0, frameOverhead+len(payload))
	raw = append(raw, start, start)
	raw = append(raw, body...)
	raw = append(raw, crc[0], crc[1])
	term := termByte(start)
	raw = append(raw, term, term)
	return raw
}

var (
	t2SessionBytes = [2]byte{0xea, 0xcb}
	t2FnBase       = byte(0x56)
	t2Deltas       = [3]byte{0, 0, 1}
)

// TestCRC16 pins the checksum against the observed T2 bootstrap frames
// (body dev|fn|plen|payload, CRC transmitted little-endian).
func TestCRC16(t *testing.T) {
	cases := []struct {
		name string
		body []byte
		want uint16
	}{
		// The CRC is transmitted little-endian (low byte first): r2's wire
		// bytes b9 e3 read as 0xE3B9.
		{"trio r1", []byte{0xaa, 0x6a, 0xea, 0xcb, 0x56, 0, 5, 1, 1, 1, 0, 0}, 0xFECF},
		{"trio r2", []byte{0xac, 0x6a, 0xea, 0xcb, 0x56, 0, 0}, 0xE3B9},
		{"trio r3", []byte{0xab, 0x6a, 0xea, 0xcb, 0x57, 0, 1, 1}, 0xF8A2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CRC16(tc.body); got != tc.want {
				t.Errorf("CRC16: got 0x%04X, want 0x%04X", got, tc.want)
			}
		})
	}
}

// TestTermByte pins the start->terminator nibble swap.
func TestTermByte(t *testing.T) {
	if got := termByte(0x7e); got != 0xe7 {
		t.Errorf("termByte(0x7e): got 0x%02x, want 0xe7", got)
	}
	if got := termByte(0x7f); got != 0xf7 {
		t.Errorf("termByte(0x7f): got 0x%02x, want 0xf7", got)
	}
}

// TestSplitSingle parses one well-formed frame and checks every field.
func TestSplitSingle(t *testing.T) {
	payload := []byte{0x01, 0x01, 0x01, 0x00, 0x00}
	raw := buildFrame(t, 0x7e, [4]byte{0xaa, 0x6a, 0xea, 0xcb}, 0x56, payload)
	frames, consumed := Split(raw, 0)
	if consumed != len(raw) {
		t.Fatalf("consumed: got %d, want %d (full frame)", consumed, len(raw))
	}
	if len(frames) != 1 {
		t.Fatalf("frames: got %d, want 1", len(frames))
	}
	f := frames[0]
	if f.Off != 0 || f.Raw[0] != 0x7e || f.Fn != 0x56 || len(f.Payload) != len(payload) {
		t.Errorf("fields: off=%d start=0x%02x fn=0x%02x plen=%d", f.Off, f.Raw[0], f.Fn, len(f.Payload))
	}
	if f.Device != [4]byte{0xaa, 0x6a, 0xea, 0xcb} {
		t.Errorf("device: got % x", f.Device[:])
	}
	if !bytes.Equal(f.Payload, payload) {
		t.Errorf("payload: got % x, want % x", f.Payload, payload)
	}
	if !bytes.Equal(f.Raw, raw) {
		t.Errorf("raw: got % x, want % x", f.Raw, raw)
	}
}

// t2TrioBuf is the observed T2 bootstrap trio, back-to-back (45 B).
func t2TrioBuf(t *testing.T) []byte {
	t.Helper()
	trio := BootstrapTrio(trioReqDev0, TrioParams{
		SessionBytes: t2SessionBytes,
		FnBase:       t2FnBase,
		Deltas:       t2Deltas,
	})
	var buf []byte
	for _, f := range trio {
		buf = append(buf, f...)
	}
	return buf
}

// TestSplitMulti parses the observed T2 trio (18/13/14 B) back-to-back.
func TestSplitMulti(t *testing.T) {
	buf := t2TrioBuf(t)
	frames, consumed := Split(buf, 0)
	if consumed != len(buf) {
		t.Fatalf("consumed: got %d, want %d", consumed, len(buf))
	}
	if len(frames) != 3 {
		t.Fatalf("frames: got %d, want 3", len(frames))
	}
	for i, want := range []int64{0, 18, 31} {
		if frames[i].Off != want {
			t.Errorf("frame %d off: got %d, want %d", i, frames[i].Off, want)
		}
	}
}

// TestSplitPartialTail: a trailing incomplete frame is returned as residual,
// not consumed.
func TestSplitPartialTail(t *testing.T) {
	buf := t2TrioBuf(t)
	// 5 bytes of a next frame: start marker + 3 body bytes (plen says 5).
	buf = append(buf, 0x7e, 0x7e, 0x11, 0x01, 0x01)
	frames, consumed := Split(buf, 0)
	if len(frames) != 3 {
		t.Fatalf("frames: got %d, want 3", len(frames))
	}
	if residual := len(buf) - consumed; residual != 5 {
		t.Fatalf("residual: got %d, want 5", residual)
	}
}

// TestSplitShortTail: fewer bytes than a start marker + plen are residual.
func TestSplitShortTail(t *testing.T) {
	frames, consumed := Split([]byte{0x7e}, 0)
	if len(frames) != 0 || consumed != 0 {
		t.Fatalf("frames=%d consumed=%d, want 0/0 (the lone byte is residual)", len(frames), consumed)
	}
}

// assertSkippedFirstFrame checks that a corrupted r1 was skipped and r2/r3
// still parse at their offsets.
func assertSkippedFirstFrame(t *testing.T, buf []byte) {
	t.Helper()
	frames, consumed := Split(buf, 0)
	if consumed != len(buf) {
		t.Fatalf("consumed: got %d, want %d (r2+r3 still parse)", consumed, len(buf))
	}
	if len(frames) != 2 {
		t.Fatalf("frames: got %d, want 2", len(frames))
	}
	if frames[0].Off != 18 || frames[1].Off != 31 {
		t.Errorf("offs: got %d %d, want 18 31", frames[0].Off, frames[1].Off)
	}
}

// TestSplitCorruptCRC skips the broken frame and still finds the following
// ones.
func TestSplitCorruptCRC(t *testing.T) {
	buf := t2TrioBuf(t)
	buf[15] ^= 0xFF // corrupt r1's last payload byte
	assertSkippedFirstFrame(t, buf)
}

// TestSplitCorruptTerminator skips a frame whose terminator does not match
// its start marker.
func TestSplitCorruptTerminator(t *testing.T) {
	buf := t2TrioBuf(t)
	buf[17] = 0xe8 // break r1's second terminator byte
	assertSkippedFirstFrame(t, buf)
}

// TestSplitStartFamily7f parses the 0x7f7f/0xf7f7 start family (observed on
// the T2 s2c read frames).
func TestSplitStartFamily7f(t *testing.T) {
	payload := []byte{0x01, 0x03, 0xbb, 0x80, 0x00, 0x0a}
	raw := buildFrame(t, 0x7f, [4]byte{0x11, 0xe7, 0x5a, 0x42}, 0x42, payload)
	frames, consumed := Split(raw, 0)
	if consumed != len(raw) || len(frames) != 1 {
		t.Fatalf("frames=%d consumed=%d, want 1/%d", len(frames), consumed, len(raw))
	}
	if frames[0].Raw[0] != 0x7f {
		t.Errorf("start: got 0x%02x, want 0x7f", frames[0].Raw[0])
	}
	if !bytes.Equal(frames[0].Raw[17:19], []byte{0xf7, 0xf7}) {
		t.Errorf("terminator: got % x, want two 0xf7 bytes", frames[0].Raw[17:19])
	}
}
