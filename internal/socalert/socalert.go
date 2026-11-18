// Package socalert decides the battery alerts (owner, 2026-11-18):
//
//   - battery-low: the state of charge is at or below the threshold;
//   - battery-forecast-low: a least-squares line through the last window
//     of readings, projected to now + horizon, has its one-sided upper
//     prediction bound (confidence 1 − p) at or below the threshold —
//     the charge will be there within the horizon, with that confidence.
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
	at  time.Time
	soc float64
}

// Watch holds the recent readings and what has been announced. Not safe
// for concurrent use (one telemetry stream feeds it).
type Watch struct {
	samples      []sample
	lowSent      bool
	forecastSent bool
	forecastSoc  int // the charge when the forecast fired
}

// Observe takes one reading and returns the alerts it triggers.
func (w *Watch) Observe(now time.Time, soc int, p Params) []Alert {
	if n := len(w.samples); n > 0 && now.Before(w.samples[n-1].at) {
		w.samples = w.samples[:0] // the clock went back: start afresh
	}
	w.samples = append(w.samples, sample{now, float64(soc)})
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
	if a, ok := w.forecast(now, soc, p); ok {
		w.forecastSent, w.forecastSoc = true, soc
		return []Alert{a}
	}
	return nil
}

// forecast fits the window and tests the bound at the horizon.
func (w *Watch) forecast(now time.Time, soc int, p Params) (Alert, bool) {
	n := len(w.samples)
	if n < minSamples || now.Sub(w.samples[0].at) < p.Window/minSpanDiv {
		return Alert{}, false
	}
	// x in hours relative to now (≤ 0), y in %
	var sx, sy float64
	for _, s := range w.samples {
		sx += s.at.Sub(now).Hours()
		sy += s.soc
	}
	fn := float64(n)
	mx, my := sx/fn, sy/fn
	var sxx, sxy float64
	for _, s := range w.samples {
		dx := s.at.Sub(now).Hours() - mx
		sxx += dx * dx
		sxy += dx * (s.soc - my)
	}
	if sxx == 0 {
		return Alert{}, false
	}
	slope := sxy / sxx // %/h
	if slope >= 0 {
		return Alert{}, false
	}
	icept := my - slope*mx // the fitted charge now
	var sse float64
	for _, s := range w.samples {
		r := s.soc - (icept + slope*s.at.Sub(now).Hours())
		sse += r * r
	}
	df := n - 2
	sd := math.Sqrt(sse / float64(df))
	x0 := p.Horizon.Hours()
	se := sd * math.Sqrt(1+1/fn+(x0-mx)*(x0-mx)/sxx)
	upper := icept + slope*x0 + tQuantile(p.P, df)*se
	if upper > float64(p.Threshold) {
		return Alert{}, false
	}
	eta := now.Add(time.Duration((float64(p.Threshold) - icept) / slope * float64(time.Hour)))
	conf := strconv.FormatFloat((1-p.P)*100, 'f', -1, 64)
	return Alert{KindForecastLow, fmt.Sprintf(
		"Battery %d %%, falling %s %%/h: expected at %d %% by about %s (%s %% confident within %s).",
		soc, strconv.FormatFloat(math.Round(-slope*oneDecimal)/oneDecimal, 'f', -1, 64), p.Threshold,
		eta.Format("15:04"), conf, fmtHorizon(p.Horizon))}, true
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
