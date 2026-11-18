package compat

import (
	"foxesslocal/internal/fmdata"
	"foxesslocal/internal/identity"
)

// ScanBlock is one read of a device scan: the map it serves ("identity"
// for the identity blocks), the function (3 holding / 4 input) and the
// register block.
type ScanBlock struct {
	Map   string `json:"map"`
	Fn    int    `json:"fn"`
	Addr  int    `json:"addr"`
	Count int    `json:"count"`
}

// identityMap labels the identity blocks in a scan.
const identityMap = "identity"

// ScanPlan is the read-only device scan for a model (F8): the identity
// blocks (model string, firmware versions), then every register any
// candidate foxess_modbus map names — each map with its own read
// function, never reading or spanning an address the map marks invalid,
// in blocks of at most 125 registers; a block two maps share is read
// once. A model no map covers gets the identity blocks only.
func ScanPlan(model string) []ScanBlock {
	plan := []ScanBlock{
		{Map: identityMap, Fn: fnHolding, Addr: identity.ModelAddr, Count: identity.ModelCount},
		{Map: identityMap, Fn: fnHolding, Addr: identity.VersionAddr, Count: identity.VersionCount},
	}
	seen := map[[3]int]bool{}
	for _, b := range plan {
		seen[[3]int{b.Fn, b.Addr, b.Count}] = true
	}
	for _, c := range Candidates(model) {
		for _, b := range fmdata.PlanReads(allAddrs(c.Map), c.Invalid, c.Individual, maxReadCount) {
			k := [3]int{c.Fn, b.Addr, b.Count}
			if seen[k] {
				continue
			}
			seen[k] = true
			plan = append(plan, ScanBlock{Map: c.Key, Fn: c.Fn, Addr: b.Addr, Count: b.Count})
		}
	}
	return plan
}

// allAddrs is every register address a map's entities name.
func allAddrs(m fmdata.Map) []int {
	var out []int
	for _, e := range m {
		out = append(out, e.Addresses...)
	}
	return out
}
