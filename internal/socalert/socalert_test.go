package socalert

import (
	"math"
	"math/rand/v2"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)

var params = Params{Threshold: 20, Horizon: 2 * time.Hour, Window: 30 * time.Minute, P: 0.05}

// feed runs SoC samples every 5 s from t0 for d, soc(t) given in hours:
// rounded as the inverter reports it, and as the estimate (exact: what
// socest tracks between ticks). It returns every alert.
func feed(w *Watch, p Params, d time.Duration, soc func(h float64) float64) []Alert {
	var out []Alert
	for at := time.Duration(0); at <= d; at += 5 * time.Second {
		est := soc(at.Hours())
		out = append(out, w.Observe(t0.Add(at), int(math.Round(est)), est, p, Session{})...)
	}
	return out
}

// TestForecastSlowDischarge (owner, 2026-11-20): a night's discharge of
// about 1 %/h moves the reported SoC once an hour, so a 30 min window of
// whole percents is flat or one step; the estimate between the ticks
// forecasts it about two hours ahead. (2026-11-20 on the KH10: 48 % at
// 20:30, 40 % at 03:40, and only battery-low was sent.)
func TestForecastSlowDischarge(t *testing.T) {
	p := params
	p.Threshold = 40
	var w Watch
	// 48 % falling 1.1 %/h: 40 % after 7 h 16 min
	got := feed(&w, p, 8*time.Hour, func(h float64) float64 { return 48 - 1.1*h })
	if len(got) != 2 || got[0].Kind != KindForecastLow || got[1].Kind != KindLow {
		t.Fatalf("alerts %+v", got)
	}
	// the 2 h forecast reaches 40 % from about 42.2 %, never from a step
	// read as a steep fall (the raw-only fit warned at 45 %, "2.8 %/h")
	if !strings.Contains(got[0].Detail, "Battery 42 %") || !strings.Contains(got[0].Detail, "falling 1.1 %/h") {
		t.Fatalf("detail %q", got[0].Detail)
	}
}

// TestForecastAlerts (owner, 2026-11-18): a discharge that will reach the
// threshold within the horizon alerts once, with the expected time.
func TestForecastAlerts(t *testing.T) {
	var w Watch
	// 60 % falling 15 %/h: 20 % at 02:40; the 2 h forecast reaches it
	// after about 40 min, so an hour of readings must alert, once
	got := feed(&w, params, time.Hour, func(h float64) float64 { return 60 - 15*h })
	if len(got) != 1 || got[0].Kind != KindForecastLow {
		t.Fatalf("alerts %+v", got)
	}
	// whole-percent readings put the fitted crossing within a minute of
	// 02:40
	if !strings.Contains(got[0].Detail, "falling 15 %/h") || !strings.Contains(got[0].Detail, "95 % confident") ||
		(!strings.Contains(got[0].Detail, "02:39") && !strings.Contains(got[0].Detail, "02:40")) {
		t.Fatalf("detail %q", got[0].Detail)
	}
}

// TestForecastQuiet: a slow discharge that will not reach the threshold
// within the horizon, a rising charge, and too little data never alert.
func TestForecastQuiet(t *testing.T) {
	for name, soc := range map[string]func(h float64) float64{
		"slow":   func(h float64) float64 { return 60 - 5*h }, // 20 % in 8 h
		"rising": func(h float64) float64 { return 40 + 10*h },
		"flat":   func(float64) float64 { return 25 },
	} {
		var w Watch
		if got := feed(&w, params, 2*time.Hour, soc); len(got) != 0 {
			t.Errorf("%s: %+v", name, got)
		}
	}
	var w Watch // a steep fall, but only 10 min of the 30 min window
	if got := feed(&w, params, 10*time.Minute, func(h float64) float64 { return 30 - 30*h }); len(got) != 0 {
		t.Errorf("too little data: %+v", got)
	}
}

// TestForecastNoise: noisy readings around a level do not alert, while
// the same noise on a clear fall still does.
func TestForecastNoise(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	var w Watch
	// a level well clear of the alert level, ±4 % of noise
	if got := feed(&w, params, 2*time.Hour, func(float64) float64 { return 45 + 4*r.NormFloat64() }); len(got) != 0 {
		t.Errorf("noise alone: %+v", got)
	}
	var w2 Watch
	if got := feed(&w2, params, time.Hour, func(h float64) float64 { return 50 - 20*h + 2*r.NormFloat64() }); len(got) != 1 {
		t.Errorf("a clear fall under noise: %+v", got)
	}
}

// TestLowAndRearm: at or below the threshold, one battery-low (and no
// forecast after it); both re-arm only 5 points above the threshold.
func TestLowAndRearm(t *testing.T) {
	var w Watch
	p := params
	var got []Alert
	for i, soc := range []int{22, 21, 20, 19, 20, 21, 24, 25, 26, 20} {
		got = append(got, w.Observe(t0.Add(time.Duration(i)*time.Minute), soc, float64(soc), p, Session{})...)
	}
	if len(got) != 2 || got[0].Kind != KindLow || got[1].Kind != KindLow {
		t.Fatalf("alerts %+v", got)
	}
	if !strings.Contains(got[0].Detail, "20 %") {
		t.Fatalf("detail %q", got[0].Detail)
	}
}

// TestForecastRearm: after a forecast alert, a discharge that goes on
// stays quiet; charging back up re-arms it, and the next fall alerts
// again.
func TestForecastRearm(t *testing.T) {
	var w Watch
	soc := func(h float64) float64 {
		switch {
		case h < 1:
			return 60 - 15*h // falls: alert
		case h < 2:
			return 45 + 20*(h-1) // charges back to 65
		}
		return 65 - 15*(h-2) // falls again: alert again
	}
	// the second fall forecasts once the window has slid past the
	// charging (about 3 h 15 min in)
	if got := feed(&w, params, 4*time.Hour, soc); len(got) != 2 {
		t.Fatalf("alerts %+v", got)
	}
}

// TestOffAndClockJump: threshold 0 turns the alerts off; a clock that
// jumps back (no RTC, NTP syncing) starts the window afresh.
func TestOffAndClockJump(t *testing.T) {
	var w Watch
	off := params
	off.Threshold = 0
	if got := feed(&w, off, time.Hour, func(h float64) float64 { return 15 - 10*h }); len(got) != 0 {
		t.Fatalf("off: %+v", got)
	}
	var w2 Watch
	feed(&w2, params, 20*time.Minute, func(h float64) float64 { return 60 - 15*h })
	w2.Observe(t0.Add(-time.Hour), 55, 55, params, Session{})
	if n := len(w2.samples); n != 1 {
		t.Fatalf("after a backward jump %d samples, want 1", n)
	}
}

// TestTQuantile: the t quantile used for the bound is close to the
// tabulated values.
func TestTQuantile(t *testing.T) {
	for _, c := range []struct {
		p    float64
		df   int
		want float64
	}{{0.05, 10, 1.812}, {0.05, 30, 1.697}, {0.05, 1000, 1.646}, {0.01, 30, 2.457}} {
		if got := tQuantile(c.p, c.df); math.Abs(got-c.want) > 0.01 {
			t.Errorf("t(%v, %d) = %.3f, want %.3f", c.p, c.df, got, c.want)
		}
	}
}

// feedSession is feed with a force-discharge session given per hour.
func feedSession(w *Watch, p Params, d time.Duration, soc func(h float64) float64, sess func(h float64) Session) []Alert {
	var out []Alert
	for at := time.Duration(0); at <= d; at += 5 * time.Second {
		est := soc(at.Hours())
		out = append(out, w.Observe(t0.Add(at), int(math.Round(est)), est, p, sess(at.Hours()))...)
	}
	return out
}

// sessionUntil is a force discharge ending at hour end, with cut-off
// cut %, the battery falling forced %/h in it and the house using house
// %/h (+ = surplus) throughout; after it the battery follows the house.
func sessionUntil(end, cut, forced, house float64) func(h float64) Session {
	return func(h float64) Session {
		if h >= end {
			return Session{RatesKnown: true, BattRate: house, HouseRate: house}
		}
		return Session{
			Forced: true, Left: time.Duration((end - h) * float64(time.Hour)), CutoffPct: cut,
			RatesKnown: true, BattRate: forced, HouseRate: house,
		}
	}
}

// TestForecastForceDischargeEnds (owner, 2026-11-20): a force discharge
// falls fast, but it ends: past its expected end the battery falls only
// at the house's own usage, so a session that ends well above the alert
// level does not warn — during it, or after, while the window still
// holds its readings.
func TestForecastForceDischargeEnds(t *testing.T) {
	var w Watch
	// 70 % falling 20 %/h until 1 h (50 %), then 1 %/h
	soc := func(h float64) float64 {
		if h < 1 {
			return 70 - 20*h
		}
		return 50 - (h - 1)
	}
	if got := feedSession(&w, params, 2*time.Hour, soc, sessionUntil(1, 10, -20, -1)); len(got) != 0 {
		t.Fatalf("alerts %+v", got)
	}
}

// TestForecastForceDischargeCutoff: the inverter stops forcing at the
// window's cut-off SoC, so a cut-off above the alert level holds the
// forecast there.
func TestForecastForceDischargeCutoff(t *testing.T) {
	var w Watch
	p := params
	p.Threshold = 40
	// 70 % falling 30 %/h for 40 min (to the cut-off, 50 %); the window
	// runs 3 h
	if got := feedSession(&w, p, 40*time.Minute, func(h float64) float64 { return 70 - 30*h }, sessionUntil(3, 50, -30, 0)); len(got) != 0 {
		t.Fatalf("alerts %+v", got)
	}
}

// TestForecastForceDischargeAlerts: a session that will itself take the
// battery to the alert level warns within its first minute (the measured
// rate needs no line), naming the session; one that ends just above the
// level warns when the house's own usage carries on down.
func TestForecastForceDischargeAlerts(t *testing.T) {
	var w Watch
	// 60 % falling 20 %/h, the window runs 3 h, cut-off 10 %: 20 % at 2 h
	got := feedSession(&w, params, time.Minute, func(h float64) float64 { return 60 - 20*h }, sessionUntil(3, 10, -20, -1))
	if len(got) != 1 || got[0].Kind != KindForecastLow || !strings.Contains(got[0].Detail, "force discharging at 20 %/h") {
		t.Fatalf("in-session: alerts %+v", got)
	}
	var w2 Watch
	// 50 % falling 20 %/h until 1 h (30 %), then the house uses 15 %/h
	got = feedSession(&w2, params, 55*time.Minute, func(h float64) float64 { return 50 - 20*h }, sessionUntil(1, 10, -20, -15))
	if len(got) != 1 || got[0].Kind != KindForecastLow || !strings.Contains(got[0].Detail, "then the house uses 15 %/h") {
		t.Fatalf("after the session: alerts %+v", got)
	}
}
