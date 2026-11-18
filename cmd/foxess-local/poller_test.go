package main

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// TestStopPollerWaitsForExit: Shutdown's first step stops the poller
// and waits for its goroutine — the poller writes to the store, and a
// write after st.Close panics (DEF-007 class). It must also hold on the
// fatal path, where the root ctx is NOT cancelled before Shutdown.
func TestStopPollerWaitsForExit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := &handle{}
		var exited atomic.Bool
		h.startPoller(t.Context(), func(ctx context.Context) {
			<-ctx.Done()
			time.Sleep(time.Second) // a tick finishing its store write
			exited.Store(true)
		})
		h.stopPoller()
		if !exited.Load() {
			t.Fatal("stopPoller returned before the poller exited")
		}
		h.stopPoller() // idempotent
	})
}

// TestStopPollerNeverStarted: Shutdown on a handle without a poller
// (tests, handles that never Start) is a no-op.
func TestStopPollerNeverStarted(_ *testing.T) {
	(&handle{}).stopPoller()
}
