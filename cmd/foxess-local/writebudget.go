package main

import (
	"fmt"
	"time"

	"foxesslocal/internal/api"
	"foxesslocal/internal/config"
	"foxesslocal/internal/gateway"
)

// budgetOptions turns [writes] into the gateway's budget: per_hour for
// every setting, per_setting overrides by feature name (an unknown name
// is a configuration error, not a silent no-op).
func budgetOptions(w config.Writes) ([]gateway.Option, error) {
	over := map[gateway.CommandKind]int{}
	for name, n := range w.PerSetting {
		k, ok := api.FeatureKind(name)
		if !ok {
			return nil, fmt.Errorf("config: [writes.per_setting] %q is not a setting (work_mode, schedule, power_limits, current_limits, meter_compensation, battery_currents, power_limit, port_limits, eps_mode)", name)
		}
		if n < 0 {
			return nil, fmt.Errorf("config: [writes.per_setting] %s must be >= 0", name)
		}
		over[k] = n
	}
	if w.Hourly() < 0 {
		return nil, fmt.Errorf("config: [writes] per_hour must be >= 0")
	}
	return []gateway.Option{gateway.WithWriteBudget(w.Hourly(), time.Hour), gateway.WithWriteBudgetFor(over)}, nil
}
