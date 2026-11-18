package main

import (
	"slices"
	"testing"
	"time"

	"foxesslocal/internal/frame"
)

// TestEnergyWatch (foxess-control local backend, 2026-11-19): the
// lifetime counters as the integration's contract wants them — null
// until read, never decreasing; a lower reading is held until the next
// read confirms it (one bad read is not a reset), then reported as a
// reset of that field; the previous run's last reading seeds the
// baseline, so a reset across a restart is caught too.
func TestEnergyWatch(t *testing.T) {
	t0 := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	var w energyWatch
	if e := w.energy(); e.GridExportKWh != nil || e.AsOf != nil {
		t.Fatalf("before any read: %+v", e)
	}
	w.seed(frame.KHEnergy{FeedIn: 1000, PV: 5000}, t0)
	if e := w.energy(); e.GridExportKWh == nil || *e.GridExportKWh != 100 || e.Sources["grid_export_kwh"] != "register" {
		t.Fatalf("seeded: %+v", e)
	}
	if r := w.observe(frame.KHEnergy{FeedIn: 1012, PV: 5003}, t0.Add(time.Minute)); len(r) != 0 {
		t.Fatalf("a rise reported resets %v", r)
	}
	if e := w.energy(); *e.GridExportKWh != 101.2 || *e.PVKWh != 500.3 || !e.AsOf.Equal(t0.Add(time.Minute)) {
		t.Fatalf("after a rise: %+v", e)
	}
	// one low read (a glitch): held, not reported
	if r := w.observe(frame.KHEnergy{FeedIn: 3, PV: 5004}, t0.Add(2*time.Minute)); len(r) != 0 {
		t.Fatalf("a single low read reported %v", r)
	}
	if e := w.energy(); *e.GridExportKWh != 101.2 || *e.PVKWh != 500.4 {
		t.Fatalf("the glitch leaked: %+v", e)
	}
	// back up: the glitch is forgotten
	w.observe(frame.KHEnergy{FeedIn: 1013, PV: 5005}, t0.Add(3*time.Minute))
	// a real reset: two low reads in a row
	w.observe(frame.KHEnergy{FeedIn: 2, PV: 5006}, t0.Add(4*time.Minute))
	r := w.observe(frame.KHEnergy{FeedIn: 4, PV: 5007}, t0.Add(5*time.Minute))
	if !slices.Equal(r, []string{"grid_export_kwh"}) {
		t.Fatalf("resets %v", r)
	}
	if e := w.energy(); *e.GridExportKWh != 0.4 {
		t.Fatalf("after the reset: %+v", e)
	}
}
