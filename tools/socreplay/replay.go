package main

import (
	"fmt"
	"math"
	"time"

	"foxesslocal/internal/socest"
)

// The estimator's constants, restated for the independent recomputation
// (a drift between the two is reported as a violation).
const (
	bucketBelow = 0.5
	bucketAbove = 0.494
	maxGap      = time.Minute
	kMin, kMax  = 0.7, 1.3
	pctMax      = 100
	whPerHour   = 3600 // seconds per hour, for W·s → Wh
	tolerance   = 1e-6 // estimate vs recomputation, %
	edgeSlack   = 1e-9 // bucket bounds, %
)

// row is one telemetry sample: the reported SoC and the battery power
// (W, + = charging).
type row struct {
	TS   time.Time
	Soc  int
	Batt int
}

// tick is one change of the reported SoC.
type tick struct {
	At       time.Time
	From, To int
	Dir      int           // +1 up, −1 down; 0 = a jump of more than 1 %
	Reversal bool          // the first tick after one in the other direction
	Span     time.Duration // since the previous tick
	NetWh    float64       // the energy integrated since the previous tick
	Gap      bool          // the interval had a gap (not integrated across)
	Pinned   float64       // % the clamp removed during the interval
	Jump     float64       // % the tick moved the estimate beyond the integration (+ = forward)
	EstAt    float64       // the estimate just before the tick
}

// result is a replay's outcome.
type result struct {
	Rows       int
	Ticks      []tick
	Violations []string
	Anomalies  []string
	Learned    socest.Learned
	Est        []float64 // per row
}

// replayer runs rows through a fresh estimator and measures each tick.
type replayer struct {
	e       socest.Estimator
	capWh   int
	res     result
	prev    row
	prevEst float64
	started bool
	lastDir int
	cur     tick // the interval being measured
}

// replay runs the rows through a fresh estimator (restored from learned
// when given).
func replay(rows []row, capWh int, learned *socest.Learned) result {
	p := &replayer{capWh: capWh, res: result{Rows: len(rows), Est: make([]float64, len(rows))}}
	if learned != nil {
		p.e.Restore(*learned)
	}
	for i, r := range rows {
		p.res.Est[i] = p.step(r)
	}
	p.res.Learned = p.e.Learned()
	return p.res
}

func (p *replayer) violate(r row, f string, a ...any) {
	p.res.Violations = append(p.res.Violations, r.TS.Format(time.RFC3339)+" "+fmt.Sprintf(f, a...))
}

// bucket is the range the estimate is held to for a reported SoC.
func bucket(raw int) (lo, hi float64) {
	return math.Max(float64(raw)-bucketBelow, 0), math.Min(float64(raw)+bucketAbove, pctMax)
}

// step observes one row and returns the estimate.
func (p *replayer) step(r row) float64 {
	l := p.e.Learned() // the factor this reading integrates with
	est := p.e.Observe(r.TS, r.Soc, r.Batt, p.capWh)
	lo, hi := bucket(r.Soc)
	switch {
	case math.IsNaN(est) || math.IsInf(est, 0):
		p.violate(r, "estimate not finite: %v", est)
	case est < lo-edgeSlack || est > hi+edgeSlack:
		p.violate(r, "estimate %.4f outside [%.3f, %.3f] (raw %d)", est, lo, hi, r.Soc)
	}
	if !p.started {
		p.started, p.prev, p.prevEst, p.cur = true, r, est, tick{At: r.TS}
		return est
	}
	free, meanW := p.integrate(r, l)
	want := math.Min(math.Max(p.prevEst+free, lo), hi)
	if p.capWh <= 0 {
		want = float64(r.Soc)
	}
	if math.Abs(want-est) > tolerance {
		p.violate(r, "estimate %.6f, recomputed %.6f", est, want)
	}
	removed := p.prevEst + free - est // what the clamp took, signed
	if r.Soc != p.prev.Soc {
		p.tick(r, removed, meanW)
	} else if !p.cur.Gap {
		p.cur.Pinned += math.Abs(removed)
	}
	p.prev, p.prevEst = r, est
	return est
}

// integrate is the change since the previous row, % (0 across a gap,
// which marks the interval), and the mean power.
func (p *replayer) integrate(r row, l socest.Learned) (free, meanW float64) {
	dt := r.TS.Sub(p.prev.TS)
	meanW = float64(p.prev.Batt+r.Batt) / 2
	if dt <= 0 || dt > maxGap {
		p.cur.Gap = true
		return 0, meanW
	}
	if p.capWh <= 0 {
		return 0, meanW
	}
	wh := meanW * dt.Seconds() / whPerHour
	p.cur.NetWh += wh
	return wh * factor(l, meanW, p.capWh) / float64(p.capWh) * pctMax, meanW
}

// tick closes the interval at a change of the reported SoC.
func (p *replayer) tick(r row, removed, meanW float64) {
	step := r.Soc - p.prev.Soc
	t := p.cur
	t.From, t.To, t.At, t.Span, t.EstAt = p.prev.Soc, r.Soc, r.TS, r.TS.Sub(p.cur.At), p.prevEst
	if step == 1 || step == -1 {
		t.Dir = step
		t.Jump = -removed * float64(step)
		t.Reversal = p.lastDir != 0 && p.lastDir != step
		p.lastDir = step
		if meanW != 0 && (meanW > 0) != (step > 0) {
			p.res.Anomalies = append(p.res.Anomalies, fmt.Sprintf("%s SoC %d→%d against the battery flow (%.0f W)",
				r.TS.Format(time.RFC3339), p.prev.Soc, r.Soc, meanW))
		}
	} else {
		p.lastDir = 0
	}
	p.res.Ticks = append(p.res.Ticks, t)
	p.cur = tick{At: r.TS}
}

// factor mirrors the estimator's learnt correction.
func factor(l socest.Learned, meanW float64, capWh int) float64 {
	d := l.Discharging
	if meanW > 0 {
		d = l.Charging
	}
	if !d.Ready() {
		return 1
	}
	return math.Min(math.Max(float64(capWh)/pctMax/d.WhPerPct, kMin), kMax)
}
