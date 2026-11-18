package compat

import "testing"

// TestScanPlan (F8, read-only verification): the scan reads the identity
// blocks first, then every register any candidate map names, in blocks of
// at most 125 registers, never an invalid address; a read appears once
// even when two maps share it.
func TestScanPlan(t *testing.T) {
	plan := ScanPlan("KH10")
	if len(plan) < 3 {
		t.Fatalf("plan %+v", plan)
	}
	if plan[0].Addr != 30000 || plan[0].Count != 15 || plan[1].Addr != 30016 || plan[1].Count != 3 {
		t.Fatalf("identity blocks first: %+v", plan[:2])
	}
	checkBlocks(t, plan)
	if !covers(plan, fnHolding, 31002) {
		t.Fatal("the KH map's live registers (PV1 power, 31002) are not in the plan")
	}
	for _, c := range Candidates("KH10") {
		checkNoInvalid(t, plan, c)
	}
}

// checkBlocks: every block is a valid read, and none repeats.
func checkBlocks(t *testing.T, plan []ScanBlock) {
	t.Helper()
	seen := map[[3]int]bool{}
	for _, b := range plan {
		if b.Count < 1 || b.Count > maxReadCount || (b.Fn != fnHolding && b.Fn != fnInput) {
			t.Fatalf("bad block %+v", b)
		}
		k := [3]int{b.Fn, b.Addr, b.Count}
		if seen[k] {
			t.Fatalf("duplicate block %+v", b)
		}
		seen[k] = true
	}
}

// covers reports whether a block of the plan reads addr with fn.
func covers(plan []ScanBlock, fn, addr int) bool {
	for _, b := range plan {
		if b.Fn == fn && b.Addr <= addr && addr < b.Addr+b.Count {
			return true
		}
	}
	return false
}

// checkNoInvalid: no block of a map reads an address it marks invalid.
func checkNoInvalid(t *testing.T, plan []ScanBlock, c Candidate) {
	t.Helper()
	for _, b := range plan {
		if b.Map != c.Key || b.Fn != c.Fn {
			continue
		}
		for a := b.Addr; a < b.Addr+b.Count; a++ {
			if inAddrRanges(a, c.Invalid) {
				t.Fatalf("%s: block %+v reads invalid %d", c.Key, b, a)
			}
		}
	}
}

// TestScanPlanUnknownModel: a model no map covers still gets the
// identity blocks (they name the model for the report).
func TestScanPlanUnknownModel(t *testing.T) {
	plan := ScanPlan("NOT-A-FOXESS")
	if len(plan) != 2 {
		t.Fatalf("plan %+v", plan)
	}
}

func inAddrRanges(a int, rs [][2]int) bool {
	for _, r := range rs {
		if a >= r[0] && a <= r[1] {
			return true
		}
	}
	return false
}
