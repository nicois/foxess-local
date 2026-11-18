package identity

import "testing"

// Reference-device values (the verified KH10).
const (
	refModel = "KH10"
	refSlave = "1.03"
	refMgr   = "1.71"
	refMast  = "1.73"
)

// snap01 builds a 141 B device-info payload in the real layout (profile
// §4): SN [0:15], four NUL-padded firmware strings, the model at [93:109]
// padded with spaces (the live KH10 sends "KH10" + 12 spaces).
func snap01(sn, model string, fw ...string) []byte {
	p := make([]byte, 141)
	copy(p[0:15], sn)
	for i, at := range []int{33, 39, 45, 51} {
		if i < len(fw) {
			copy(p[at:at+4], fw[i])
		}
	}
	copy(p[93:109], model+"            ")
	return p
}

func TestFromSnapshots(t *testing.T) {
	p60 := make([]byte, 280)
	copy(p60[50:65], "BATTERYMODULE01")
	copy(p60[71:86], "BATTERYMODULE02")
	d := FromSnapshots(snap01("TESTSN000000001", refModel, refMast, refSlave, refMgr, "0.00"), p60)
	if d.SN != "TESTSN000000001" || d.Model != refModel {
		t.Fatalf("sn %q model %q", d.SN, d.Model)
	}
	if d.Firmware != (Firmware{Master: refMast, Slave: refSlave, Manager: refMgr}) {
		t.Fatalf("firmware %+v", d.Firmware)
	}
	if len(d.Versions) != 4 || d.Versions[3] != "0.00" {
		t.Fatalf("versions %v", d.Versions)
	}
	if len(d.BatterySNs) != 2 || d.BatterySNs[1] != "BATTERYMODULE02" {
		t.Fatalf("battery SNs %v", d.BatterySNs)
	}
	if d := FromSnapshots(nil, nil); d.SN != "" || d.Model != "" {
		t.Fatalf("empty input decoded %+v", d)
	}
}

// TestModelFromRegisters: holding 30000 ×15, decoded the foxess_modbus
// way — one ASCII char per register (high byte 0) or two (high byte
// first), stopping at the first non-printable, spaces trimmed.
func TestModelFromRegisters(t *testing.T) {
	cases := []struct {
		name string
		regs []uint16
		want string
	}{
		{"two per register", []uint16{0x4B48, 0x3130, 0x2020, 0}, refModel},
		{"one per register", []uint16{'K', 'H', '1', '0', ' ', 0}, refModel},
		{"dotted", []uint16{0x4B48, 0x3130, 0x2E35, 0}, "KH10.5"},
		{"empty", []uint16{0, 0, 0}, ""},
		{"non-printable stops", []uint16{0x4B48, 0x01FF}, "KH"},
	}
	for _, c := range cases {
		if got := ModelFromRegisters(c.regs); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// TestVersionString: 30016-30018 hold the version as hex digits
// (0x173 = refMast, profile §5).
func TestVersionString(t *testing.T) {
	for v, want := range map[uint16]string{0x173: refMast, 0x103: refSlave, 0x171: refMgr, 0x1205: "12.05", 0: ""} {
		if got := VersionString(v); got != want {
			t.Errorf("%#x: %q, want %q", v, got, want)
		}
	}
}

// TestMatchVerified: only an exact model + firmware triple names a
// verified profile; any difference is no match (the confidence levels
// below "exact" are D-020's job, not this lookup's).
func TestMatchVerified(t *testing.T) {
	ref := Firmware{Master: refMast, Slave: refSlave, Manager: refMgr}
	if p, ok := MatchVerified(refModel, ref); !ok || p.Name != "kh10-fw1.73" {
		t.Fatalf("reference device: %+v %v", p, ok)
	}
	for _, c := range []struct {
		model string
		fw    Firmware
	}{
		{refModel, Firmware{Master: "1.74", Slave: refSlave, Manager: refMgr}},
		{"KH8", ref},
		{"", ref},
		{refModel, Firmware{}},
	} {
		if p, ok := MatchVerified(c.model, c.fw); ok {
			t.Errorf("%q %+v matched %+v", c.model, c.fw, p)
		}
	}
}
