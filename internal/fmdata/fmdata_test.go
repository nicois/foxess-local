package fmdata

import "testing"

// TestSourceAttribution: the embedded data carries its provenance and
// licence (D-021: copied with attribution).
func TestSourceAttribution(t *testing.T) {
	d := Load()
	if d.Source.License != "MIT" || len(d.Source.Commit) != 40 || d.Source.Repo == "" || d.Source.Notice == "" {
		t.Fatalf("source block: %+v", d.Source)
	}
}

// TestMatchModel: profiles match in upstream's list order (the more
// specific pattern first: H3-…-Smart before H3, H1-…-G2 before H1).
func TestMatchModel(t *testing.T) {
	d := Load()
	for model, want := range map[string]string{
		"KH10":          "KH",
		"KH10.5":        "KH",
		"H3-10.0-SMART": "H3_SMART",
		"H3-10.0-E":     "H3",
		"H1-5.0-E-G2":   "H1_G2",
		"H1-5.0-E":      "H1",
		"AIO-H3-10.0":   "AIO-H3",
		"H3-Pro-15.0":   "H3_PRO",
	} {
		p, ok := d.MatchModel(model)
		if !ok || p.Model != want {
			t.Errorf("%q: %q %v, want %q", model, p.Model, ok, want)
		}
	}
	if p, ok := d.MatchModel("XYZ-1"); ok {
		t.Errorf("unknown model matched %+v", p)
	}
}

// TestKHPre133Map: the KH_PRE133 map is the layout the reference KH10
// answers (profile §5): PV1 voltage at 31000 (0.1 V), load power the
// 31053:31054 pair listed low word first.
func TestKHPre133Map(t *testing.T) {
	m, ok := Load().Map("KH_PRE133-HOLDING")
	if !ok {
		t.Fatal("KH_PRE133-HOLDING map missing")
	}
	e, ok := m.Entity("pv1_voltage")
	if !ok || len(e.Addresses) != 1 || e.Addresses[0] != 31000 || e.Scale == nil || *e.Scale != 0.1 {
		t.Fatalf("pv1_voltage: %+v", e)
	}
	e, ok = m.Entity("load_power")
	if !ok || len(e.Addresses) != 2 || e.Addresses[0] != 31054 || e.Addresses[1] != 31053 {
		t.Fatalf("load_power: %+v", e)
	}
}

// TestPlanReads: addresses become as few block reads as the rules allow
// — contiguous runs within maxGap are merged, a block never spans an
// invalid address, never exceeds maxCount, and every address inside an
// individual-read range is read on its own; invalid addresses are never
// read at all.
func TestPlanReads(t *testing.T) {
	addrs := []int{31000, 31001, 31002, 31006, 31009, 31054, 31053, 41000, 41007, 41008, 31060}
	invalid := [][2]int{{31055, 31999}}
	individual := [][2]int{{41000, 41999}}
	got := PlanReads(addrs, invalid, individual, 125)
	want := []Block{{31000, 10}, {31053, 2}, {41000, 1}, {41007, 1}, {41008, 1}}
	if len(got) != len(want) {
		t.Fatalf("plan %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("plan %v, want %v", got, want)
		}
	}
	// a long run is split at maxCount
	var long []int
	for a := 32000; a < 32200; a++ {
		long = append(long, a)
	}
	if p := PlanReads(long, nil, nil, 125); len(p) != 2 || p[0] != (Block{32000, 125}) || p[1] != (Block{32125, 75}) {
		t.Fatalf("long run: %v", p)
	}
}

// TestConnectionRanges: the special register ranges are imported per
// connection (KH: 31055-31999 invalid, 410xx read individually).
func TestConnectionRanges(t *testing.T) {
	p, _ := Load().MatchModel("KH10")
	c := p.Connections[0]
	if len(c.InvalidRanges) == 0 || len(c.IndividualRanges) != 1 || c.IndividualRanges[0] != [2]int{41000, 41999} {
		t.Fatalf("KH ranges: %+v / %+v", c.InvalidRanges, c.IndividualRanges)
	}
}
