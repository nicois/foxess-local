// Package siteclock is the site's time zone and the inverter's clock
// (owner, 2026-11-19). The inverter keeps a bare wall clock (40000 ×6,
// no zone); the FoxESS cloud used to set it to the plant's local time,
// and away from the cloud this app does. The site zone is a setting of
// its own: it is never assumed to be the host's.
package siteclock

import (
	"errors"
	"fmt"
	"time"
	_ "time/tzdata" // the zone database is embedded: the host's is not needed
)

// The judgement's bounds.
const (
	// InStepMax: a smaller difference is left alone.
	InStepMax = time.Minute
	// DriftMax: a difference up to this is drift, corrected
	// automatically; it is also how far a chosen zone may be from the
	// inverter's clock in setup.
	DriftMax = 5 * time.Minute
	// ShiftLookback: an inverter still on an offset the site had this
	// recently missed a daylight-saving change (corrected automatically).
	ShiftLookback = 60 * 24 * time.Hour
)

// ErrNoZone is an empty or non-IANA zone name.
var ErrNoZone = errors.New("siteclock: not a time zone name (want Area/City, e.g. Australia/Melbourne)")

// Zone loads an IANA zone by name. "Local" (the host's zone) is not a
// site zone.
func Zone(name string) (*time.Location, error) {
	if name == "" || name == "Local" {
		return nil, ErrNoZone
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("%w: %q", ErrNoZone, name)
	}
	return loc, nil
}

// Kind is a clock judgement.
type Kind string

// The judgements.
const (
	InStep      Kind = "in_step"     // within InStepMax
	Drift       Kind = "drift"       // within DriftMax: corrected automatically
	ZoneShift   Kind = "zone_shift"  // a missed daylight-saving change: corrected automatically
	Unexplained Kind = "unexplained" // anything else: never corrected without the owner
	Invalid     Kind = "invalid"     // the reading is not a date and time
)

// Verdict is the inverter's clock against the site's time.
type Verdict struct {
	Kind Kind
	// Off is the inverter's wall time minus the site's (positive: the
	// inverter is ahead), to the second.
	Off time.Duration
}

// Judge compares the inverter's clock reading (year, month, day, hour,
// minute, second), taken at readAt, with the site's wall time then. now
// bounds the daylight-saving lookback.
func Judge(loc *time.Location, rtc [6]int, readAt, now time.Time) Verdict {
	inv := time.Date(rtc[0], time.Month(rtc[1]), rtc[2], rtc[3], rtc[4], rtc[5], 0, time.UTC)
	if Wall(time.UTC, inv) != rtc {
		return Verdict{Kind: Invalid}
	}
	site := readAt.Truncate(time.Second).In(loc)
	_, cur := site.Zone()
	siteWall := time.Date(site.Year(), site.Month(), site.Day(), site.Hour(), site.Minute(), site.Second(), 0, time.UTC)
	off := inv.Sub(siteWall)
	switch {
	case off.Abs() < InStepMax:
		return Verdict{InStep, off}
	case off.Abs() <= DriftMax:
		return Verdict{Drift, off}
	}
	implied := off + time.Duration(cur)*time.Second // the inverter's offset from UTC
	for d := time.Duration(0); d <= ShiftLookback; d += 24 * time.Hour {
		_, o := now.Add(-d).In(loc).Zone()
		if o != cur && (implied-time.Duration(o)*time.Second).Abs() <= DriftMax {
			return Verdict{ZoneShift, off}
		}
	}
	return Verdict{Unexplained, off}
}

// Wall is t's wall time in loc: what the inverter's clock should read.
func Wall(loc *time.Location, t time.Time) [6]int {
	w := t.In(loc)
	return [6]int{w.Year(), int(w.Month()), w.Day(), w.Hour(), w.Minute(), w.Second()}
}

// Fresh is how old a clock reading may be to stand for the inverter's
// clock now (the poller reads it every 5 minutes).
const Fresh = 15 * time.Minute

// InverterNow is the inverter's wall time now, from a reading taken at
// readAt: false when the reading is older than Fresh or impossible. The
// result's zone is a fixed offset named "inverter" (its wall fields are
// what the inverter's clock shows).
func InverterNow(rtc [6]int, readAt, now time.Time) (time.Time, bool) {
	inv := time.Date(rtc[0], time.Month(rtc[1]), rtc[2], rtc[3], rtc[4], rtc[5], 0, time.UTC)
	if Wall(time.UTC, inv) != rtc || now.Sub(readAt) > Fresh || now.Before(readAt.Add(-time.Minute)) {
		return time.Time{}, false
	}
	off := inv.Sub(readAt.Truncate(time.Second)).Round(time.Second)
	return now.In(time.FixedZone("inverter", int(off/time.Second))), true
}

// The inverter's clock block: 40000 (0x9C40) ×6, read with the settings.
const (
	RegClock  = 0x9C40
	ClockRegs = 6
)

// ReadingFrom is the newest clock reading through a register lookup
// (store.RegisterRead): the six values and when the last was read;
// false before a complete read.
func ReadingFrom(read func(addr int) (val int, ts time.Time, ok bool, err error)) (rtc [6]int, at time.Time, ok bool) {
	for i := range ClockRegs {
		v, ts, found, err := read(RegClock + i)
		if err != nil || !found {
			return rtc, at, false
		}
		rtc[i] = v
		if i == ClockRegs-1 {
			at = ts
		}
	}
	return rtc, at, true
}

// WallNow is the inverter's wall time now — what its schedule runs on:
// from a fresh reading, else the site zone's time, else unknown. The
// host's zone is never used.
func WallNow(zone *time.Location, rtc [6]int, at time.Time, read bool, now time.Time) (time.Time, bool) {
	if read {
		if w, ok := InverterNow(rtc, at, now); ok {
			return w, true
		}
	}
	if zone != nil {
		return now.In(zone), true
	}
	return time.Time{}, false
}
