package update

import (
	"testing"
	"time"
)

// TestCheckDue (2026-11-19): the installer's interval (default a week;
// 0 = off) — the first check a minute after start, then one interval
// after the last; a changed interval applies at once.
func TestCheckDue(t *testing.T) {
	start := time.Date(2026, 11, 19, 8, 0, 0, 0, time.UTC)
	week := 7 * 24 * time.Hour
	var never time.Time
	for _, c := range []struct {
		name     string
		now      time.Time
		last     time.Time
		interval time.Duration
		want     bool
	}{
		{"off", start.Add(time.Hour), never, 0, false},
		{"settling", start.Add(30 * time.Second), never, week, false},
		{"first", start.Add(firstCheckAfter), never, week, true},
		{"too soon", start.Add(3 * 24 * time.Hour), start.Add(time.Minute), week, false},
		{"a week on", start.Add(week + time.Minute), start.Add(time.Minute), week, true},
		{"shortened", start.Add(2 * time.Hour), start.Add(time.Minute), time.Hour, true},
		{"clock went back", start, start.Add(time.Hour), week, true}, // a last check in the future: check
	} {
		if got := checkDue(c.now, start, c.last, c.interval); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// TestAutoApplyTarget (2026-11-19): auto-apply tries each new eligible
// release once — not a version already attempted, not one that rolled
// back, never when it cannot be applied here.
func TestAutoApplyTarget(t *testing.T) {
	ok := View{Current: vPrev, Latest: vNew, Available: true, CanApply: true}
	if tag, yes := autoApplyTarget(ok, ""); !yes || tag != vNew {
		t.Fatalf("eligible: %q %v", tag, yes)
	}
	if _, yes := autoApplyTarget(ok, vNew); yes {
		t.Fatal("an attempted version was tried again")
	}
	rolled := ok
	rolled.Failed = &Marker{From: vPrev, To: vNew}
	if _, yes := autoApplyTarget(rolled, ""); yes {
		t.Fatal("a rolled-back version was tried again")
	}
	cannot := ok
	cannot.CanApply = false
	if _, yes := autoApplyTarget(cannot, ""); yes {
		t.Fatal("applied where it cannot be")
	}
	newer := rolled
	newer.Latest = "v0.2.1"
	if tag, yes := autoApplyTarget(newer, vNew); !yes || tag != "v0.2.1" {
		t.Fatalf("a later release after a rollback: %q %v", tag, yes)
	}
}
