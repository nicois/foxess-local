package store

import (
	"testing"
	"time"
)

// TestHistoryBucketsLiveReachesNow: the 24 h / 7 d / 30 d views must
// reach "now" — the stored rollups plus live aggregates for every window
// after the watermark (the in-progress bucket, and a completed bucket
// the 60 s maintenance has not rolled yet).
func TestHistoryBucketsLiveReachesNow(t *testing.T) {
	s := newTestStore(t)                  // now = testBase (12:00)
	b1 := testBase.Add(-45 * time.Minute) // 11:15, rolled
	b2 := testBase.Add(-30 * time.Minute) // 11:30, completed but not yet rolled
	seedBucket(t, s, b1, 100, 0, 0, 0, 0, 50, 2)
	if _, err := s.RollUp(); err != nil {
		t.Fatal(err)
	}
	seedBucket(t, s, b2, 200, 0, 0, 0, 0, 51, 2)
	partial := testBase.Add(-15 * time.Minute) // 11:45, in progress at 11:52
	seedTeleRow(t, s, partial.Add(time.Minute), 300, 0, 0, 0, 0, 52, 2)
	now := partial.Add(7 * time.Minute)

	got, err := s.HistoryBucketsLive(testBase.Add(-24*time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d buckets, want 3 (rolled + unrolled + in-progress)", len(got))
	}
	want := []struct {
		ts  time.Time
		sol int
	}{{b1, 100}, {b2, 200}, {partial, 300}}
	for i, w := range want {
		if !got[i].TS.Equal(w.ts) || got[i].SolAvg != w.sol {
			t.Errorf("bucket %d: %v sol %d, want %v sol %d", i, got[i].TS, got[i].SolAvg, w.ts, w.sol)
		}
	}
}

// TestHistoryBucketsLiveNoRollupYet: a fresh install (no rollup has run)
// still shows its data — computed from MIN(ts), not from `since`.
func TestHistoryBucketsLiveNoRollupYet(t *testing.T) {
	s := newTestStore(t)
	seedTeleRow(t, s, testBase.Add(-5*time.Minute), 400, 0, 0, 0, 0, 60, 2)
	got, err := s.HistoryBucketsLive(testBase.Add(-7*24*time.Hour), testBase)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SolAvg != 400 || !got[0].TS.Equal(testBase.Add(-15*time.Minute)) {
		t.Fatalf("got %+v, want the one in-progress bucket", got)
	}
}
