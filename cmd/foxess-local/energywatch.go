package main

import (
	"sync"
	"time"

	"foxesslocal/internal/api"
	"foxesslocal/internal/frame"
)

// energyWatch keeps the lifetime energy counters for /state and the SSE
// snapshot (foxess-control local backend, 2026-11-19). Counters never
// decrease: a lower reading is held until the next read confirms it — a
// single bad read is not a reset — and a confirmed one is reported (the
// host publishes counter-reset) and taken as the new value.
type energyWatch struct {
	mu      sync.Mutex
	cur     *frame.KHEnergy
	at      time.Time
	pending map[string]bool // fields whose last read was below cur
}

// energyField is one counter: its API name and its value in a read.
type energyField struct {
	name string
	get  func(*frame.KHEnergy) *uint32
}

// energyFields are the reported counters (input energy has no API field).
var energyFields = []energyField{
	{"grid_export_kwh", func(e *frame.KHEnergy) *uint32 { return &e.FeedIn }},
	{"grid_import_kwh", func(e *frame.KHEnergy) *uint32 { return &e.GridImport }},
	{"pv_kwh", func(e *frame.KHEnergy) *uint32 { return &e.PV }},
	{"battery_charge_kwh", func(e *frame.KHEnergy) *uint32 { return &e.Charge }},
	{"battery_discharge_kwh", func(e *frame.KHEnergy) *uint32 { return &e.Discharge }},
	{"load_kwh", func(e *frame.KHEnergy) *uint32 { return &e.Load }},
	{"output_kwh", func(e *frame.KHEnergy) *uint32 { return &e.Yield }},
}

// seed sets the baseline from the previous run's last stored reading.
func (w *energyWatch) seed(e frame.KHEnergy, at time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cur == nil {
		w.cur, w.at = &e, at
	}
}

// observe takes one read and returns the fields whose reset it confirms.
func (w *energyWatch) observe(e frame.KHEnergy, at time.Time) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cur == nil {
		w.cur, w.at = &e, at
		return nil
	}
	if w.pending == nil {
		w.pending = map[string]bool{}
	}
	var resets []string
	for _, f := range energyFields {
		cur, got := f.get(w.cur), *f.get(&e)
		switch {
		case got >= *cur:
			*cur = got
			delete(w.pending, f.name)
		case w.pending[f.name]: // lower twice in a row: a real reset
			*cur = got
			delete(w.pending, f.name)
			resets = append(resets, f.name)
		default: // lower once: held until confirmed
			w.pending[f.name] = true
		}
	}
	w.at = at
	return resets
}

// energy is the API view (kWh, one decimal).
func (w *energyWatch) energy() api.Energy {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := api.Energy{Sources: map[string]string{}}
	if w.cur == nil {
		return out
	}
	at := w.at
	out.AsOf = &at
	set := map[string]**float64{
		"grid_export_kwh": &out.GridExportKWh, "grid_import_kwh": &out.GridImportKWh, "pv_kwh": &out.PVKWh,
		"battery_charge_kwh": &out.BatteryChargeKWh, "battery_discharge_kwh": &out.BatteryDischargeKWh,
		"load_kwh": &out.LoadKWh, "output_kwh": &out.OutputKWh,
	}
	for _, f := range energyFields {
		v := float64(*f.get(w.cur)) / 10
		*set[f.name] = &v
		out.Sources[f.name] = "register"
	}
	return out
}

// snapshotEnergy is the counters for an SSE snapshot (nil until read).
func (h *handle) snapshotEnergy() *api.Energy {
	e := h.energy.energy()
	if e.AsOf == nil {
		return nil
	}
	return &e
}

// seedEnergy loads the previous run's last energy reading as the
// baseline (a reset across a restart is then caught too).
func (h *handle) seedEnergy() {
	regs, err := h.st.RegistersRange(frame.KHEnergyAddr, frame.KHEnergyAddr+frame.KHEnergyCount-1)
	if err != nil || len(regs) < frame.KHEnergyCount {
		return
	}
	vals := make([]uint16, frame.KHEnergyCount)
	for i := range vals {
		vals[i] = regs[frame.KHEnergyAddr+i]
	}
	e, err := frame.ParseKHEnergy(vals)
	if err != nil {
		return
	}
	_, at, ok, err := h.st.RegisterRead(frame.KHEnergyAddr)
	if err != nil || !ok {
		return
	}
	h.energy.seed(e, at)
}
