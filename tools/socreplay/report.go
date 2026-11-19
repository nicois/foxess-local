package main

import (
	"fmt"
	"io"
	"math"
	"slices"
)

// maxListed caps the violations listed.
const maxListed = 50

// printer writes formatted lines, keeping the first error.
type printer struct {
	w   io.Writer
	err error
}

func (p *printer) f(format string, a ...any) {
	if p.err == nil {
		_, p.err = fmt.Fprintf(p.w, format, a...)
	}
}

// stats is one group of ticks.
type stats struct {
	pinned, jump, span []float64
}

func (s *stats) add(t tick) {
	s.pinned = append(s.pinned, t.Pinned)
	s.jump = append(s.jump, t.Jump)
	s.span = append(s.span, t.Span.Minutes())
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return math.NaN()
	}
	s := slices.Clone(v)
	slices.Sort(s)
	return s[len(s)/2]
}

func mean(v []float64) float64 {
	if len(v) == 0 {
		return math.NaN()
	}
	sum := 0.0
	for _, x := range v {
		sum += x
	}
	return sum / float64(len(v))
}

var groupOrder = []string{"down, continuing", "down, after reversal", "up, continuing", "up, after reversal"}

// excluded says why a tick is left out of the summary ("" = counted).
func excluded(i int, t tick) string {
	switch {
	case t.Dir == 0:
		return "multi-step jump"
	case t.Gap:
		return "gap in interval"
	case i == 0:
		return "first (interval incomplete)"
	}
	return ""
}

func groupOf(t tick) string {
	k := "down"
	if t.Dir > 0 {
		k = "up"
	}
	if t.Reversal {
		return k + ", after reversal"
	}
	return k + ", continuing"
}

func report(w io.Writer, res result, capWh int, verbose bool) error {
	p := &printer{w: w}
	p.f("rows %d, ticks %d, capacity %d Wh (%.1f Wh/%%)\n\n", res.Rows, len(res.Ticks), capWh, float64(capWh)/pctMax)
	if verbose {
		listTicks(p, res.Ticks)
	}
	groups := map[string]*stats{}
	for _, k := range groupOrder {
		groups[k] = &stats{}
	}
	skipped := map[string]int{}
	for i, t := range res.Ticks {
		if why := excluded(i, t); why != "" {
			skipped[why]++
			continue
		}
		groups[groupOf(t)].add(t)
	}
	p.f("group                  n   pinned %% (mean / median)   jump %% (mean / median)   span min (median)\n")
	for _, k := range groupOrder {
		g := groups[k]
		p.f("%-20s %3d   %7.3f / %7.3f          %7.3f / %7.3f        %7.1f\n",
			k, len(g.pinned), mean(g.pinned), median(g.pinned), mean(g.jump), median(g.jump), median(g.span))
	}
	p.f("\nexcluded: %d multi-step jumps, %d intervals with a gap, the first tick\n", skipped["multi-step jump"], skipped["gap in interval"])
	l := res.Learned
	p.f("learnt: charging %.1f Wh/%% (n=%d), discharging %.1f Wh/%% (n=%d)\n",
		l.Charging.WhPerPct, l.Charging.N, l.Discharging.WhPerPct, l.Discharging.N)
	listLines(p, "anomalies", res.Anomalies)
	listLines(p, "violations", res.Violations)
	return p.err
}

func listTicks(p *printer, ticks []tick) {
	p.f("time (UTC)            tick     span    net Wh  est before  pinned %%  jump %%  notes\n")
	for i, t := range ticks {
		note := excluded(i, t)
		if note == "" && t.Reversal {
			note = "reversal"
		}
		p.f("%s  %3d→%-3d %6.1fm  %8.1f  %10.3f  %8.3f  %6.3f  %s\n",
			t.At.UTC().Format("2006-01-02 15:04:05"), t.From, t.To, t.Span.Minutes(), t.NetWh, t.EstAt, t.Pinned, t.Jump, note)
	}
	p.f("\n")
}

func listLines(p *printer, title string, lines []string) {
	p.f("\n%s: %d\n", title, len(lines))
	for i, v := range lines {
		if i == maxListed {
			p.f("  … %d more\n", len(lines)-maxListed)
			return
		}
		p.f("  %s\n", v)
	}
}
