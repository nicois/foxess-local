package siteclock

import (
	"testing"
	"time"
)

func mel(t *testing.T) *time.Location {
	t.Helper()
	loc, err := Zone("Australia/Melbourne")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// rtcOf is the inverter clock reading the wall time w (in loc) shows.
func rtcOf(w time.Time) [6]int {
	return [6]int{w.Year(), int(w.Month()), w.Day(), w.Hour(), w.Minute(), w.Second()}
}

// TestZone: an IANA name from the embedded database (the host's zone
// files are not needed and its zone is never assumed); no name, a
// path, or a fixed abbreviation is not a site zone.
func TestZone(t *testing.T) {
	for _, ok := range []string{"Australia/Melbourne", "Europe/London", "America/Argentina/Buenos_Aires", "UTC"} {
		if _, err := Zone(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "Local", "Australia/Atlantis", "../etc/passwd", "/etc/localtime", "AEDT"} {
		if _, err := Zone(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// TestJudge (owner, 2026-11-19): the inverter's clock against the site's
// time now. Under a minute is in step; up to 5 minutes is drift; a whole
// zone offset the site had in the last 60 days is a missed daylight-
// saving change; anything else is unexplained (never corrected without
// the owner).
func TestJudge(t *testing.T) {
	loc := mel(t)
	now := time.Date(2026, 11, 19, 5, 17, 26, 0, time.UTC) // 16:17:26 AEDT
	wall := now.In(loc)
	for name, c := range map[string]struct {
		rtc  time.Time // the wall time the inverter shows
		want Kind
		off  time.Duration
	}{
		"in step":        {wall.Add(20 * time.Second), InStep, 20 * time.Second},
		"drift behind":   {wall.Add(-3 * time.Minute), Drift, -3 * time.Minute},
		"drift ahead":    {wall.Add(4*time.Minute + 59*time.Second), Drift, 4*time.Minute + 59*time.Second},
		"7 min ahead":    {wall.Add(7 * time.Minute), Unexplained, 7 * time.Minute},
		"host on UTC":    {now, Unexplained, -11 * time.Hour},
		"half an hour":   {wall.Add(-30 * time.Minute), Unexplained, -30 * time.Minute},
		"two hours":      {wall.Add(-2 * time.Hour), Unexplained, -2 * time.Hour},
		"missed DST, +2": {wall.Add(-time.Hour + 90*time.Second), ZoneShift, -time.Hour + 90*time.Second},
	} {
		v := Judge(loc, rtcOf(c.rtc), now, now)
		if v.Kind != c.want || v.Off != c.off {
			t.Errorf("%s: %s %s, want %s %s", name, v.Kind, v.Off, c.want, c.off)
		}
	}
	// after the change back (5 April 2027, 03:00 AEDT → 02:00 AEST) an
	// inverter still on daylight time is an hour ahead: a zone shift
	after := time.Date(2027, 4, 10, 2, 0, 0, 0, time.UTC)
	if v := Judge(loc, rtcOf(after.In(loc).Add(time.Hour)), after, after); v.Kind != ZoneShift {
		t.Errorf("April, an hour ahead: %s %s", v.Kind, v.Off)
	}
	// but more than 60 days later the old offset is no longer explained
	late := time.Date(2027, 7, 1, 2, 0, 0, 0, time.UTC)
	if v := Judge(loc, rtcOf(late.In(loc).Add(time.Hour)), late, late); v.Kind != Unexplained {
		t.Errorf("July, an hour ahead: %s", v.Kind)
	}
	// the reading's age counts: read 4 min ago, it has moved on since
	read := now.Add(-4 * time.Minute)
	if v := Judge(loc, rtcOf(read.In(loc)), read, now); v.Kind != InStep || v.Off != 0 {
		t.Errorf("an older reading: %s %s", v.Kind, v.Off)
	}
	// an impossible reading
	if v := Judge(loc, [6]int{2026, 13, 1, 0, 0, 0}, now, now); v.Kind != Invalid {
		t.Errorf("month 13: %s", v.Kind)
	}
}

// TestWall: the value to write is the site's wall time.
func TestWall(t *testing.T) {
	now := time.Date(2026, 11, 19, 5, 17, 26, 900_000_000, time.UTC)
	if got := Wall(mel(t), now); got != [6]int{2026, 11, 19, 16, 17, 26} {
		t.Fatalf("got %v", got)
	}
}

// TestInverterNow: the inverter's wall time now, from a reading taken
// earlier; a stale reading gives none.
func TestInverterNow(t *testing.T) {
	read := time.Date(2026, 11, 19, 5, 17, 26, 0, time.UTC)
	now := read.Add(3*time.Minute + 500*time.Millisecond)
	w, ok := InverterNow([6]int{2026, 11, 19, 16, 17, 26}, read, now)
	if !ok || w.Hour() != 16 || w.Minute() != 20 || w.Second() != 26 || w.Day() != 19 {
		t.Fatalf("got %v %v", w, ok)
	}
	if _, ok := InverterNow([6]int{2026, 11, 19, 16, 17, 26}, read, read.Add(Fresh+time.Second)); ok {
		t.Error("a stale reading gave a time")
	}
	if _, ok := InverterNow([6]int{2026, 13, 19, 16, 17, 26}, read, now); ok {
		t.Error("an impossible reading gave a time")
	}
}
