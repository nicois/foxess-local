package api

import "time"

// Energy is the inverter's lifetime energy counters in kWh (foxess-control
// local backend, 2026-11-19): monotonic, never decreasing — a counter that
// went backwards (inverter replaced, firmware reset) is reported with a
// counter-reset system event. A field is null until its first read, and
// null when this model does not report it.
type Energy struct {
	GridExportKWh       *float64 `json:"grid_export_kwh"`
	GridImportKWh       *float64 `json:"grid_import_kwh"`
	PVKWh               *float64 `json:"pv_kwh"`
	BatteryChargeKWh    *float64 `json:"battery_charge_kwh"`
	BatteryDischargeKWh *float64 `json:"battery_discharge_kwh"`
	LoadKWh             *float64 `json:"load_kwh"`
	OutputKWh           *float64 `json:"output_kwh"`
	// AsOf is when the counters were last read (null: never).
	AsOf *time.Time `json:"as_of"`
	// Sources says where each reported field comes from: "register" (the
	// inverter's own lifetime total) or "integrated" (power integrated
	// here). Fields that are not reported are absent.
	Sources map[string]string `json:"sources"`
}

// SystemCounterReset is the system event for a lifetime energy counter
// that went backwards (the field name is the detail): the integration
// re-baselines instead of booking a negative delta.
const SystemCounterReset = "counter-reset"
