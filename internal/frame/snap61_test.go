package frame

import (
	"bytes"
	"fmt"
	"math"
	"testing"
)

// TestSnap61Fixture pins ParseSnap61 against a 11-05 snapshot: 30 TLV
// entries, the confirmed SoC/energy/bus-voltage fields, and the zero pads.
func TestSnap61Fixture(t *testing.T) {
	const dir = sess1105b
	f := frameAt(t, dir, dirC2S, 6461)
	if kind, _ := Decode(f); kind != Kind("snap61") {
		t.Fatalf("kind: got %s, want snap61", kind)
	}
	s, err := ParseSnap61(f.Payload)
	if err != nil {
		t.Fatalf("ParseSnap61: %v", err)
	}
	// the corpus carries a real serial (never written into the
	// repository): check its shape, not its value
	if len(s.SN) != 15 || s.SN[:4] != "60KB" {
		t.Errorf("SN: got %q, want a 15-character 60KB… serial", s.SN)
	}
	if len(s.TLV) != 30 {
		t.Errorf("TLV entries: got %d, want 30 (21 main + 9 module)", len(s.TLV))
	}
	if s.SocPct != 61 {
		t.Errorf("SocPct: got %d, want 61", s.SocPct)
	}
	if s.EnergyUnits != 614 {
		t.Errorf("EnergyUnits: got %d, want 614", s.EnergyUnits)
	}
	if len(s.BusVoltage) != 9 {
		t.Fatalf("BusVoltage: got %d entries, want 9", len(s.BusVoltage))
	}
	for i, v := range s.BusVoltage {
		// 0.01 V units: the pack DC bus read 527.0-527.64 V here.
		if v < 52000 || v > 53000 {
			t.Errorf("BusVoltage[%d]: got %d (0.01 V), want in [52000, 53000]", i, v)
		}
	}
	if !bytes.Equal(s.Remainder, make([]byte, len(s.Remainder))) {
		t.Errorf("Remainder: expected all-zero padding, got % x", s.Remainder)
	}
	if len(s.Remainder) != 226 {
		t.Errorf("Remainder len: got %d, want 226 (120 pad + 106 pad)", len(s.Remainder))
	}
}

// TestSnap61EnergyCapacity checks that the 1907 counter is proportional
// to SoC over the full 11-05 burst session (131 frames): counter ÷ SoC
// fraction stays in a narrow band. It asserts proportionality only — the
// counter's unit is undetermined (the pack is 41.93 kWh nominal).
func TestSnap61EnergyCapacity(t *testing.T) {
	const dir = sess1105a
	frames, _ := splitFile(t, snapshotSession(t, dir), dirC2S)
	minCap, maxCap := math.Inf(1), math.Inf(-1)
	n := 0
	for _, f := range frames {
		if kind, _ := Decode(f); kind != Kind("snap61") {
			continue
		}
		capWh, err := snap61CapacityWh(f)
		if err != nil {
			t.Fatalf("off=%d: %v", f.Off, err)
		}
		if capWh < minCap {
			minCap = capWh
		}
		if capWh > maxCap {
			maxCap = capWh
		}
		n++
	}
	if n != 131 {
		t.Fatalf("snap61 frames: got %d, want 131", n)
	}
	// counts per SoC percent at 100 % SoC: 970–1016 (the SoC floor
	// widens the band at low SoC; live 2026-11-16: 989 at 100 %)
	if minCap < 970 || maxCap > 1016 {
		t.Errorf("counts-per-full-SoC range: got [%.1f, %.1f], want within [970, 1016]", minCap, maxCap)
	}
	t.Logf("counter-at-100%% range over %d frames: [%.1f, %.1f]", n, minCap, maxCap)
}

// snap61CapacityWh is the 1907 counter scaled to 100 % SoC for one
// snap61 frame (counter ÷ SoC fraction) — a proportionality check; the
// counter's unit is undetermined (profile §4).
func snap61CapacityWh(f Frame) (float64, error) {
	s, err := ParseSnap61(f.Payload)
	if err != nil {
		return 0, err
	}
	if s.SocPct < 1 {
		return 0, fmt.Errorf("SocPct=%d: no SoC floor", s.SocPct)
	}
	return float64(s.EnergyUnits) / (float64(s.SocPct) / 100), nil
}

// TestSnap61SizeError pins the size check.
func TestSnap61SizeError(t *testing.T) {
	if _, err := ParseSnap61(make([]byte, 625)); err == nil {
		t.Errorf("ParseSnap61(625 B) succeeded, want error")
	}
	if _, err := ParseTele(make([]byte, 149)); err == nil {
		t.Errorf("ParseTele(149 B) succeeded, want error")
	}
}

// TestSnap61CellTemps: tag 0x1874 carries the BMS's highest [0:2] and
// lowest [2:4] cell temperature, 0.1 °C signed little-endian (verified
// 2026-11-18 against the FoxESS portal's "BMS battery temperature" — the
// minimum — 14 of 15 changes matched). A frame without the tag has none.
func TestSnap61CellTemps(t *testing.T) {
	p := make([]byte, snap61Total)
	e := snap61TLVBase + 2*snap61TLVStride
	p[e], p[e+1] = 0x18, 0x74
	copy(p[e+2:], []byte{0xfd, 0x00, 0xce, 0xff}) // max 25.3 °C, min −5.0 °C
	s, err := ParseSnap61(p)
	if err != nil {
		t.Fatal(err)
	}
	if !s.HasCellTemps || s.CellMaxDeciC != 253 || s.CellMinDeciC != -50 {
		t.Fatalf("cell temps: %+v %d %d", s.HasCellTemps, s.CellMaxDeciC, s.CellMinDeciC)
	}
	s, err = ParseSnap61(make([]byte, snap61Total))
	if err != nil || s.HasCellTemps {
		t.Fatalf("no tag: %v %v", s.HasCellTemps, err)
	}
}

// TestSnap61FixtureCellTemps: the corpus snapshot carries plausible cell
// temperatures (lowest ≤ highest, a mild indoor range).
func TestSnap61FixtureCellTemps(t *testing.T) {
	s, err := ParseSnap61(frameAt(t, sess1105b, dirC2S, 6461).Payload)
	if err != nil {
		t.Fatal(err)
	}
	if !s.HasCellTemps || s.CellMinDeciC > s.CellMaxDeciC || s.CellMinDeciC < 0 || s.CellMaxDeciC > 450 {
		t.Errorf("cell temps: %v min %d max %d (0.1 °C)", s.HasCellTemps, s.CellMinDeciC, s.CellMaxDeciC)
	}
}
