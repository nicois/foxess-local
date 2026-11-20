package frame

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func splitFile(t *testing.T, dst, which string) ([]Frame, int) {
	t.Helper()
	//nolint:gosec // G304: this test's own t.TempDir snapshot.
	data, err := os.ReadFile(filepath.Join(dst, which+".bin"))
	if err != nil {
		t.Fatalf("read %s: %v", which, err)
	}
	frames, res := Split(data, 0)
	return frames, res
}

// TestModbusT2Reads pins ParseModbus against the 216 B T2 session's s2c
// stream: four fn03 @48000 x10, four fn03 @48010 x80, one fn04 @10110 x28
// (wire-observed; the profile's "read cycle x3" understates the 48000/48010
// reads).
func TestModbusT2Reads(t *testing.T) {
	dst := snapshotSession(t, t2Session)
	frames, res := splitFile(t, dst, dirS2C)
	if len(frames) != 12 {
		t.Fatalf("frames: got %d, want 12", len(frames))
	}
	got := map[string]int{}
	for _, f := range frames {
		kind, _ := Decode(f)
		if kind != KindMbread {
			continue
		}
		m, err := ParseModbus(f.Payload)
		if err != nil {
			t.Fatalf("off=%d ParseModbus: %v", f.Off, err)
		}
		got[readKey(m)]++
	}
	want := map[string]int{
		"03-48000-10": 4,
		"03-48010-80": 4,
		"04-10110-28": 1,
	}
	if len(got) != len(want) {
		t.Fatalf("read multiset: got %v, want %v", got, want)
	}
	for k, n := range want {
		if got[k] != n {
			t.Errorf("read %s: got %d, want %d (full: %v vs %v)", k, got[k], n, got, want)
		}
	}
	if res != 216 {
		t.Errorf("residual: got %d, want 216 (the whole 216 B s2c stream)", res)
	}
}

// readKey is the multiset key of a Modbus read request.
func readKey(m Modbus) string {
	return fmt.Sprintf("%02x-%d-%d", m.Fn, m.Addr, m.Count)
}

// TestModbusWrites pins ParseModbus against two corpus downlink writes:
// the fn10 scheduler push to the 48010 time-group table and the fn06
// work-mode write to 41000.
func TestModbusWrites(t *testing.T) {
	const dir = sess1105a
	f10 := frameAt(t, dir, dirS2C, 572)
	m, err := ParseModbus(f10.Payload)
	if err != nil {
		t.Fatalf("fn10 ParseModbus: %v", err)
	}
	if m.Fn != 0x10 || m.Addr != 48010 || m.Count != 80 || len(m.Data) != 160 {
		t.Errorf("fn10: got fn=0x%02x addr=%d count=%d len(Data)=%d, want 0x10 48010 80 160",
			m.Fn, m.Addr, m.Count, len(m.Data))
	}
	if RegName(m.Addr) == "" {
		t.Errorf("RegName(48010) is empty; the time-group table is registered")
	}

	f06 := frameAt(t, dir, dirS2C, 6032)
	m, err = ParseModbus(f06.Payload)
	if err != nil {
		t.Fatalf("fn06 ParseModbus: %v", err)
	}
	if m.Fn != 0x06 || m.Addr != 41000 || m.Value != 0 {
		t.Errorf("fn06: got fn=0x%02x addr=%d value=%d, want 0x06 41000 0", m.Fn, m.Addr, m.Value)
	}
	if RegName(m.Addr) == "" {
		t.Errorf("RegName(41000) is empty; the work-mode register is registered")
	}
}

// TestRegName pins the registered map and its unregistered holes.
func TestRegName(t *testing.T) {
	for addr, wantEmpty := range map[int]bool{
		48000: false, 48010: false, 10110: false, 41000: false,
		41021: false, 41023: false, 41029: false,
		57000: false, 57050: false, 57100: false, 57150: false, 57200: false,
		40000: true, // RTC block, unregistered
		15000: true,
		53400: true,
		48090: true, // mirror blocks, unregistered
	} {
		empty := RegName(addr) == ""
		if empty != wantEmpty {
			t.Errorf("RegName(%d): empty=%v, want %v", addr, empty, wantEmpty)
		}
	}
}

// TestParseModbusErrors pins the reject paths.
func TestParseModbusErrors(t *testing.T) {
	cases := []struct {
		name string
		p    []byte
	}{
		{"empty", nil},
		{"wrong unit", []byte{0x02, 0x03, 0xbb, 0x80, 0x00, 0x0a}},
		{"fn03 short", []byte{0x01, 0x03, 0xbb, 0x80, 0x00}},
		{"fn06 short", []byte{0x01, 0x06, 0xa0, 0x28}},
		{"fn10 bc mismatch", []byte{0x01, 0x10, 0xbb, 0x8a, 0x00, 0x05, 0x06, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}},
		{"fn10 wrong length", []byte{0x01, 0x10, 0xbb, 0x8a, 0x00, 0x02, 0x04, 0x00, 0x00, 0x00}},
		{"unsupported fn", []byte{0x01, 0x05, 0xbb, 0x80, 0x00, 0x0a}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseModbus(tc.p); err == nil {
				t.Errorf("ParseModbus(% x) succeeded, want error", tc.p)
			}
		})
	}
}
