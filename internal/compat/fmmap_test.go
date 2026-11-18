package compat

import (
	"bufio"
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"

	"foxesslocal/internal/fmdata"
	"foxesslocal/internal/frame"
	"foxesslocal/internal/identity"
)

// probeRegs loads the real register reads of the 2026-11-16 14:52 live
// probe (two active PV strings, 242.6 V / 49.99 Hz, load 4326 W).
func probeRegs(t *testing.T) regs {
	t.Helper()
	f, err := os.Open("testdata/kh-live-values.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	out := regs{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, `{"fn":`) {
			continue
		}
		var r struct {
			Registers []struct {
				Addr  int `json:"addr"`
				Value int `json:"value"`
			} `json:"registers"`
		}
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		for _, x := range r.Registers {
			out[x.Addr] = uint16(x.Value) //nolint:gosec // G115: register values
		}
	}
	return out
}

// TestMapDecodeMatchesOwnDecode: the foxess_modbus KH_PRE133 map,
// applied to the real probe registers, yields the same live values as
// our own decode (frame.ParseKHRealtime), and passes every live check.
func TestMapDecodeMatchesOwnDecode(t *testing.T) {
	r := probeRegs(t)
	m, _ := fmdata.Load().Map("KH_PRE133-HOLDING")
	got := FromMap(m, r.get)

	a, _ := (reader{get: r.get}).block(frame.KHRealtimeAAddr, frame.KHRealtimeACount)
	b, _ := (reader{get: r.get}).block(frame.KHRealtimeBAddr, frame.KHRealtimeBCount)
	rt, err := frame.ParseKHRealtime(a, b)
	if err != nil {
		t.Fatal(err)
	}
	want := FromKH(rt)
	near := func(name string, g, w *float64) {
		t.Helper()
		if g == nil || w == nil || math.Abs(*g-*w) > 0.5 {
			t.Errorf("%s: map %v, own %v", name, deref(g), deref(w))
		}
	}
	near("grid V", got.GridV, want.GridV)
	near("grid Hz", got.GridHz, want.GridHz)
	near("SoC", got.SocPct, want.SocPct)
	near("load W", got.LoadW, want.LoadW)
	near("inverter W", got.InverterW, want.InverterW)
	near("grid import W", got.GridImportW, want.GridImportW)
	for i := range 2 {
		near("PV W", &got.PV[i].W, &want.PV[i].W)
	}
	for _, c := range LiveChecks(got, nil) {
		if c.Status == StatusFail || c.Status == StatusPending {
			t.Errorf("KH_PRE133 map on the probe: %s %s %s", c.Name, c.Status, c.Detail)
		}
	}
}

// TestWrongMapsRejected: every other foxess_modbus map, applied to the
// same real registers, must be rejected — some core live check fails or
// cannot be evaluated. A wrong map that passes means the checks are too
// weak to catch a misidentified generation (design D-021 test use).
func TestWrongMapsRejected(t *testing.T) {
	r := probeRegs(t)
	for key, m := range fmdata.Load().Maps {
		if key == "KH_PRE133-HOLDING" {
			continue
		}
		if Classifiable(LiveChecks(FromMap(m, r.get), nil)) {
			t.Errorf("wrong map %s would classify on the KH10 probe", key)
		}
	}
	// The H1 LAN map shares the KH PV / grid / SoC registers: only the
	// power balance tells it apart (its load register is elsewhere). Its
	// rejection must be a real failure, not missing data.
	m, _ := fmdata.Load().Map("H1_LAN-HOLDING")
	for _, c := range LiveChecks(FromMap(m, r.get), nil) {
		if c.Name == "power_balance" && c.Status != StatusFail {
			t.Fatalf("H1_LAN on the KH10 probe: power_balance %s, want fail (the discriminating check)", c.Status)
		}
	}
}

// TestKnownDivergences: where foxess_modbus's KH_PRE133 settings map and
// our verified profile disagree. Pinned, so a data refresh that changes
// the picture is noticed (and reported upstream, D-021).
func TestKnownDivergences(t *testing.T) {
	m, _ := fmdata.Load().Map("KH_PRE133-HOLDING")
	p, _ := identity.MatchVerified("KH10", refFW)
	got := Divergences(p, m)
	want := []Divergence{{Addr: 41010, Ours: "max_soc_from_grid", Theirs: "max_soc"}}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("divergences %+v, want %+v", got, want)
	}
}

func deref(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

// TestReportThirdParty: the report names the third-party map behind the
// identified layout, its source commit, and where it disagrees with the
// verified profile (D-021: flagged, never silently resolved).
func TestReportThirdParty(t *testing.T) {
	rep := Evaluate(ref())
	tp := rep.ThirdParty
	if tp == nil || tp.Map != "KH_PRE133-HOLDING" || len(tp.Commit) != 40 ||
		len(tp.Divergences) != 1 || tp.Divergences[0].Addr != 41010 {
		t.Fatalf("third party: %+v", tp)
	}
}

// TestNoFlowIsNotEvidence: a map that reads load, inverter and grid as
// all zero while the PV strings produce kilowatts is not accepted — an
// all-zero balance proves nothing, and the PV power must go somewhere
// (dc_balance: PV ≈ inverter output + battery charging).
func TestNoFlowIsNotEvidence(t *testing.T) {
	r := reference()
	for _, c := range Candidates("H1-5.0-E") {
		if Classifiable(LiveChecks(FromMap(c.Map, r.get), nil)) {
			t.Errorf("%s classifies on KH-shaped data", c.Key)
		}
	}
	// the real probe: 4726 W PV = 4398 W out + 28 W charging + ~300 W losses
	p := probeRegs(t)
	m, _ := fmdata.Load().Map("KH_PRE133-HOLDING")
	for _, ch := range LiveChecks(FromMap(m, p.get), nil) {
		if ch.Name == "dc_balance" && ch.Status != StatusPass {
			t.Fatalf("dc_balance on the probe: %s %s", ch.Status, ch.Detail)
		}
	}
}
