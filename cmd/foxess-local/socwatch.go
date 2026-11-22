package main

import (
	"encoding/json"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"foxesslocal/internal/api"
	"foxesslocal/internal/frame"
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
	// fd reports the force discharge running now (api.Server's
	// ForceDischargeNow, set once the API is built): during one the
	// forecast runs the measured fall until the discharge is expected to
	// end, then the house's usage (owner, 2026-11-20).
	fd atomic.Pointer[func() (time.Duration, int, bool)]
	// fc reports whether a force charge runs within d from now
	// (api.Server's ForceChargeWithin): no forecast-low alert while one
	// comes before the expected breach (owner, 2026-11-22).
	fc atomic.Pointer[func(time.Duration) bool]
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

// socSample takes one live telemetry sample (connID's row at ts): the
// SoC estimate moves, is stored on the row for the history (0.01 %), and
// is returned rounded as the API carries it (nil without a capacity).
func (h *handle) socSample(connID string, ts time.Time, t frame.Tele) *float64 {
	est := h.observeSoc(t.Soc, t.Batt, t.Sol-t.Haus, ts)
	if est != nil && h.st != nil && connID != "" {
		h.st.RecordSocEstimate(connID, ts, int(math.Round(*est*centiPerPct)))
	}
	return est
}

// centiPerPct: the stored estimate is in 0.01 %; pctPerUnit turns a
// fraction of the capacity (per hour) into % (per hour).
const (
	centiPerPct = 100
	pctPerUnit  = 100
)

// anchorSoc takes the BMS's SoC (0.1 %) from a snapshot: the estimate
// continues from it (D-028).
func (h *handle) anchorSoc(at time.Time, deciPct int) {
	h.soc.est.Anchor(at, deciPct)
}

// observeSoc takes one live reading (the SoC, the battery power and
// the solar less the house load — what the battery would carry without
// a forced export — W, + = charging) with the current settings (a change applies at the next
// reading), publishes the alerts it triggers and returns the estimate,
// rounded as the API carries it (nil while the capacity is not set).
func (h *handle) observeSoc(soc byte, battW, selfW int32, now time.Time) *float64 {
	if h.set == nil {
		return nil
	}
	v := h.set.Get()
	est := h.soc.est.Observe(now, int(soc), int(battW), v.CapacityWh)
	p := socalert.Params{Threshold: v.SocThresholdPct, Horizon: v.SocHorizon, Window: v.SocWindow, P: v.SocPValue, Zone: v.SiteZone}
	var sess socalert.Session
	if fd := h.soc.fd.Load(); fd != nil {
		if left, cut, ok := (*fd)(); ok {
			sess = socalert.Session{Forced: true, Left: left, CutoffPct: float64(cut)}
		}
	}
	if fc := h.soc.fc.Load(); fc != nil {
		sess.ChargeWithin = *fc
	}
	if v.CapacityWh > 0 { // the measured rates, %/h
		capPct := float64(v.CapacityWh) / pctPerUnit
		sess.RatesKnown = true
		sess.BattRate = float64(battW) / capPct
		sess.HouseRate = float64(selfW) / capPct
	}
	h.soc.mu.Lock()
	alerts := h.soc.w.Observe(now, int(soc), est, p, sess)
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
