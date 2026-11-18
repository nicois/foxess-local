package driver

import "testing"

// TestSelect: the KH_PRE133 layout has a driver; an unrecognised layout
// has none (every write is then unmapped).
func TestSelect(t *testing.T) {
	d, ok := Select(LayoutKHPre133)
	if !ok || d.Name() != "kh-pre133" {
		t.Fatalf("KH_PRE133: %v %v", d, ok)
	}
	if d, ok := Select(""); ok {
		t.Fatalf("empty layout selected %v", d.Name())
	}
}

// TestKHPre133WriteSupport: the four write features the gateway encodes
// for the KH_PRE133 layout are verified encodings; anything else is
// unmapped (no encoding), never silently allowed.
func TestKHPre133WriteSupport(t *testing.T) {
	d, _ := Select(LayoutKHPre133)
	for _, f := range []string{"work_mode", "power_limits", "meter_compensation", "schedule"} {
		if s := d.WriteSupport(f); s != Verified {
			t.Errorf("%s: %s, want verified", f, s)
		}
	}
	if s := d.WriteSupport("eps_recover_soc"); s != NotSupported {
		t.Errorf("eps_recover_soc: %s, want not_supported (41037 illegal address on fw 1.73)", s)
	}
	if s := d.WriteSupport("no_such_feature"); s != Unmapped {
		t.Errorf("unknown feature: %s, want unmapped", s)
	}
}

// TestTentativeDrivers: a foxess_modbus layout gets a driver whose only
// possible write is the work mode, and only where the map puts it at
// holding 41000 with our encoding (0 Self Use, 1 Feed-in First) — the
// gateway's fn06 @41000 write is then the same register and values.
// It is untested (needs the installer's grant), never verified. Every
// other feature is unmapped.
func TestTentativeDrivers(t *testing.T) {
	d, ok := Select("H1_G2_144-HOLDING")
	if !ok {
		t.Fatal("no driver for an imported layout")
	}
	if s := d.WriteSupport("work_mode"); s != Untested {
		t.Errorf("H1_G2 work_mode: %s, want untested", s)
	}
	for _, f := range []string{"power_limits", "meter_compensation", "schedule"} {
		if s := d.WriteSupport(f); s != Unmapped {
			t.Errorf("H1_G2 %s: %s, want unmapped", f, s)
		}
	}
	// H3-Pro keeps work mode at 49203 with 1-based values: our encoding
	// does not fit
	d, _ = Select("H3_PRO_122-HOLDING")
	if s := d.WriteSupport("work_mode"); s != Unmapped {
		t.Errorf("H3_PRO work_mode: %s, want unmapped", s)
	}
	if _, ok := Select("NOT_A_LAYOUT"); ok {
		t.Error("unknown layout selected")
	}
}
