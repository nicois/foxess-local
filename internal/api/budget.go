package api

import (
	"time"

	"foxesslocal/internal/gateway"
)

// WriteBudget is one setting's write budget now (foxess-control local
// backend, 2026-11-19): at most Limit real writes per rolling WindowS.
// What counts: one confirmed or sent write per request — a PUT /schedule
// is one schedule write however many groups changed; a write the
// inverter already holds (not sent) and a write refused before sending
// count zero; a write sent whose read-back timed out (504, applied
// unknown) counts one.
type WriteBudget struct {
	Limit     int `json:"limit"`
	Remaining int `json:"remaining"`
	WindowS   int `json:"window_s"`
	// NextFreeAt is when the oldest counted write leaves the window
	// (Remaining goes up by one); null while Remaining == Limit.
	NextFreeAt *time.Time `json:"next_free_at"`
}

// kindFeature names each budgeted command by its identity.writes feature.
var kindFeature = map[gateway.CommandKind]string{
	gateway.KindWorkMode:          "work_mode",
	gateway.KindLimits:            "power_limits",
	gateway.KindMeterCompensation: "meter_compensation",
	gateway.KindSchedule:          "schedule",
	gateway.KindCurrentLimits:     "current_limits",
	gateway.KindBatteryCurrents:   "battery_currents",
	gateway.KindPowerLimit:        "power_limit",
	gateway.KindPortLimits:        "port_limits",
	gateway.KindEPSMode:           "eps_mode",
}

// KindFeature is a command kind's feature name (the config's
// [writes.per_setting] keys and the API's).
func KindFeature(k gateway.CommandKind) string { return kindFeature[k] }

// FeatureKind is the command kind for a feature name.
func FeatureKind(feature string) (gateway.CommandKind, bool) {
	for k, f := range kindFeature {
		if f == feature {
			return k, true
		}
	}
	return "", false
}

// BudgetView maps the gateway's budget to the API's (budgeted settings
// only).
func BudgetView(b map[gateway.CommandKind]gateway.BudgetState) map[string]WriteBudget {
	out := map[string]WriteBudget{}
	for k, s := range b {
		name, ok := kindFeature[k]
		if !ok {
			continue
		}
		out[name] = WriteBudget{Limit: s.Limit, Remaining: s.Remaining, WindowS: int(s.Window / time.Second), NextFreeAt: s.NextFree}
	}
	return out
}

// writeBudget is the budget for /state: the host's provider, else this
// server's gateway, else none.
func (s *Server) writeBudget() map[string]WriteBudget {
	switch {
	case s.info.WriteBudget != nil:
		return s.info.WriteBudget()
	case s.gw != nil:
		return BudgetView(s.gw.Budget())
	}
	return map[string]WriteBudget{}
}
