package frame

import (
	"bytes"
	"strconv"
	"testing"
)

// decodeFixtures pin one corpus frame per Kind (session, direction, byte
// offset). Offsets are frozen: the four completed sessions are append-only
// captures, so frame offsets never move.
var decodeFixtures = []struct {
	dir   string
	which string
	off   int64
	kind  Kind
}{
	{sess1105b, dirC2S, 679, KindTele},
	{sess1105b, dirC2S, 67, KindKeepalive},
	{sess1105b, dirC2S, 8676, KindRSSI},
	{sess1105b, dirC2S, 8853, KindCfg40},
	{sess1105b, dirC2S, 0, KindRegister},
	{sess1105a, dirC2S, 41665, KindMbecho},
	{t2Session, dirC2S, 1159, KindCfg91},
	{sess1105b, dirC2S, 5576, Kind("snap01")},
	{sess1105b, dirC2S, 5730, Kind("snap02")},
	{sess1105b, dirC2S, 5183, Kind("snap04")},
	{sess1105b, dirC2S, 6168, Kind("snap60")},
	{sess1105b, dirC2S, 6461, Kind("snap61")},
	{sess1105b, dirC2S, 5398, Kind("snap65")},
	{sess1105b, dirS2C, 64, KindSerialQuery},
	{sess1105b, dirS2C, 45, KindMbread},
	{sess1105a, dirS2C, 572, KindMbWrite},
	{t2Session, dirS2C, 0, KindBootstrapResp},
	{t2Session, dirC2S, 215, KindWrapper},
	{t2Session, dirC2S, 36, KindOther},
}

// TestDecodeFixtures classifies one corpus frame per Kind.
func TestDecodeFixtures(t *testing.T) {
	for _, fx := range decodeFixtures {
		t.Run(string(fx.kind)+"@"+strconv.FormatInt(fx.off, 10), func(t *testing.T) {
			f := frameAt(t, fx.dir, fx.which, fx.off)
			kind, fields := Decode(f)
			if kind != fx.kind {
				t.Fatalf("kind: got %s, want %s", kind, fx.kind)
			}
			if !bytes.Equal(fields, f.Payload) {
				t.Fatalf("fields differ from payload: % x", fields)
			}
		})
	}
}

// TestSnapKind pins the device-lead-byte selector.
func TestSnapKind(t *testing.T) {
	for dev0, want := range map[byte]Kind{
		0x01: Kind("snap01"),
		0x02: Kind("snap02"),
		0x04: Kind("snap04"),
		0x60: Kind("snap60"),
		0x61: Kind("snap61"),
		0x65: Kind("snap65"),
	} {
		if got := SnapKind(dev0); got != want {
			t.Errorf("SnapKind(0x%02x): got %s, want %s", dev0, got, want)
		}
	}
}

// TestMbreadVsRespVsCfg91 distinguishes the Modbus read classes by the
// response bit: request dev0 0x11 -> mbread, response-bit dev0 ->
// mbreadresp, except dev0 0x91 which mon150 folds into cfg91 (K12104 config
// read responses and Modbus read responses share device 0x91).
func TestMbreadVsRespVsCfg91(t *testing.T) {
	payload := []byte{0x01, 0x03, 0xbb, 0x80, 0x00, 0x0a}
	cases := []struct {
		dev0 byte
		want Kind
	}{
		{0x11, KindMbread},
		{0xB1, KindMbreadResp},
		{0x91, KindCfg91},
	}
	for _, tc := range cases {
		f := buildFrame(t, 0x7e, [4]byte{tc.dev0, 0x00, 0x00, 0x00}, 0x00, payload)
		frames, res := Split(f, 0)
		if res != len(f) || len(frames) != 1 {
			t.Fatalf("split: frames=%d residual=%d", len(frames), res)
		}
		kind, _ := Decode(frames[0])
		if kind != tc.want {
			t.Errorf("dev0=0x%02x: got %s, want %s", tc.dev0, kind, tc.want)
		}
	}
}

// TestMbechoVsMbWrite distinguishes the 6 B Modbus frames by the response
// bit: the inverter's write-ack echo (dev0 0x92 = 0x12|0x80) is mbecho, the
// cloud's write request (dev0 0x12) is mbwrite.
func TestMbechoVsMbWrite(t *testing.T) {
	payload := []byte{0x01, 0x06, 0xa0, 0x28, 0x00, 0x00}
	f := buildFrame(t, 0x7e, [4]byte{0x92, 0x00, 0x00, 0x00}, 0x00, payload)
	frames, _ := Split(f, 0)
	if kind, _ := Decode(frames[0]); kind != KindMbecho {
		t.Errorf("dev0=0x92: got %s, want mbecho", kind)
	}
	f = buildFrame(t, 0x7e, [4]byte{0x12, 0x00, 0x00, 0x00}, 0x00, payload)
	frames, _ = Split(f, 0)
	if kind, _ := Decode(frames[0]); kind != KindMbWrite {
		t.Errorf("dev0=0x12: got %s, want mbwrite", kind)
	}
}
