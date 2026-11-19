package socest

import (
	"math"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)

const capWh = 41930

func near(t *testing.T, what string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 0.005 {
		t.Fatalf("%s = %.4f, want %.4f", what, got, want)
	}
}

// run feeds readings every 5 s for d at a constant battery power and
// reported SoC; it returns the last estimate.
func run(e *Estimator, from time.Time, d time.Duration, raw, battW int) float64 {
	var v float64
	for at := time.Duration(0); at <= d; at += 5 * time.Second {
		v = e.Observe(from.Add(at), raw, battW, capWh)
	}
	return v
}

// TestIntegratesBetweenTicks (owner, 2026-11-20): between whole-percent
// ticks the estimate moves with the battery power (+ = charging).
func TestIntegratesBetweenTicks(t *testing.T) {
	var e Estimator
	near(t, "first", e.Observe(t0, 42, -500, capWh), 42)
	// 10 min at 500 W out of 41 930 Wh: 500/6/41930 = 0.1987 %
	near(t, "discharging", run(&e, t0, 10*time.Minute, 42, -500), 42-500.0/6/capWh*100)
	var c Estimator
	near(t, "charging", run(&c, t0, 10*time.Minute, 42, 500), 42+500.0/6/capWh*100)
}

// TestTrapezoid: a power step is integrated as the mean of the two
// readings around it.
func TestTrapezoid(t *testing.T) {
	var e Estimator
	e.Observe(t0, 50, 0, capWh)
	got := e.Observe(t0.Add(time.Minute), 50, -4193, capWh)
	near(t, "one minute ramping 0 → 4193 W", got, 50-2096.5/60/capWh*100)
}

// TestDeadBand (profile §5 31024, 2026-11-20): the inverter's SoC
// trails the BMS's with a 1 % dead band — it changes when the BMS SoC
// reaches the shown value (ceil while discharging, floor while
// charging). So at a change of the reported SoC the estimate is that
// value, and in between it stays within 1 % of it.
func TestDeadBand(t *testing.T) {
	var e Estimator
	near(t, "first reading", e.Observe(t0, 42, -500, capWh), 42)
	near(t, "three hours at 500 W stop 1 % below", run(&e, t0, 3*time.Hour, 42, -500), 41)
	near(t, "a tick sets the estimate", e.Observe(t0.Add(3*time.Hour+5*time.Second), 41, -500, capWh), 41)
	near(t, "then integrates", e.Observe(t0.Add(3*time.Hour+10*time.Second), 41, -500, capWh), 41-500.0/720/capWh*100)
	var u Estimator
	run(&u, t0, time.Minute, 42, 0)
	near(t, "a jump down", u.Observe(t0.Add(time.Minute+5*time.Second), 40, 0, capWh), 40)
	near(t, "a jump up", u.Observe(t0.Add(time.Minute+10*time.Second), 45, 0, capWh), 45)
	var c Estimator
	near(t, "charging stops 1 % above", run(&c, t0, 3*time.Hour, 42, 500), 43)
	var f Estimator
	near(t, "never above 100", run(&f, t0, time.Hour, 100, 5000), 100)
	var z Estimator
	near(t, "never below 0", run(&z, t0, time.Hour, 0, -5000), 0)
}

// TestAnchor (profile §4 1908, 2026-11-20): the snapshot's BMS SoC (0.1 %,
// about every 5 minutes) sets the estimate; between snapshots it
// integrates the battery power unscaled (the learnt factor mixes the
// over-read at low power with the true scale at high power) and is not
// held to the reported SoC, which trails the BMS.
func TestAnchor(t *testing.T) {
	var e Estimator
	e.Restore(Learned{Discharging: Dir{WhPerPct: capWh / 100 * 1.4, N: minObs}}) // a factor of 0.71
	e.Observe(t0, 68, -5000, capWh)
	e.Anchor(t0.Add(2*time.Second), 669)
	got := e.Observe(t0.Add(5*time.Second), 68, -5000, capWh)
	near(t, "anchored, then 5 s at 5 kW", got, 66.9-5000.0/720/capWh*100)
	got = run(&e, t0.Add(5*time.Second), 5*time.Minute, 68, -5000)
	near(t, "5 more minutes, unscaled, below the reported 68 − 1", got, 66.9-5000.0*(5*60+5)/3600/capWh*100)
}

// TestAnchorFirst: a snapshot before the first reading sets the first
// estimate.
func TestAnchorFirst(t *testing.T) {
	var e Estimator
	e.Anchor(t0, 413)
	near(t, "first reading after a snapshot", e.Observe(t0.Add(3*time.Second), 42, 0, capWh), 41.3)
}

// TestAnchorExpires: without a snapshot for anchorMaxAge the estimate
// falls back to the reported SoC's dead band and ticks.
func TestAnchorExpires(t *testing.T) {
	var e Estimator
	e.Observe(t0, 42, 0, capWh)
	e.Anchor(t0, 400) // 40.0 against a reported 42: outside its band
	near(t, "fresh", e.Observe(t0.Add(5*time.Second), 42, 0, capWh), 40)
	got := run(&e, t0.Add(10*time.Second), anchorMaxAge, 42, 0)
	near(t, "stale: back within the band", got, 41)
	near(t, "a tick sets it", e.Observe(t0.Add(anchorMaxAge+time.Minute), 43, 0, capWh), 43)
}

// TestGapAndClock: a gap in the readings (a lost poll, a restart of the
// session) or a clock that goes back is not integrated across.
func TestGapAndClock(t *testing.T) {
	var e Estimator
	e.Observe(t0, 42, -5000, capWh)
	near(t, "after a gap", e.Observe(t0.Add(10*time.Minute), 42, -5000, capWh), 42)
	near(t, "clock back", e.Observe(t0.Add(5*time.Minute), 42, -5000, capWh), 42)
}

// TestNoCapacity: without the capacity the estimate is the reported SoC.
func TestNoCapacity(t *testing.T) {
	var e Estimator
	for i := range 100 {
		if got := e.Observe(t0.Add(time.Duration(i)*5*time.Second), 42, -5000, 0); got != 42 {
			t.Fatalf("reading %d: %v", i, got)
		}
	}
	if _, ok := e.Latest(); ok {
		t.Fatal("Latest without a capacity")
	}
}

// TestLatest: the newest reading and its estimate, for the API.
func TestLatest(t *testing.T) {
	var e Estimator
	if _, ok := e.Latest(); ok {
		t.Fatal("Latest before a reading")
	}
	run(&e, t0, 10*time.Minute, 42, -500)
	r, ok := e.Latest()
	if !ok || r.Raw != 42 || !r.At.Equal(t0.Add(10*time.Minute)) {
		t.Fatalf("%+v %v", r, ok)
	}
	near(t, "Latest.Pct", r.Pct, 42-500.0/6/capWh*100)
}

// battery simulates a pack whose real energy per percent is whPerPct
// (the nominal capacity says capWh/100): the true SoC follows the power,
// and the inverter reports it as this one does — the shown value moves
// when the true SoC reaches it (ceil while discharging, floor while
// charging; profile §5 31024). It feeds e every 5 s and returns the
// largest |estimate − true| over the last hour.
func battery(e *Estimator, from time.Time, d time.Duration, soc0, whPerPct float64, battW func(h float64) int) float64 {
	soc, worst := soc0, 0.0
	shown := int(math.Round(soc0))
	var prevW int
	for at := time.Duration(0); at <= d; at += 5 * time.Second {
		w := battW(at.Hours())
		if at > 0 {
			soc += float64(prevW+w) / 2 * (5.0 / 3600) / whPerPct
		}
		prevW = w
		switch {
		case soc <= float64(shown-1):
			shown = int(math.Ceil(soc))
		case soc >= float64(shown+1):
			shown = int(math.Floor(soc))
		}
		est := e.Observe(from.Add(at), shown, w, capWh)
		if d-at <= time.Hour {
			worst = max(worst, math.Abs(est-soc))
		}
	}
	return worst
}

// TestLearnsScale (owner, 2026-11-20): consecutive one-step ticks in one
// direction are exactly 1 % apart, whatever the inverter's rounding; the
// energy between them measures the energy per percent (a pack at 90 %
// health, or a capacity set wrong). Once learnt, the estimate follows
// the true SoC closely between ticks.
func TestLearnsScale(t *testing.T) {
	const healthy = capWh / 100 * 0.9 // 377.37 Wh per %
	var e Estimator
	worst := battery(&e, t0, 10*time.Hour, 60.2, healthy, func(float64) int { return -500 })
	l := e.Learned()
	if l.Discharging.N < minObs || math.Abs(l.Discharging.WhPerPct-healthy)/healthy > 0.01 {
		t.Fatalf("learnt %+v, want about %.1f Wh/%%", l.Discharging, healthy)
	}
	if l.Charging.N != 0 {
		t.Fatalf("charging learnt from a discharge: %+v", l.Charging)
	}
	if worst > 0.03 {
		t.Fatalf("estimate off by %.3f %% after learning", worst)
	}
	// unlearnt, the same discharge is off by up to 0.1 % before each tick
	var u Estimator
	if w := battery(&u, t0, 2*time.Hour, 60.2, healthy, func(float64) int { return -500 }); w < 0.08 {
		t.Fatalf("unlearnt error %.3f: the test does not separate", w)
	}
}

// TestLearnsPerDirection: charging learns its own energy per percent
// (losses differ by direction).
func TestLearnsPerDirection(t *testing.T) {
	var e Estimator
	battery(&e, t0, 10*time.Hour, 20.2, capWh/100*1.05, func(float64) int { return 600 })
	l := e.Learned()
	if l.Charging.N < minObs || math.Abs(l.Charging.WhPerPct-capWh/100*1.05) > 4 || l.Discharging.N != 0 {
		t.Fatalf("learnt %+v", l)
	}
}

// step is one reading after dt.
type step struct {
	dt  time.Duration
	raw int
	w   int
}

// interval is a clean one-hour interval at 419 W (about 1 %): 42 → 41,
// an hour at 41, → 40 — two single steps down, energy flowing out.
// mut alters reading i.
func interval(mut func(i int, s *step)) []step {
	out := make([]step, 0, 722)
	for i := range 722 {
		s := step{5 * time.Second, 41, -419}
		switch i {
		case 0:
			s.raw = 42
		case 721:
			s.raw = 40
		}
		mut(i, &s)
		out = append(out, s)
	}
	return out
}

func feedSteps(steps []step) Learned {
	var e Estimator
	at := t0
	for _, s := range steps {
		at = at.Add(s.dt)
		e.Observe(at, s.raw, s.w, capWh)
	}
	return e.Learned()
}

// rejectCases each spoil the clean interval one way.
var rejectCases = map[string]func(i int, s *step){
	"gap": func(i int, s *step) {
		if i == 300 {
			s.dt = 5 * time.Minute
		}
	},
	"clock back": func(i int, s *step) {
		if i == 300 {
			s.dt = -time.Minute
		}
	},
	"double step": func(i int, s *step) {
		if i == 721 {
			s.raw = 39
		}
	},
	"reversal": func(i int, s *step) {
		if i == 721 {
			s.raw = 42
		}
	},
	"two-way flow": func(i int, s *step) { // net 419 W out, in 5-min blocks
		if i > 0 && i/60%2 == 0 {
			s.w = 600
		} else if i > 0 {
			s.w = -1438
		}
	},
	"against the flow": func(_ int, s *step) { s.w = 419 },
	"recalibration jump": func(i int, s *step) { // 41 for five minutes
		if i > 60 {
			s.raw = 40
		}
	},
}

// TestLearnRejects: an interval is learnt from only when it is clean —
// no gap, two single steps in the direction the energy flowed, mostly
// one-way flow, and a plausible size (not a BMS recalibration jump).
func TestLearnRejects(t *testing.T) {
	if l := feedSteps(interval(func(int, *step) {})); l.Discharging.N != 1 || math.Abs(l.Discharging.WhPerPct-419) > 1 {
		t.Fatalf("the clean interval: %+v", l)
	}
	for name, mut := range rejectCases {
		if l := feedSteps(interval(mut)); l.Discharging.N != 0 || l.Charging.N != 0 {
			t.Errorf("%s: learnt %+v", name, l)
		}
	}
}

// TestRestoreAndBounds: a restored measurement applies at once (after a
// restart); fewer than minObs samples apply nothing; the factor is held
// within [kMin, kMax].
func TestRestoreAndBounds(t *testing.T) {
	drop := func(l Learned) float64 {
		var e Estimator
		e.Restore(l)
		return 42 - run(&e, t0, 10*time.Minute, 42, -500)
	}
	nominal := 500.0 / 6 / capWh * 100
	near(t, "learnt 90 %", drop(Learned{Discharging: Dir{WhPerPct: capWh / 100 * 0.9, N: minObs}}), nominal/0.9)
	near(t, "too few samples", drop(Learned{Discharging: Dir{WhPerPct: capWh / 100 * 0.9, N: minObs - 1}}), nominal)
	near(t, "charging only", drop(Learned{Charging: Dir{WhPerPct: capWh / 100 * 0.9, N: minObs}}), nominal)
	near(t, "held at kMax", drop(Learned{Discharging: Dir{WhPerPct: capWh / 100 / 2, N: minObs}}), nominal*kMax)
	near(t, "held at kMin", drop(Learned{Discharging: Dir{WhPerPct: capWh / 100 * 2, N: minObs}}), nominal*kMin)
}
