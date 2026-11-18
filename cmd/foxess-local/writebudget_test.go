package main

import (
	"testing"
	"time"

	"foxesslocal/internal/config"
	"foxesslocal/internal/gateway"
)

// TestBudgetOptions (foxess-control local backend, 2026-11-19): the write
// budget is configurable — [writes] per_hour (default 10) and per-setting
// overrides by feature name; an unknown name is refused at start.
func TestBudgetOptions(t *testing.T) {
	gw := gateway.New(nil, time.Now, mustBudget(t, config.Writes{})...)
	if b := gw.Budget()[gateway.KindSchedule]; b.Limit != 10 {
		t.Fatalf("default: %+v", b)
	}
	n := 4
	gw = gateway.New(nil, time.Now, mustBudget(t, config.Writes{PerHour: &n, PerSetting: map[string]int{"schedule": 30, "work_mode": 0}})...)
	v := gw.Budget()
	if v[gateway.KindSchedule].Limit != 30 || v[gateway.KindLimits].Limit != 4 {
		t.Fatalf("overrides: %+v", v)
	}
	if _, ok := v[gateway.KindWorkMode]; ok {
		t.Fatal("per_setting 0 leaves the setting unbudgeted")
	}
	if _, err := budgetOptions(config.Writes{PerSetting: map[string]int{"warp_drive": 1}}); err == nil {
		t.Fatal("an unknown setting was accepted")
	}
}

func mustBudget(t *testing.T, w config.Writes) []gateway.Option {
	t.Helper()
	opts, err := budgetOptions(w)
	if err != nil {
		t.Fatal(err)
	}
	return opts
}
