package diskguard

import (
	"context"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"foxesslocal/internal/store"
)

const mb = 1 << 20

// fakeStore holds raw data from oldest to now; discarding frees
// bytesPerHour for every hour removed.
type fakeStore struct {
	mu           sync.Mutex
	oldest       time.Time
	free         uint64
	bytesPerHour uint64
	cutoffs      []time.Time
	reclaims     int
}

func (f *fakeStore) OldestRaw() (time.Time, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.oldest, !f.oldest.IsZero(), nil
}

func (f *fakeStore) DiscardRawBefore(cutoff time.Time) (store.DiscardReport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cutoffs = append(f.cutoffs, cutoff)
	if cutoff.After(f.oldest) {
		f.free += uint64(cutoff.Sub(f.oldest).Hours() * float64(f.bytesPerHour))
		f.oldest = cutoff
	}
	return store.DiscardReport{Frames: 1}, nil
}

func (f *fakeStore) Reclaim() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reclaims++
	return nil
}

func (f *fakeStore) space() (uint64, uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.free, 32 << 30, nil
}

func newGuard(f *fakeStore, now time.Time, alerts *[]string) *Guard {
	return &Guard{
		St: f, Space: f.space, MinFree: 200 * mb, KeepRaw: 6 * time.Hour,
		Now:   func() time.Time { return now },
		Alert: func(m string) { *alerts = append(*alerts, m) },
	}
}

// TestCheckPlenty: enough space — nothing is touched.
func TestCheckPlenty(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	f := &fakeStore{oldest: now.Add(-30 * 24 * time.Hour), free: 500 * mb, bytesPerHour: mb}
	var alerts []string
	if _, err := newGuard(f, now, &alerts).Check(); err != nil {
		t.Fatal(err)
	}
	if len(f.cutoffs) != 0 || len(alerts) != 0 {
		t.Fatalf("discarded %v, alerts %v", f.cutoffs, alerts)
	}
}

// TestCheckDiscardsOldestHalves: below the threshold the guard discards
// the older half of the raw window, step by step, until free space is
// back above twice the threshold — and alerts once.
func TestCheckDiscardsOldestHalves(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	oldest := now.Add(-30 * 24 * time.Hour)
	// 30 days at 1 MB/h: the first half (~357 h) frees ~357 MB, enough
	// for the 400 MB target from 100 MB free.
	f := &fakeStore{oldest: oldest, free: 100 * mb, bytesPerHour: mb}
	var alerts []string
	res, err := newGuard(f, now, &alerts).Check()
	if err != nil {
		t.Fatal(err)
	}
	floor := now.Add(-6 * time.Hour)
	if len(f.cutoffs) != 1 || !f.cutoffs[0].Equal(oldest.Add(floor.Sub(oldest)/2)) {
		t.Fatalf("cutoffs %v, want one at the midpoint", f.cutoffs)
	}
	if f.reclaims != 1 || res.Free < 400*mb {
		t.Fatalf("reclaims %d, free %d MB", f.reclaims, res.Free/mb)
	}
	if len(alerts) != 1 || !strings.Contains(alerts[0], "discarded") {
		t.Fatalf("alerts %v", alerts)
	}
}

// TestCheckKeepsRecentRaw: the newest KeepRaw is never discarded; when
// that is all that is left, the guard says so.
func TestCheckKeepsRecentRaw(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	f := &fakeStore{oldest: now.Add(-48 * time.Hour), free: 10 * mb, bytesPerHour: mb / 10}
	var alerts []string
	if _, err := newGuard(f, now, &alerts).Check(); err != nil {
		t.Fatal(err)
	}
	floor := now.Add(-6 * time.Hour)
	for _, c := range f.cutoffs {
		if c.After(floor) {
			t.Fatalf("discarded newer than the floor: %v", c)
		}
	}
	if last := f.cutoffs[len(f.cutoffs)-1]; !last.Equal(floor) {
		t.Fatalf("last cutoff %v, want the floor %v", last, floor)
	}
	if len(alerts) != 2 || !strings.Contains(alerts[1], "critically low") {
		t.Fatalf("alerts %v", alerts)
	}
}

// TestRunHourly: Run checks at start and then every hour (virtual time).
func TestRunHourly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var (
			mu     sync.Mutex
			checks int
		)
		g := &Guard{
			St:      &fakeStore{},
			MinFree: 200 * mb,
			Now:     time.Now,
			Space: func() (uint64, uint64, error) {
				mu.Lock()
				checks++
				mu.Unlock()
				return 1 << 40, 1 << 41, nil
			},
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { g.Run(ctx); close(done) }()
		synctest.Wait()
		time.Sleep(3*time.Hour + time.Minute)
		synctest.Wait()
		cancel()
		<-done
		mu.Lock()
		defer mu.Unlock()
		if checks != 4 {
			t.Fatalf("checks: %d, want 4 (start + 3 hourly)", checks)
		}
	})
}
