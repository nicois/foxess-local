package frame

import "testing"

// TestParseKHEnergy (foxess-control local backend, 2026-11-19): the
// 32000 block's lifetime totals — 32-bit, high word first, 0.1 kWh
// (foxess_modbus KH_PRE133 / KH_133: solar 32000, battery charge 32003,
// discharge 32006, feed-in 32009, grid consumption 32012, yield 32015,
// input 32018, load 32021); the "today" words between are skipped.
func TestParseKHEnergy(t *testing.T) {
	regs := make([]uint16, KHEnergyCount)
	set := func(off int, v uint32) { regs[off], regs[off+1] = uint16(v>>16), uint16(v) } //nolint:gosec // G115: splitting a uint32 into its words
	set(0, 123456)                                                                       // PV 12345.6 kWh (crosses a word: 0x0001_E240)
	set(3, 40010)                                                                        // charge
	set(6, 38001)                                                                        // discharge
	set(9, 91234)                                                                        // feed-in
	set(12, 5678)                                                                        // grid import
	set(15, 150002)                                                                      // yield
	set(18, 70)                                                                          // input
	set(21, 88888)                                                                       // load
	regs[2] = 999                                                                        // today's PV: not a total
	e, err := ParseKHEnergy(regs)
	if err != nil {
		t.Fatal(err)
	}
	want := KHEnergy{PV: 123456, Charge: 40010, Discharge: 38001, FeedIn: 91234, GridImport: 5678, Yield: 150002, Input: 70, Load: 88888}
	if e != want {
		t.Fatalf("got %+v\nwant %+v", e, want)
	}
	if _, err := ParseKHEnergy(regs[:5]); err == nil {
		t.Fatal("a short block parsed")
	}
}
