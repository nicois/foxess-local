// Package socest estimates the state of charge between the inverter's
// whole-percent readings (owner, 2026-11-20; D-028), as the foxess-control
// integration does: the battery power is integrated between readings,
// and the result is held within the reported tick's rounding bucket
// [raw − 0.5, raw + 0.494] — at two decimals the estimate rounds back to
// the reported value, and the reported value corrects the integration's
// drift at every tick.
//
// About 1 % of a 42 kWh pack is 420 Wh, so a slow discharge holds the
// reported SoC for an hour; the estimate moves every reading.
//
// The energy per percent is learnt (owner, 2026-11-20): two consecutive
// single-step ticks in one direction are exactly 1 % apart, whether the
// inverter rounds or truncates, so the energy integrated between them is
// the energy per percent — less than the nominal capacity says on a
// pack that has lost health, or when the capacity is set wrong, and
// different for charging and discharging (losses). It is learnt per
// direction and applied as a factor on the integration once minObs
// clean intervals agree, held within [kMin, kMax]; the clamp stays the
// backstop.
package socest

import (
	"math"
	"sync"
	"time"
)

const (
	// bucketBelow / bucketAbove bound the estimate around the reported
	// tick: raw + 0.494 shows as raw.49 at two decimals, which rounds
	// back to raw (foxess-control's 0.44 is the same bound at one
	// decimal).
	bucketBelow = 0.5
	bucketAbove = 0.494
	// maxGap: readings further apart than this are not integrated
	// across (a lost poll is 5–10 s; a gap is a session restart or an
	// outage, when the power in between is unknown).
	maxGap = time.Minute
	pctMax = 100

	// minObs clean intervals before the learnt energy per percent
	// applies; alphaMin is the weight of a new one once there are more
	// than 1/alphaMin (a running mean before, an exponential one after).
	minObs   = 3
	alphaMin = 0.2
	// kMin / kMax hold the applied factor (nominal ÷ learnt energy per
	// percent); an interval implying a factor outside [obsMin, obsMax]
	// is not learnt from (a BMS recalibration, two ticks in quick
	// succession).
	kMin, kMax     = 0.7, 1.3
	obsMin, obsMax = 0.5, 2.0
	// maxReverse: the energy flowing against the tick's direction may be
	// at most this share of the energy flowing with it.
	maxReverse = 0.1
	whPerHour  = 3600 // seconds per hour, for W·s → Wh
)

// Reading is the newest reported SoC and its estimate.
type Reading struct {
	Raw int       // the inverter's SoC, %
	Pct float64   // the estimate, %
	At  time.Time // when it was read
}

// Dir is what has been learnt for one direction.
type Dir struct {
	WhPerPct float64 `json:"wh_per_pct"` // the energy per percent, Wh
	N        int     `json:"n"`          // the clean intervals it is from
}

// Ready reports whether it applies (enough intervals).
func (d Dir) Ready() bool { return d.N >= minObs && d.WhPerPct > 0 }

// Learned is the energy per percent, charging and discharging.
type Learned struct {
	Charging    Dir `json:"charging"`
	Discharging Dir `json:"discharging"`
}

// span is the interval since the last single-step tick.
type span struct {
	dir   int     // +1 up, −1 down; 0 = no interval (none yet, or not clean)
	upWh  float64 // energy in (charging) since the tick
	outWh float64 // energy out (discharging)
}

// Estimator holds the running estimate. Safe for concurrent use (one
// telemetry stream feeds it; the API reads it).
type Estimator struct {
	mu    sync.Mutex
	have  bool
	ok    bool // a capacity was known: Pct is an estimate
	last  Reading
	lastW int
	span  span
	learn Learned
}

// Observe takes one reading — the reported SoC (%), the battery power
// (W, + = charging) and the pack's nominal capacity (Wh; 0 = unknown,
// when the estimate is the reported SoC) — and returns the estimate.
func (e *Estimator) Observe(now time.Time, raw, battW, capWh int) float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	est := float64(raw)
	if e.have && capWh > 0 {
		est = e.last.Pct + e.integrate(now, battW, capWh)
	}
	if e.have && raw != e.last.Raw {
		e.tick(raw-e.last.Raw, capWh)
	}
	est = min(max(est, float64(raw)-bucketBelow, 0), float64(raw)+bucketAbove, pctMax)
	e.have, e.ok = true, capWh > 0
	e.last, e.lastW = Reading{Raw: raw, Pct: est, At: now}, battW
	return est
}

// integrate is the change since the last reading, % (0 across a gap,
// which also voids the interval being measured).
func (e *Estimator) integrate(now time.Time, battW, capWh int) float64 {
	dt := now.Sub(e.last.At)
	if dt <= 0 || dt > maxGap {
		e.span.dir = 0 // the energy in between is unknown
		return 0
	}
	meanW := float64(e.lastW+battW) / 2
	wh := meanW * dt.Seconds() / whPerHour
	if wh >= 0 {
		e.span.upWh += wh
	} else {
		e.span.outWh -= wh
	}
	return wh * e.factor(meanW, capWh) / float64(capWh) * pctMax
}

// factor is the learnt correction for the flow's direction (1 until
// learnt).
func (e *Estimator) factor(meanW float64, capWh int) float64 {
	d := e.learn.Discharging
	if meanW > 0 {
		d = e.learn.Charging
	}
	if !d.Ready() {
		return 1
	}
	return min(max(float64(capWh)/pctMax/d.WhPerPct, kMin), kMax)
}

// tick closes the interval at a change of the reported SoC (learning
// from it when it is clean) and opens the next.
func (e *Estimator) tick(step, capWh int) {
	if step != 1 && step != -1 {
		e.span = span{} // a jump: not a 1 % interval, nor the start of one
		return
	}
	if s := e.span; s.dir == step && capWh > 0 {
		with, against := s.upWh, s.outWh
		d := &e.learn.Charging
		if step < 0 {
			with, against, d = s.outWh, s.upWh, &e.learn.Discharging
		}
		net := with - against
		if k := float64(capWh) / pctMax / net; net > 0 && against <= maxReverse*with && k >= obsMin && k <= obsMax {
			d.N++
			d.WhPerPct += max(1/float64(d.N), alphaMin) * (net - d.WhPerPct)
		}
	}
	e.span = span{dir: step}
}

// Latest is the newest reading; false before the first one, or while
// the capacity is unknown (no estimate).
func (e *Estimator) Latest() (Reading, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.last, e.have && e.ok
}

// Learned is the energy per percent learnt so far (to keep across
// restarts).
func (e *Estimator) Learned() Learned {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.learn
}

// Restore sets what was learnt (from a previous run). Values that are
// not plausible are dropped.
func (e *Estimator) Restore(l Learned) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, d := range []*Dir{&l.Charging, &l.Discharging} {
		if d.N < 0 || !(d.WhPerPct > 0) || math.IsInf(d.WhPerPct, 0) {
			*d = Dir{}
		}
	}
	e.learn = l
}
