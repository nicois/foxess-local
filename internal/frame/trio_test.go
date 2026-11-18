package frame

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// readFileTail returns the first n bytes of the snapshot's direction file.
func readFileTail(t *testing.T, dst, which string, n int) []byte {
	t.Helper()
	//nolint:gosec // G304: this test's own t.TempDir snapshot.
	data, err := os.ReadFile(filepath.Join(dst, which+".bin"))
	if err != nil {
		t.Fatalf("read %s: %v", which, err)
	}
	if len(data) < n {
		t.Fatalf("%s: %d bytes, want >= %d", which, len(data), n)
	}
	return data[:n]
}

// trioObserved are the five sessions' bootstrap parameter sets as observed
// on the wire (session bytes, response fn base, per-response fn deltas).
// reqDev0 (the three c2s registration device bytes, registration first) is
// 0x2a/0x2c/0x2b in every session.
var trioReqDev0 = [3]byte{0x2a, 0x2c, 0x2b}

var trioObserved = []struct {
	name string
	p    TrioParams
}{
	{t2Session, TrioParams{SessionBytes: [2]byte{0xea, 0xcb}, FnBase: 0x56, Deltas: [3]byte{0, 0, 1}}},
	{sess1104b, TrioParams{SessionBytes: [2]byte{0xea, 0xe6}, FnBase: 0xa9, Deltas: [3]byte{0, 1, 1}}},
	{sess1105a, TrioParams{SessionBytes: [2]byte{0xeb, 0xd6}, FnBase: 0x80, Deltas: [3]byte{0, 1, 1}}},
	{sess1105b, TrioParams{SessionBytes: [2]byte{0xec, 0x5c}, FnBase: 0x94, Deltas: [3]byte{0, 1, 1}}},
	{sessLive, TrioParams{SessionBytes: [2]byte{0xec, 0x5f}, FnBase: 0xa2, Deltas: [3]byte{0, 1, 1}}},
}

// trioPayloadWant are the fixed response payloads r1..r3.
var trioPayloadWant = [][]byte{
	{0x01, 0x01, 0x01, 0x00, 0x00},
	{},
	{0x01},
}

// TestBootstrapTrioRoundTrip generates each observed parameter set, splits
// the concatenation, and checks the frame structure.
func TestBootstrapTrioRoundTrip(t *testing.T) {
	for _, tc := range trioObserved {
		t.Run(tc.name, func(t *testing.T) {
			trio := BootstrapTrio(trioReqDev0, tc.p)
			if len(trio[0]) != 18 || len(trio[1]) != 13 || len(trio[2]) != 14 {
				t.Fatalf("sizes: got %d/%d/%d B, want 18/13/14 B", len(trio[0]), len(trio[1]), len(trio[2]))
			}
			var buf []byte
			for _, f := range trio {
				buf = append(buf, f...)
			}
			frames, res := Split(buf, 0)
			if res != len(buf) || len(frames) != 3 {
				t.Fatalf("split: frames=%d consumed=%d, want 3/%d", len(frames), res, len(buf))
			}
			for i := range frames {
				checkTrioFrame(t, i, frames[i], tc.p)
			}
		})
	}
}

// checkTrioFrame asserts one generated bootstrap response frame.
func checkTrioFrame(t *testing.T, i int, f Frame, p TrioParams) {
	t.Helper()
	kind, _ := Decode(f)
	if kind != KindBootstrapResp {
		t.Errorf("r%d kind: got %s, want bootstrapresp", i+1, kind)
	}
	if f.Device[0] != trioReqDev0[i]|respBit || f.Device[1] != SessionDev1 {
		t.Errorf("r%d device: got % x, want %02x %02x", i+1, f.Device[:], trioReqDev0[i]|respBit, SessionDev1)
	}
	if f.Device[2] != p.SessionBytes[0] || f.Device[3] != p.SessionBytes[1] {
		t.Errorf("r%d session bytes: got %02x %02x, want %02x %02x",
			i+1, f.Device[2], f.Device[3], p.SessionBytes[0], p.SessionBytes[1])
	}
	if want := p.FnBase + p.Deltas[i]; f.Fn != want {
		t.Errorf("r%d fn: got 0x%02x, want 0x%02x", i+1, f.Fn, want)
	}
	if !bytes.Equal(f.Payload, trioPayloadWant[i]) {
		t.Errorf("r%d payload: got % x, want % x", i+1, f.Payload, trioPayloadWant[i])
	}
}

// TestBootstrapTrioT2ByteExact: the generated T2 trio must equal the first
// 45 bytes of the T2 session's s2c capture. (The profile's 18/14/16 B
// arithmetic says 48 B; the wire is 18+13+14 = 45 B.)
func TestBootstrapTrioT2ByteExact(t *testing.T) {
	p := trioObserved[0].p
	trio := BootstrapTrio(trioReqDev0, p)
	var buf []byte
	for _, f := range trio {
		buf = append(buf, f...)
	}
	dst := snapshotSession(t, t2Session)
	want := readFileTail(t, dst, dirS2C, 45)
	if !bytes.Equal(buf, want) {
		t.Errorf("generated trio:\n %x\nwire:\n %x", buf, want)
	}
}

// TestTrioParamsAt pins the time-derived parameter model (profile §3,
// 11-11 corpus, 11/11 sessions): S = (unix/256) % 65536, q = unix % 256.
// The instant reproduces the 11-11 21:24 session exactly (S f442, q 5e).
func TestTrioParamsAt(t *testing.T) {
	p := TrioParamsAt(time.Unix(1794392670, 0))
	if p.SessionBytes != [2]byte{0xf4, 0x42} {
		t.Errorf("S: got % x, want f4 42", p.SessionBytes)
	}
	if p.FnBase != 0x5e {
		t.Errorf("q: got %#x, want 0x5e", p.FnBase)
	}
	if p.Deltas != [3]byte{0, 0, 0} {
		t.Errorf("deltas: got % x, want 00 00 00", p.Deltas)
	}
	// +256 s: S increments by one, q is unchanged.
	p2 := TrioParamsAt(time.Unix(1794392670+256, 0))
	if p2.SessionBytes != [2]byte{0xf4, 0x43} {
		t.Errorf("S+256s: got % x, want f4 43", p2.SessionBytes)
	}
	if p2.FnBase != 0x5e {
		t.Errorf("q+256s: got %#x, want 0x5e", p2.FnBase)
	}
	// +256*65536 s: S wraps the 16-bit counter, q is unchanged.
	p3 := TrioParamsAt(time.Unix(1794392670+256*65536, 0))
	if p3.SessionBytes != [2]byte{0xf4, 0x42} {
		t.Errorf("S wrap: got % x, want f4 42", p3.SessionBytes)
	}
	// +255 s: q rolls down to 0x5d and S carries.
	p4 := TrioParamsAt(time.Unix(1794392670+255, 0))
	if p4.SessionBytes != [2]byte{0xf4, 0x43} {
		t.Errorf("S carry: got % x, want f4 43", p4.SessionBytes)
	}
	if p4.FnBase != 0x5d {
		t.Errorf("q roll: got %#x, want 0x5d", p4.FnBase)
	}
}
