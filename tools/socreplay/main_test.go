package main

import (
	"math"
	"testing"
	"time"
)

// synth is a 42 kWh pack cycling at constant power, sampled every 5 s,
// reported through bms (true SoC → whole percent).
func synth(bms func(float64) int) []row {
	const capWh = 42000
	var (
		rows []row
		soc  = 60.3
		t0   = time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
		t    = t0
	)
	phase := func(w int, d time.Duration) {
		for end := t.Add(d); t.Before(end); t = t.Add(5 * time.Second) {
			rows = append(rows, row{TS: t, Soc: bms(soc), Batt: w})
			soc += float64(w) * 5 / 3600 / capWh * 100
		}
	}
	for range 3 {
		phase(-840, 6*time.Hour) // −12 %
		phase(1680, 3*time.Hour) // +12 %
	}
	return rows
}

func groupMeans(res result) (contPinned, revPinned, revJump float64) {
	var c, r, rj []float64
	for i, t := range res.Ticks {
		if i == 0 || t.Dir == 0 || t.Gap {
			continue
		}
		if t.Reversal {
			r, rj = append(r, t.Pinned), append(rj, t.Jump)
		} else {
			c = append(c, t.Pinned)
		}
	}
	return mean(c), mean(r), mean(rj)
}

func TestReplayRoundingBMS(t *testing.T) {
	res := replay(synth(func(s float64) int { return int(math.Round(s)) }), 42000, nil)
	if len(res.Violations) > 0 {
		t.Fatalf("violations: %v", res.Violations[:min(5, len(res.Violations))])
	}
	cont, rev, revJump := groupMeans(res)
	t.Logf("continuing pinned %.3f, reversal pinned %.3f jump %.3f", cont, rev, revJump)
	if cont > 0.05 || rev > 0.05 || math.Abs(revJump) > 0.05 {
		t.Errorf("a rounding BMS on a calibrated estimate should neither pin nor jump")
	}
	if l := res.Learned.Discharging; !l.Ready() || math.Abs(l.WhPerPct-420) > 5 {
		t.Errorf("learnt discharging %+v, want ~420 Wh/%%", l)
	}
}

// A truncating BMS is a constant half-percent offset: it fits inside
// the bucket in both directions and cannot be seen from the ticks.
func TestReplayTruncatingBMSInvisible(t *testing.T) {
	res := replay(synth(func(s float64) int { return int(math.Floor(s)) }), 42000, nil)
	if len(res.Violations) > 0 {
		t.Fatalf("violations: %v", res.Violations[:min(5, len(res.Violations))])
	}
	cont, rev, revJump := groupMeans(res)
	if cont > 0.05 || rev > 0.05 || math.Abs(revJump) > 0.05 {
		t.Errorf("pinned %.3f / %.3f, jump %.3f: truncation should look like rounding", cont, rev, revJump)
	}
}

// hysteresis reports a new value only once the true SoC is more than
// 0.5 + h from the reported one.
func hysteresis(h float64) func(float64) int {
	rep := -1
	return func(s float64) int {
		if rep < 0 || math.Abs(s-float64(rep)) > 0.5+h {
			rep = int(math.Round(s))
		}
		return rep
	}
}

func TestReplayHysteresisBMS(t *testing.T) {
	res := replay(synth(hysteresis(0.3)), 42000, nil)
	if len(res.Violations) > 0 {
		t.Fatalf("violations: %v", res.Violations[:min(5, len(res.Violations))])
	}
	cont, rev, revJump := groupMeans(res)
	t.Logf("continuing pinned %.3f, reversal pinned %.3f jump %.3f", cont, rev, revJump)
	if cont > 0.05 {
		t.Errorf("continuing ticks pinned %.3f: hysteresis shows at reversals", cont)
	}
	if rev+math.Abs(revJump) < 0.3 {
		t.Errorf("reversal pinned %.3f + |jump| %.3f, want ~2h = 0.6", rev, revJump)
	}
}

func TestReplayMiscalibrated(t *testing.T) {
	// capacity set 20 % too low: the estimate runs fast until learnt
	res := replay(synth(func(s float64) int { return int(math.Round(s)) }), 35000, nil)
	if len(res.Violations) > 0 {
		t.Fatalf("violations: %v", res.Violations[:min(5, len(res.Violations))])
	}
	early := res.Ticks[1].Pinned
	if early < 0.1 {
		t.Errorf("first ticks pinned %.3f: a fast estimate waits at the edge", early)
	}
	if l := res.Learned.Discharging; math.Abs(l.WhPerPct-420) > 5 {
		t.Errorf("learnt discharging %.1f, want ~420 (the real energy per percent)", l.WhPerPct)
	}
}
