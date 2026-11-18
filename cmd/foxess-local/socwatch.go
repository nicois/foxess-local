package main

import (
	"sync"
	"time"

	"foxesslocal/internal/api"
	"foxesslocal/internal/socalert"
)

// socWatch feeds the live state of charge to the battery alerts (owner,
// 2026-11-18). The telemetry observer may run concurrently.
type socWatch struct {
	mu sync.Mutex
	w  socalert.Watch
}

// observeSoc takes one live reading with the current alert settings (a
// change applies at the next reading) and publishes what it triggers.
func (h *handle) observeSoc(soc byte, now time.Time) {
	if h.set == nil {
		return
	}
	v := h.set.Get()
	p := socalert.Params{Threshold: v.SocThresholdPct, Horizon: v.SocHorizon, Window: v.SocWindow, P: v.SocPValue}
	h.soc.mu.Lock()
	alerts := h.soc.w.Observe(now, int(soc), p)
	h.soc.mu.Unlock()
	for _, a := range alerts {
		h.bus.PublishSystem(api.SystemEvent{Type: a.Kind, Detail: a.Detail})
	}
}
