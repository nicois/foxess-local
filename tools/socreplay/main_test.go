package main

import (
	"math"
	"testing"
	"time"
)

const synthCapWh = 42000

// synth is a 42 kWh pack cycling at constant power, sampled every 5 s,
// reported through bms (the true SoC → the whole percent shown). With
// snapEvery > 0 it also yields the BMS's 0.1 % SoC at that interval; the
// inverter's power reads overRead W more discharge than flows.
func synth(bms func(float64) int, snapEvery time.Duration, overRead int) ([]row, []anchor) {
	var (
		rows    []row
		anchors []anchor
		soc     = 60.3
		t       = time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
		next    = t
	)
	phase := func(w int, d time.Duration) {
		for end := t.Add(d); t.Before(end); t = t.Add(5 * time.Second) {
			if snapEvery > 0 && !t.Before(next) {
				anchors = append(anchors, anchor{TS: t.Add(time.Second), Deci: int(math.Floor(soc * deciPerPct))})
				next = next.Add(snapEvery)
			}
			read := w
			if w < 0 {
				read -= overRead
			}
			rows = append(rows, row{TS: t, Soc: bms(soc), Batt: read})
			soc += float64(w) * 5 / 3600 / synthCapWh * 100
		}
	}
	for range 3 {
		phase(-840, 6*time.Hour) // −12 %
		phase(1680, 3*time.Hour) // +12 %
	}
	return rows, anchors
}

func groupMeans(res result) (contPinned, contJump, revPinned, revJump float64) {
	var c, cj, r, rj []float64
	for i, t := range res.Ticks {
		if excluded(i, t) != "" {
			continue
		}
		if t.Reversal {
			r, rj = append(r, t.Pinned), append(rj, t.Jump)
		} else {
			c, cj = append(c, t.Pinned), append(cj, t.Jump)
		}
	}
	return mean(c), mean(cj), mean(r), mean(rj)
}

// deadBand is the KH10's reporting (profile §5 31024): the shown value
// moves when the true SoC reaches it — ceil while discharging, floor
// while charging.
func deadBandBMS() func(float64) int {
	shown := -1
	return func(s float64) int {
		switch {
		case shown < 0:
			shown = int(math.Round(s))
		case s <= float64(shown-1):
			shown = int(math.Ceil(s))
		case s >= float64(shown+1):
			shown = int(math.Floor(s))
		}
		return shown
	}
}

func noViolations(t *testing.T, res result) {
	t.Helper()
	if len(res.Violations) > 0 {
		t.Fatalf("violations: %v", res.Violations[:min(5, len(res.Violations))])
	}
}

// The model fits a dead-band BMS: no pinning, no jumps, and the energy
// per percent learnt.
func TestReplayDeadBandBMS(t *testing.T) {
	rows, _ := synth(deadBandBMS(), 0, 0)
	res := replay(rows, nil, synthCapWh, nil)
	noViolations(t, res)
	cp, cj, rp, rj := groupMeans(res)
	t.Logf("continuing pinned %.3f jump %.3f, reversal pinned %.3f jump %.3f", cp, cj, rp, rj)
	if cp > 0.02 || math.Abs(cj) > 0.02 || rp > 0.02 || math.Abs(rj) > 0.02 {
		t.Error("the dead-band model should neither pin nor jump on a dead-band BMS")
	}
	if l := res.Learned.Discharging; !l.Ready() || math.Abs(l.WhPerPct-420) > 5 {
		t.Errorf("learnt discharging %+v, want ~420 Wh/%%", l)
	}
}

// A rounding BMS does not fit the model: a constant half-percent offset
// is invisible on continuing ticks, but each reversal jumps.
func TestReplayRoundingBMS(t *testing.T) {
	rows, _ := synth(func(s float64) int { return int(math.Round(s)) }, 0, 0)
	res := replay(rows, nil, synthCapWh, nil)
	noViolations(t, res)
	cp, cj, rp, rj := groupMeans(res)
	t.Logf("continuing pinned %.3f jump %.3f, reversal pinned %.3f jump %.3f", cp, cj, rp, rj)
	if cp > 0.02 || math.Abs(cj) > 0.02 {
		t.Errorf("continuing: pinned %.3f jump %.3f", cp, cj)
	}
	if rp+math.Abs(rj) < 0.4 {
		t.Errorf("reversals: pinned %.3f jump %.3f, want the mismatch to show", rp, rj)
	}
}

// A capacity set 20 % low makes the estimate run fast: it waits at the
// band's edge until learnt.
func TestReplayMiscalibrated(t *testing.T) {
	rows, _ := synth(deadBandBMS(), 0, 0)
	res := replay(rows, nil, 35000, nil)
	noViolations(t, res)
	if early := res.Ticks[1].Pinned; early < 0.1 {
		t.Errorf("first ticks pinned %.3f: a fast estimate waits at the edge", early)
	}
	if l := res.Learned.Discharging; math.Abs(l.WhPerPct-420) > 5 {
		t.Errorf("learnt discharging %.1f, want ~420 (the real energy per percent)", l.WhPerPct)
	}
}

// Snapshots anchor the estimate: ticks in between are not held to the
// band, and the drift at each snapshot is the integration's error —
// small with an accurate power reading, larger with the KH10's low-power
// over-read.
func TestReplayAnchored(t *testing.T) {
	rows, anchors := synth(deadBandBMS(), 5*time.Minute, 0)
	res := replay(rows, anchors, synthCapWh, nil)
	noViolations(t, res)
	if len(res.Drift) < len(anchors)-2 {
		t.Fatalf("%d drifts for %d snapshots", len(res.Drift), len(anchors))
	}
	for _, tk := range res.Ticks[1:] {
		if !tk.Anchored {
			t.Fatalf("tick %+v not anchored", tk)
		}
	}
	if m := maxAbs(res.Drift); m > 0.11 { // the 0.1 % snapshot resolution
		t.Errorf("max drift %.3f with an accurate power reading", m)
	}
	rows, anchors = synth(deadBandBMS(), 5*time.Minute, 250)
	res = replay(rows, anchors, synthCapWh, nil)
	noViolations(t, res)
	if d := mean(res.Drift); d > -0.02 { // 250 W × 5 min ≈ 0.05 % too low on discharge
		t.Errorf("mean drift %.3f: the over-read should show", d)
	}
}

func maxAbs(v []float64) float64 {
	m := 0.0
	for _, x := range v {
		m = max(m, math.Abs(x))
	}
	return m
}
