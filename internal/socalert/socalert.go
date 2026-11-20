// Package socalert decides the battery alerts (owner, 2026-11-18):
//
//   - battery-low: the state of charge is at or below the threshold;
//   - battery-forecast-low: a least-squares line through the last window
//     of estimates (socest: the SoC between whole-percent ticks — D-028),
//     projected to now + horizon, has its one-sided upper
//     prediction bound (confidence 1 − p) at or below the threshold —
//     the charge will be there within the horizon, with that confidence.
//
// A force discharge (owner, 2026-11-20) falls fast but ends, so during
// one the forecast is not a line through the charge: the battery falls
// at its measured rate (the last few minutes' mean) until the discharge
// is expected to end — the window's end or the cut-off SoC, whichever
// comes first — and from there at the house's recent usage (its load
// less solar, the window's mean, whatever the mode), the bound coming
// from that usage's spread. After a session, its readings are left out
// of the line.
//
// Each fires once. battery-low re-arms when the charge recovers more
// than rearmMargin points above the threshold; the forecast re-arms when
// the charge recovers that far above where it was when the forecast
// fired (charging resumed). After battery-low no forecast follows until
// both re-arm. A threshold of 0 turns both off. A clock that jumps back (a
// board without a battery-backed clock, NTP syncing) restarts the window.
//
// The bound treats the readings as independent around the line; 5-second
// readings of a slowly varying charge are not quite, so the stated
// confidence is somewhat optimistic. The minimum span and sample count
// keep a short burst from forecasting.
package socalert

import (
	"fmt"
	"math"
	"strconv"
	"time"
)

// The alert kinds (the SystemEvent types the host publishes).
const (
	KindLow         = "battery-low"
	KindForecastLow = "battery-forecast-low"
)

const (
	// rearmMargin: both alerts re-arm once the charge is this far above
	// the threshold (a reading hovering at it never repeats them).
	rearmMargin = 5
	// minSamples and minSpan (of the window) gate the forecast.
	minSamples = 30
	minSpanDiv = 2
	// oneDecimal rounds the rate shown to 0.1 %/h.
	oneDecimal = 10
	// lineParams: a line's two fitted parameters (its degrees of freedom).
	lineParams = 2
	// percent: a fraction as %.
	percent = 100
)

// Params are the knobs (the alerts.* settings).
type Params struct {
	Threshold int // %; 0 = off
	Horizon   time.Duration
	Window    time.Duration
	P         float64
}

// Alert is one decision.
type Alert struct {
	Kind   string
	Detail string
}

type sample struct {
	at     time.Time
	soc    float64
	forced bool
	rates  bool    // batt and house are known (the capacity is set)
	batt   float64 // the battery's rate, %/h (+ = charging)
	house  float64 // solar less the house load, %/h of the capacity
}

// Session is the force discharge running now (Forced false: none) and
// this reading's rates.
type Session struct {
	Forced    bool
	Left      time.Duration // until the session's window ends
	CutoffPct float64       // the inverter stops forcing at this SoC
	// RatesKnown: BattRate and HouseRate are set (the battery capacity is
	// known); without them a session gets no forecast.
	RatesKnown bool
	BattRate   float64 // the battery power over the capacity, %/h (+ = charging)
	HouseRate  float64 // solar less the house load over the capacity, %/h
}

// forcedAvg is how far back the force discharge's rate is averaged;
// minRateSamples readings (30 s) are needed for it and for the house's.
const (
	forcedAvg      = 5 * time.Minute
	minRateSamples = 6
)

// Watch holds the recent readings and what has been announced. Not safe
// for concurrent use (one telemetry stream feeds it).
type Watch struct {
	samples      []sample
	lowSent      bool
	forecastSent bool
	forecastSoc  int // the charge when the forecast fired
}

// Observe takes one reading — the reported SoC, its estimate and the
// force-discharge session (if any) — and returns the alerts it triggers. battery-low and the re-arming follow
// the reported SoC (what the inverter and the UI show); the forecast
// fits the estimate.
func (w *Watch) Observe(now time.Time, soc int, est float64, p Params, s Session) []Alert {
	if n := len(w.samples); n > 0 && now.Before(w.samples[n-1].at) {
		w.samples = w.samples[:0] // the clock went back: start afresh
	}
	w.samples = append(w.samples, sample{now, est, s.Forced, s.RatesKnown, s.BattRate, s.HouseRate})
	cut := 0
	for cut < len(w.samples) && now.Sub(w.samples[cut].at) > p.Window {
		cut++
	}
	w.samples = w.samples[cut:]

	if p.Threshold <= 0 {
		w.lowSent, w.forecastSent = false, false
		return nil
	}
	if soc > p.Threshold+rearmMargin {
		w.lowSent = false
	}
	if w.forecastSent && !w.lowSent && soc > w.forecastSoc+rearmMargin {
		w.forecastSent = false
	}
	if soc <= p.Threshold {
		if w.lowSent {
			return nil
		}
		w.lowSent, w.forecastSent, w.forecastSoc = true, true, soc
		return []Alert{{KindLow, fmt.Sprintf("Battery at %d %% (alert level %d %%).", soc, p.Threshold)}}
	}
	if w.forecastSent {
		return nil
	}
	forecast := w.forecast
	if s.Forced {
		forecast = w.forecastSession
	}
	if a, ok := forecast(now, soc, p, s); ok {
		w.forecastSent, w.forecastSoc = true, soc
		return []Alert{a}
	}
	return nil
}

// fit is a least-squares line through readings, x in hours relative
// to now (≤ 0), y in %.
type fit struct {
	n                int
	mx, sxx          float64
	slope, icept, sd float64
}

// fitSamples fits the window's readings taken in the same state (in a
// force discharge or not) as now; false when too few or too short.
func (w *Watch) fitSamples(now time.Time, p Params, forced bool) (fit, bool) {
	var xs, ys []float64
	var first time.Time
	for _, s := range w.samples {
		if s.forced != forced {
			continue
		}
		if len(xs) == 0 {
			first = s.at
		}
		xs = append(xs, s.at.Sub(now).Hours())
		ys = append(ys, s.soc)
	}
	n := len(xs)
	if n < minSamples || now.Sub(first) < p.Window/minSpanDiv {
		return fit{}, false
	}
	fn := float64(n)
	var sx, sy float64
	for i := range xs {
		sx += xs[i]
		sy += ys[i]
	}
	f := fit{n: n, mx: sx / fn}
	my := sy / fn
	var sxy float64
	for i := range xs {
		dx := xs[i] - f.mx
		f.sxx += dx * dx
		sxy += dx * (ys[i] - my)
	}
	if f.sxx == 0 {
		return fit{}, false
	}
	f.slope = sxy / f.sxx       // %/h
	f.icept = my - f.slope*f.mx // the fitted charge now
	var sse float64
	for i := range xs {
		r := ys[i] - (f.icept + f.slope*xs[i])
		sse += r * r
	}
	f.sd = math.Sqrt(sse / float64(n-lineParams))
	return f, true
}

// se is the standard error of a prediction x hours ahead.
func (f fit) se(x float64) float64 {
	return f.sd * math.Sqrt(1+1/float64(f.n)+(x-f.mx)*(x-f.mx)/f.sxx)
}

// forecast fits the window's readings (outside a force discharge) and
// tests the bound at the horizon.
func (w *Watch) forecast(now time.Time, soc int, p Params, _ Session) (Alert, bool) {
	f, ok := w.fitSamples(now, p, false)
	if !ok || f.slope >= 0 {
		return Alert{}, false
	}
	x0 := p.Horizon.Hours()
	upper := f.icept + f.slope*x0 + tQuantile(p.P, f.n-lineParams)*f.se(x0)
	if upper > float64(p.Threshold) {
		return Alert{}, false
	}
	eta := now.Add(time.Duration((float64(p.Threshold) - f.icept) / f.slope * float64(time.Hour)))
	return Alert{KindForecastLow, fmt.Sprintf(
		"Battery %d %%, falling %s %%/h: expected at %d %% by about %s (%s %% confident within %s).",
		soc, fmtRate(f.slope), p.Threshold, eta.Format("15:04"), fmtConf(p.P), fmtHorizon(p.Horizon))}, true
}

// forecastSession forecasts during a force discharge: the measured fall
// until the discharge is expected to end, then the house's recent usage.
func (w *Watch) forecastSession(now time.Time, soc int, p Params, s Session) (Alert, bool) {
	forced, house, ok := w.sessionRates(now, s)
	if !ok {
		return Alert{}, false
	}
	rF := mean(forced)
	rH, sdH := mean(house), stddev(house)
	est := w.samples[len(w.samples)-1].soc
	thr := float64(p.Threshold)
	x0 := p.Horizon.Hours()
	hEnd := dischargeEnd(est, rF, s, x0)
	yEnd := est + rF*hEnd
	rest := x0 - hEnd
	nH := float64(len(house))
	upper := yEnd + rH*rest + tQuantile(p.P, len(house)-1)*sdH/math.Sqrt(nH)*rest
	if upper > thr {
		return Alert{}, false
	}
	var etaH float64
	switch {
	case rF < 0 && yEnd <= thr:
		etaH = (thr - est) / rF
	case rH < 0:
		etaH = hEnd + (thr-yEnd)/rH
	default:
		etaH = x0
	}
	eta := now.Add(time.Duration(etaH * float64(time.Hour)))
	return Alert{KindForecastLow, fmt.Sprintf(
		"Battery %d %%, force discharging at %s %%/h until %s, then the house uses %s %%/h: expected at %d %% by about %s (%s %% confident within %s).",
		soc, fmtRate(rF), now.Add(time.Duration(hEnd*float64(time.Hour))).Format("15:04"), fmtRate(rH),
		p.Threshold, eta.Format("15:04"), fmtConf(p.P), fmtHorizon(p.Horizon))}, true
}

// sessionRates gathers the battery's rates over the last forcedAvg of
// the session and the house's over the window; false when too few.
func (w *Watch) sessionRates(now time.Time, s Session) (forced, house []float64, ok bool) {
	if !s.RatesKnown {
		return nil, nil, false
	}
	for _, x := range w.samples {
		if !x.rates {
			continue
		}
		house = append(house, x.house)
		if x.forced && now.Sub(x.at) <= forcedAvg {
			forced = append(forced, x.batt)
		}
	}
	return forced, house, len(forced) >= minRateSamples && len(house) >= minRateSamples
}

// dischargeEnd is when (hours ahead) the forced discharge is expected to
// end: the window's end, reaching the cut-off at rate rF, or the horizon
// x0, whichever comes first; now when the charge is at the cut-off.
func dischargeEnd(est, rF float64, s Session, x0 float64) float64 {
	h := math.Min(x0, s.Left.Hours())
	switch {
	case est <= s.CutoffPct:
		return 0
	case rF < 0:
		return math.Min(h, (s.CutoffPct-est)/rF)
	}
	return h
}

// fmtRate writes a falling rate (−%/h) as a positive one, to 0.1 %/h.
func fmtRate(r float64) string {
	return strconv.FormatFloat(math.Round(-r*oneDecimal)/oneDecimal, 'f', -1, 64)
}

// fmtConf writes 1 − p as a percentage.
func fmtConf(p float64) string { return strconv.FormatFloat((1-p)*percent, 'f', -1, 64) }

func mean(v []float64) float64 {
	var t float64
	for _, x := range v {
		t += x
	}
	return t / float64(len(v))
}

// stddev is the sample standard deviation.
func stddev(v []float64) float64 {
	m := mean(v)
	var t float64
	for _, x := range v {
		t += (x - m) * (x - m)
	}
	return math.Sqrt(t / float64(len(v)-1))
}

// fmtHorizon writes 2h0m0s as "2 h", 90m as "1 h 30 min".
func fmtHorizon(d time.Duration) string {
	h, m := int(d.Hours()), int(d.Minutes())%60
	switch {
	case h == 0:
		return fmt.Sprintf("%d min", m)
	case m == 0:
		return fmt.Sprintf("%d h", h)
	}
	return fmt.Sprintf("%d h %d min", h, m)
}

// tQuantile is the upper-p quantile of Student's t with df degrees of
// freedom: the normal quantile with the Cornish–Fisher correction
// (within 0.01 of the tables from df 10 up; the forecast needs 28+).
func tQuantile(p float64, df int) float64 {
	z := math.Sqrt2 * math.Erfinv(1-2*p)
	v := float64(df)
	z3, z5 := z*z*z, z*z*z*z*z
	return z + (z3+z)/(4*v) + (5*z5+16*z3+3*z)/(96*v*v)
}
