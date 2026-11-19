package main

import (
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"foxesslocal/internal/api"
	"foxesslocal/internal/socalert"
	"foxesslocal/internal/socest"
)

// socWatch feeds the live state of charge to the SoC estimate (D-028)
// and the battery alerts (owner, 2026-11-18). The telemetry observer may
// run concurrently; est is read by the API.
type socWatch struct {
	mu  sync.Mutex
	w   socalert.Watch
	est socest.Estimator
	// saved is the energy per percent last handed to a save (under mu);
	// saves are the saves in flight (Shutdown waits for them), saveMu
	// orders them.
	saved  socest.Learned
	saves  sync.WaitGroup
	saveMu sync.Mutex
}

// socLearnedKey is the store key holding the learnt energy per percent
// (socest.Learned, JSON).
const socLearnedKey = "socest.learned"

// restoreSocLearned loads the energy per percent learnt in earlier runs.
func (h *handle) restoreSocLearned() {
	v, ok, err := h.st.Setting(socLearnedKey)
	if err != nil || !ok {
		return
	}
	var l socest.Learned
	if err := json.Unmarshal([]byte(v), &l); err != nil {
		slog.Warn("soc estimate: ignoring the saved energy per percent", "err", err)
		return
	}
	h.soc.est.Restore(l)
	h.soc.mu.Lock()
	h.soc.saved = h.soc.est.Learned()
	h.soc.mu.Unlock()
}

// saveSocLearned saves the energy per percent off the telemetry hook (a
// store write must not stall the relay). Each save writes the newest
// value, so the order the goroutines run in does not matter.
func (h *handle) saveSocLearned() {
	h.soc.saves.Go(func() {
		h.soc.saveMu.Lock()
		defer h.soc.saveMu.Unlock()
		b, err := json.Marshal(h.soc.est.Learned())
		if err == nil {
			err = h.st.SetSetting(socLearnedKey, string(b))
		}
		if err != nil {
			slog.Warn("soc estimate: saving the energy per percent", "err", err)
		}
	})
}

// observeSoc takes one live reading (the SoC and the battery power, W,
// + = charging) with the current settings (a change applies at the next
// reading), publishes the alerts it triggers and returns the estimate,
// rounded as the API carries it (nil while the capacity is not set).
func (h *handle) observeSoc(soc byte, battW int32, now time.Time) *float64 {
	if h.set == nil {
		return nil
	}
	v := h.set.Get()
	est := h.soc.est.Observe(now, int(soc), int(battW), v.CapacityWh)
	p := socalert.Params{Threshold: v.SocThresholdPct, Horizon: v.SocHorizon, Window: v.SocWindow, P: v.SocPValue}
	h.soc.mu.Lock()
	alerts := h.soc.w.Observe(now, int(soc), est, p)
	l := h.soc.est.Learned()
	changed := l != h.soc.saved && h.st != nil
	if changed {
		h.soc.saved = l
	}
	h.soc.mu.Unlock()
	if changed {
		h.saveSocLearned()
	}
	for _, a := range alerts {
		h.bus.PublishSystem(api.SystemEvent{Type: a.Kind, Detail: a.Detail})
	}
	if v.CapacityWh <= 0 {
		return nil
	}
	r := api.RoundPct(est)
	return &r
}
