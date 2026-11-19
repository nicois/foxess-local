package siteclock

import (
	"context"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// fakeSite scripts a Syncer's seams.
type fakeSite struct {
	zone    *time.Location
	rtc     [6]int
	readAt  time.Time
	read    bool
	synced  bool
	online  bool
	writes  [][6]int
	audits  []string
	failSet error
}

func (f *fakeSite) syncer(clock func() time.Time) *Syncer {
	return &Syncer{
		Zone:       func() *time.Location { return f.zone },
		Reading:    func() ([6]int, time.Time, bool) { return f.rtc, f.readAt, f.read },
		HostSynced: func() bool { return f.synced },
		Online:     func() bool { return f.online },
		Set: func(_ context.Context, w [6]int) error {
			f.writes = append(f.writes, w)
			return f.failSet
		},
		Audit: func(d string) { f.audits = append(f.audits, d) },
		Clock: clock,
	}
}

// show makes the inverter's clock read the site's wall time + off, now.
func (f *fakeSite) show(now time.Time, off time.Duration) {
	f.rtc, f.readAt, f.read = Wall(f.zone, now.Add(off)), now, true
}

// TestSyncerCorrects (owner, 2026-11-19, option b): drift and a missed
// daylight-saving change are corrected — the site's wall time from the
// host clock — once per reading, at most hourly, and only from a
// synchronised host clock with a session to write on.
func TestSyncerCorrects(t *testing.T) {
	now := time.Date(2026, 11, 19, 5, 17, 26, 0, time.UTC)
	f := &fakeSite{synced: true, online: true}
	f.zone, _ = Zone("Australia/Melbourne")
	s := f.syncer(func() time.Time { return now })

	f.show(now, 3*time.Minute) // drift
	s.Tick(context.Background())
	if len(f.writes) != 1 || f.writes[0] != Wall(f.zone, now) {
		t.Fatalf("drift: writes %v", f.writes)
	}
	if len(f.audits) != 1 || !strings.Contains(f.audits[0], "3 min ahead") {
		t.Fatalf("audits %v", f.audits)
	}
	// the same (pre-write) reading again: no second write
	s.Tick(context.Background())
	if len(f.writes) != 1 {
		t.Fatalf("re-wrote on the old reading: %v", f.writes)
	}
	// a fresh reading still off, within the hour: no write
	now = now.Add(10 * time.Minute)
	f.show(now, 2*time.Minute)
	s.Tick(context.Background())
	if len(f.writes) != 1 {
		t.Fatalf("wrote twice within the hour: %v", f.writes)
	}
	// an hour on: a missed daylight-saving change (an hour behind)
	now = now.Add(time.Hour)
	f.show(now, -time.Hour)
	s.Tick(context.Background())
	if len(f.writes) != 2 {
		t.Fatalf("zone shift not corrected: %v", f.writes)
	}
}

// TestSyncerHolds: no zone, a stale reading, an unsynchronised host
// clock, no session, or an unexplained offset — no write. An
// unexplained offset is audited once, not every minute.
func TestSyncerHolds(t *testing.T) {
	now := time.Date(2026, 11, 19, 5, 17, 26, 0, time.UTC)
	mel, _ := Zone("Australia/Melbourne")
	for name, set := range map[string]func(f *fakeSite){
		"no zone":       func(f *fakeSite) { f.zone = nil },
		"stale reading": func(f *fakeSite) { f.readAt = now.Add(-Fresh - time.Minute) },
		"host unsynced": func(f *fakeSite) { f.synced = false },
		"no session":    func(f *fakeSite) { f.online = false },
		"in step":       func(f *fakeSite) { f.show(now, 10*time.Second) },
		"11 h ahead":    func(f *fakeSite) { f.show(now, 11*time.Hour) },
	} {
		f := &fakeSite{zone: mel, synced: true, online: true}
		f.show(now, 3*time.Minute)
		set(f)
		s := f.syncer(func() time.Time { return now })
		s.Tick(context.Background())
		s.Tick(context.Background())
		if len(f.writes) != 0 {
			t.Errorf("%s: wrote %v", name, f.writes)
		}
		if name == "11 h ahead" && (len(f.audits) != 1 || !strings.Contains(f.audits[0], "not corrected automatically")) {
			t.Errorf("unexplained: audits %v", f.audits)
		}
	}
}

// TestSyncerRun: the loop ticks every minute until cancelled (virtual
// time, C-004).
func TestSyncerRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeSite{synced: true, online: true}
		f.zone, _ = Zone("Australia/Melbourne")
		f.show(time.Now(), 3*time.Minute)
		s := f.syncer(time.Now)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { s.Run(ctx); close(done) }()
		time.Sleep(SyncEvery + time.Second)
		synctest.Wait()
		if len(f.writes) != 1 {
			t.Errorf("after one interval: %d writes", len(f.writes))
		}
		cancel()
		<-done
	})
}
