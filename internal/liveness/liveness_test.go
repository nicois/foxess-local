package liveness

import (
	"context"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

type rec struct {
	mu  sync.Mutex
	got []bool
}

func (r *rec) publish(v bool) { r.mu.Lock(); r.got = append(r.got, v); r.mu.Unlock() }
func (r *rec) events() []bool { r.mu.Lock(); defer r.mu.Unlock(); return slices.Clone(r.got) }

// TestTransitions: online needs a session and fresh telemetry; each
// change is published once; the session closing is offline at once.
func TestTransitions(t *testing.T) {
	now := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	r := &rec{}
	l := New(15*time.Second, func() time.Time { return now }, r.publish)

	l.SessionUp("c1")
	if l.Online() || len(r.events()) != 0 {
		t.Fatal("a session without data is not online")
	}
	l.Telemetry()
	l.Telemetry()
	if !l.Online() || !slices.Equal(r.events(), []bool{true}) {
		t.Fatalf("after telemetry: online %v, events %v", l.Online(), r.events())
	}
	now = now.Add(16 * time.Second)
	l.Tick()
	if l.Online() || !slices.Equal(r.events(), []bool{true, false}) {
		t.Fatalf("stale: online %v, events %v", l.Online(), r.events())
	}
	l.Telemetry()
	l.SessionDown("c1")
	if l.Online() || !slices.Equal(r.events(), []bool{true, false, true, false}) {
		t.Fatalf("session down: online %v, events %v", l.Online(), r.events())
	}
	// a rejected connection closes without ever opening
	l.SessionDown("rejected")
	l.Telemetry()
	if l.Online() {
		t.Fatal("telemetry without a session is not online")
	}
}

// TestOnlineReadsTheClock: Online is evaluated at call time (a /state
// read between ticks sees the staleness, and the change is published).
func TestOnlineReadsTheClock(t *testing.T) {
	now := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	r := &rec{}
	l := New(15*time.Second, func() time.Time { return now }, r.publish)
	l.SessionUp("c1")
	l.Telemetry()
	now = now.Add(20 * time.Second)
	if l.Online() || !slices.Equal(r.events(), []bool{true, false}) {
		t.Fatalf("online %v events %v", l.Online(), r.events())
	}
}

// TestRun: the ticker declares a silent inverter offline within a tick
// of the window (virtual time).
func TestRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := &rec{}
		l := New(15*time.Second, time.Now, r.publish)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { l.Run(ctx, time.Second); close(done) }()
		l.SessionUp("c1")
		l.Telemetry()
		time.Sleep(15 * time.Second)
		synctest.Wait()
		if !slices.Equal(r.events(), []bool{true}) {
			t.Fatalf("at 15 s: %v", r.events())
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if !slices.Equal(r.events(), []bool{true, false}) {
			t.Fatalf("at 17 s: %v", r.events())
		}
		cancel()
		<-done
	})
}
