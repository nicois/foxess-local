package main

import (
	"context"

	"foxesslocal/internal/api"
	"foxesslocal/internal/wincap"
)

// watchCaptures ends a maintenance window's capture when the window
// expires or is closed (the API's close stops it too; expiry is only
// seen here).
func watchCaptures(ctx context.Context, events <-chan api.Event, c *wincap.Capturer) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			if d, ok := ev.Data.(api.SystemEvent); ok &&
				(d.Type == api.SystemMaintenanceExpired || d.Type == api.SystemMaintenanceClosed) {
				c.Stop()
			}
		}
	}
}

// startCaptures runs the capture watcher.
func (h *handle) startCaptures(ctx context.Context) {
	if h.capt == nil {
		return
	}
	sub := h.bus.Subscribe()
	go func() {
		defer sub.Close()
		watchCaptures(ctx, sub.Events(), h.capt)
	}()
}
