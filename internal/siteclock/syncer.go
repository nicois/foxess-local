package siteclock

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// The correction cadence.
const (
	// SyncEvery: how often the latest reading is judged.
	SyncEvery = time.Minute
	// minGap: at most one automatic write per hour.
	minGap = time.Hour
)

// Syncer keeps the inverter's clock to the site's time (owner,
// 2026-11-19): drift and a missed daylight-saving change are corrected
// with the site zone's wall time from this machine's clock; anything
// else is left for the owner (the setup check offers to set it).
type Syncer struct {
	Zone       func() *time.Location                      // the site zone; nil = not chosen
	Reading    func() (rtc [6]int, at time.Time, ok bool) // the newest clock reading
	HostSynced func() bool                                // this machine's clock is NTP-synchronised
	Online     func() bool                                // an inverter session to write on
	// Set writes the clock (gateway KindClock) and returns once the
	// inverter echoed it.
	Set   func(ctx context.Context, wall [6]int) error
	Audit func(detail string) // one row per write, and per held state
	Clock func() time.Time

	lastWrite time.Time // when the last write was attempted
	held      string    // the last held state audited ("" = none)
}

// Run ticks every SyncEvery until ctx is done.
func (s *Syncer) Run(ctx context.Context) {
	t := time.NewTicker(SyncEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Tick(ctx)
		}
	}
}

// Tick judges the newest reading once and corrects when it may.
func (s *Syncer) Tick(ctx context.Context) {
	zone := s.Zone()
	rtc, at, ok := s.Reading()
	now := s.Clock()
	if zone == nil || !ok || !at.After(s.lastWrite) {
		return // nothing to judge (or only a reading from before the last write)
	}
	if _, fresh := InverterNow(rtc, at, now); !fresh {
		return
	}
	v := Judge(zone, rtc, at, now)
	off := describe(v.Off, zone)
	switch v.Kind {
	case InStep:
		s.held = ""
		return
	case Unexplained, Invalid:
		s.hold(fmt.Sprintf("the inverter's clock is %s: not corrected automatically (set it from Setup if %s is right)", off, zone))
		return
	case Drift, ZoneShift:
	}
	if !s.HostSynced() {
		s.hold("the inverter's clock is " + off + ": not corrected while this machine's clock is not synchronised")
		return
	}
	if !s.Online() || now.Sub(s.lastWrite) < minGap {
		return
	}
	s.held = ""
	s.lastWrite = now
	wall := Wall(zone, now)
	why := "drift"
	if v.Kind == ZoneShift {
		why = "a missed daylight-saving change"
	}
	detail := fmt.Sprintf("clock: the inverter's was %s (%s); set to %s", off, why, wallText(wall))
	if err := s.Set(ctx, wall); err != nil {
		detail += ": " + err.Error()
	}
	s.Audit(detail)
}

// hold audits a held state once (not every tick).
func (s *Syncer) hold(why string) {
	if why == s.held {
		return
	}
	s.held = why
	s.Audit("clock: " + why)
}

// describe renders an offset against the site zone: "3 min ahead of
// Australia/Melbourne time".
func describe(off time.Duration, zone *time.Location) string {
	way := "ahead of"
	if off < 0 {
		way = "behind"
	}
	return fmt.Sprintf("%s %s %s time", Duration(off.Abs()), way, zone)
}

// Duration renders a clock difference: "11 h", "5 h 30 min", "3 min",
// "3 min 10 s", "45 s".
func Duration(d time.Duration) string {
	d = d.Round(time.Second)
	h, m, sec := int(d/time.Hour), int(d%time.Hour/time.Minute), int(d%time.Minute/time.Second)
	var parts []string
	if h > 0 {
		parts = append(parts, fmt.Sprintf("%d h", h))
	}
	if m > 0 {
		parts = append(parts, fmt.Sprintf("%d min", m))
	}
	if sec > 0 && h == 0 {
		parts = append(parts, fmt.Sprintf("%d s", sec))
	}
	if len(parts) == 0 {
		return "0 s"
	}
	return strings.Join(parts, " ")
}

func wallText(w [6]int) string {
	return fmt.Sprintf("%04d-%02d-%02d %02d:%02d:%02d", w[0], w[1], w[2], w[3], w[4], w[5])
}
