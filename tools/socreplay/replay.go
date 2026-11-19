package main

import (
	"fmt"
	"math"
	"time"

	"foxesslocal/internal/socest"
)

// The estimator's model (internal/socest, D-028), restated for the
// independent recomputation: a drift between the two is reported as a
// violation.
const (
	deadBand     = 1.0              // the true SoC is within this of the reported value, %
	anchorMaxAge = 15 * time.Minute // a snapshot anchors the estimate for this long
	maxGap       = time.Minute
	kMin, kMax   = 0.5, 1.5
	pctMax       = 100
	deciPerPct   = 10
	whPerHour    = 3600 // seconds per hour, for W·s → Wh
	tolerance    = 1e-6 // estimate vs recomputation, %
	edgeSlack    = 1e-9 // bounds, %
)

// row is one telemetry sample: the reported SoC and the battery power
// (W, + = charging).
type row struct {
	TS   time.Time
	Soc  int
	Batt int
}

// anchor is one snapshot's BMS SoC (0.1 %).
type anchor struct {
	TS   time.Time
	Deci int
}

// tick is one change of the reported SoC.
type tick struct {
	At       time.Time
	From, To int
	Dir      int           // +1 up, −1 down; 0 = a jump of more than 1 %
	Reversal bool          // the first tick after one in the other direction
	Anchored bool          // a snapshot anchored the estimate (no dead band applies)
	Span     time.Duration // since the previous tick
	NetWh    float64       // the energy integrated since the previous tick
	Gap      bool          // the interval had a gap (not integrated across)
	Pinned   float64       // % the dead band removed during the interval
	Jump     float64       // % the tick moved the estimate beyond the integration (+ = forward)
	EstAt    float64       // the estimate just before the tick
}

// result is a replay's outcome.
type result struct {
	Rows       int
	Ticks      []tick
	Drift      []float64   // per snapshot: the estimate it found − the BMS SoC, % (+ = the estimate was high)
	DriftAt    []time.Time // when each was measured
	Violations []string
	Anomalies  []string
	Learned    socest.Learned
	Est        []float64 // per row
}

// replayer runs rows through a fresh estimator and measures each tick.
type replayer struct {
	e        socest.Estimator
	capWh    int
	res      result
	prev     row
	prevEst  float64
	started  bool
	lastDir  int
	cur      tick // the interval being measured
	anchorAt time.Time
	anchorP  float64
}

// replay runs the rows, and the snapshots between them, through a fresh
// estimator (restored from learned when given).
func replay(rows []row, anchors []anchor, capWh int, learned *socest.Learned) result {
	p := &replayer{capWh: capWh, res: result{Rows: len(rows), Est: make([]float64, len(rows))}}
	if learned != nil {
		p.e.Restore(*learned)
	}
	k := 0
	for i, r := range rows {
		for ; k < len(anchors) && !anchors[k].TS.After(r.TS); k++ {
			p.anchor(anchors[k])
		}
		p.res.Est[i] = p.step(r)
	}
	p.res.Learned = p.e.Learned()
	return p.res
}

func (p *replayer) violate(r row, f string, a ...any) {
	p.res.Violations = append(p.res.Violations, r.TS.Format(time.RFC3339)+" "+fmt.Sprintf(f, a...))
}

// anchor applies one snapshot, measuring how far the estimate had moved
// from the BMS SoC since the previous one.
func (p *replayer) anchor(a anchor) {
	pct := float64(a.Deci) / deciPerPct
	// only when the readings reached the snapshot (a gap in the poll is
	// not the integration's error)
	if p.started && p.fresh(a.TS) && a.TS.Sub(p.prev.TS) <= maxGap {
		p.res.Drift = append(p.res.Drift, p.prevEst-pct)
		p.res.DriftAt = append(p.res.DriftAt, a.TS)
	}
	p.e.Anchor(a.TS, a.Deci)
	p.anchorAt, p.anchorP = a.TS, pct
	if p.started {
		p.prevEst = pct
	}
}

// fresh mirrors the estimator: a snapshot anchors for anchorMaxAge.
func (p *replayer) fresh(now time.Time) bool {
	return p.capWh > 0 && !p.anchorAt.IsZero() && now.Sub(p.anchorAt) <= anchorMaxAge && p.anchorAt.Sub(now) <= maxGap
}

// step observes one row and returns the estimate.
func (p *replayer) step(r row) float64 {
	l := p.e.Learned() // the factor this reading integrates with
	fresh := p.fresh(r.TS)
	est := p.e.Observe(r.TS, r.Soc, r.Batt, p.capWh)
	lo, hi := 0.0, float64(pctMax)
	if !fresh {
		lo, hi = math.Max(float64(r.Soc)-deadBand, 0), math.Min(float64(r.Soc)+deadBand, pctMax)
	}
	switch {
	case math.IsNaN(est) || math.IsInf(est, 0):
		p.violate(r, "estimate not finite: %v", est)
	case est < lo-edgeSlack || est > hi+edgeSlack:
		p.violate(r, "estimate %.4f outside [%.3f, %.3f] (raw %d)", est, lo, hi, r.Soc)
	}
	if !p.started {
		want := float64(r.Soc)
		if fresh {
			want = p.anchorP
		}
		p.check(r, est, math.Min(math.Max(want, lo), hi))
		p.started, p.prev, p.prevEst, p.cur = true, r, est, tick{At: r.TS}
		return est
	}
	free, meanW := p.integrate(r, l, fresh)
	want := p.prevEst + free
	switch {
	case p.capWh <= 0:
		want = float64(r.Soc)
	case r.Soc != p.prev.Soc && !fresh:
		want = float64(r.Soc)
	}
	p.check(r, est, math.Min(math.Max(want, lo), hi))
	removed := p.prevEst + free - est // what the model took, signed
	if r.Soc != p.prev.Soc {
		p.tick(r, removed, meanW, fresh)
	} else if !p.cur.Gap {
		p.cur.Pinned += math.Abs(removed)
	}
	p.prev, p.prevEst = r, est
	return est
}

func (p *replayer) check(r row, est, want float64) {
	if math.Abs(want-est) > tolerance {
		p.violate(r, "estimate %.6f, recomputed %.6f", est, want)
	}
}

// integrate is the change since the previous row, % (0 across a gap,
// which marks the interval), and the mean power. Anchored, the power is
// taken as measured.
func (p *replayer) integrate(r row, l socest.Learned, fresh bool) (free, meanW float64) {
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
	k := 1.0
	if !fresh {
		k = factor(l, meanW, p.capWh)
	}
	return wh * k / float64(p.capWh) * pctMax, meanW
}

// tick closes the interval at a change of the reported SoC.
func (p *replayer) tick(r row, removed, meanW float64, fresh bool) {
	step := r.Soc - p.prev.Soc
	t := p.cur
	t.From, t.To, t.At, t.Span, t.EstAt, t.Anchored = p.prev.Soc, r.Soc, r.TS, r.TS.Sub(p.cur.At), p.prevEst, fresh
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
