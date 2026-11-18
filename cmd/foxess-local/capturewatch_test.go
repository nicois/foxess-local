package main

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"foxesslocal/internal/api"
	"foxesslocal/internal/wincap"
)

// TestWatchCaptures: a window that expires or is closed ends its capture.
func TestWatchCaptures(t *testing.T) {
	for _, typ := range []string{api.SystemMaintenanceExpired, api.SystemMaintenanceClosed} {
		synctest.Test(t, func(t *testing.T) {
			c := wincap.New(t.TempDir(), 1<<20, time.Now, "test")
			if _, err := c.Start(); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ev := make(chan api.Event, 1)
			go watchCaptures(ctx, ev, c)
			ev <- api.Event{Kind: api.EventSystem, Data: api.SystemEvent{Type: api.SystemMaintenanceExtended}}
			synctest.Wait()
			if !c.Active() {
				t.Fatal("a re-arm ended the capture")
			}
			ev <- api.Event{Kind: api.EventSystem, Data: api.SystemEvent{Type: typ}}
			synctest.Wait()
			if c.Active() {
				t.Fatalf("%s: still capturing", typ)
			}
		})
	}
}
