// Package liveness decides whether the inverter is online — a session is
// up and its telemetry is fresh — and publishes every change. The one
// tracker feeds both /state's online flag and the SSE connection
// events, so the web UI's header and a fresh /state never disagree.
package liveness

import (
	"context"
	"sync"
	"time"
)

// Tracker is the online state machine.
type Tracker struct {
	window  time.Duration
	clock   func() time.Time
	publish func(online bool)

	mu       sync.Mutex
	sessions map[string]bool
	last     time.Time // newest telemetry
	online   bool
}

// New returns a tracker: online while a session is up and telemetry
// arrived within window; publish is called on each change (under the
// tracker's lock: it must be cheap and must not call back).
func New(window time.Duration, clock func() time.Time, publish func(online bool)) *Tracker {
	return &Tracker{window: window, clock: clock, publish: publish, sessions: map[string]bool{}}
}

// SessionUp records an admitted session (no data yet: not online).
func (t *Tracker) SessionUp(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sessions[id] = true
	t.updateLocked()
}

// SessionDown records a closed session (a rejected one never opened).
func (t *Tracker) SessionDown(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.sessions, id)
	t.updateLocked()
}

// Telemetry records one live sample.
func (t *Tracker) Telemetry() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.last = t.clock()
	t.updateLocked()
}

// Tick re-evaluates the staleness (the Run loop's step).
func (t *Tracker) Tick() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.updateLocked()
}

// Online reports the state now (re-evaluated at the call).
func (t *Tracker) Online() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.updateLocked()
	return t.online
}

// Run ticks every period until ctx is done, so silence is noticed
// without a reader asking.
func (t *Tracker) Run(ctx context.Context, period time.Duration) {
	tk := time.NewTicker(period)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			t.Tick()
		}
	}
}

func (t *Tracker) updateLocked() {
	v := len(t.sessions) > 0 && !t.last.IsZero() && t.clock().Sub(t.last) <= t.window
	if v == t.online {
		return
	}
	t.online = v
	if t.publish != nil {
		t.publish(v)
	}
}
