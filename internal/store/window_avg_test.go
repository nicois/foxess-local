package store

import (
	"testing"
	"time"
)

// TestWindowAverage: the "Now" averaging source — the mean of every raw
// sample in [from, to), with the sample count (coverage is visible).
func TestWindowAverage(t *testing.T) {
	s := newTestStore(t)
	from := testBase.Add(-5 * time.Minute)
	seedTeleRow(t, s, from.Add(10*time.Second), 1000, -200, 800, 900, -100, 60, 2)
	seedTeleRow(t, s, from.Add(20*time.Second), 3000, 200, 1200, 1100, 100, 61, 2)
	seedTeleRow(t, s, from.Add(-time.Second), 9999, 0, 0, 0, 0, 0, 2) // before the window
	a, ok, err := s.WindowAverage(from, testBase)
	if err != nil || !ok {
		t.Fatalf("WindowAverage: ok=%v err=%v", ok, err)
	}
	if a.N != 2 || a.SolAvg != 2000 || a.GridAvg != 0 || a.HausAvg != 1000 || a.AcportAvg != 1000 || a.BattAvg != 0 || a.SocLast != 61 {
		t.Fatalf("got %+v", a)
	}
	if _, ok, err := s.WindowAverage(testBase, testBase.Add(time.Minute)); err != nil || ok {
		t.Fatalf("empty window: ok=%v err=%v, want ok=false", ok, err)
	}
}
