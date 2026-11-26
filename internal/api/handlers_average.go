package api

import (
	"net/http"
	"time"
)

// The accepted trailing windows for /power/average.
const (
	avg5m  = 5 * time.Minute
	avg15m = 15 * time.Minute
	avg30m = 30 * time.Minute
)

var averageWindows = map[string]time.Duration{
	"5m": avg5m, label15m: avg15m, "30m": avg30m, "1h": time.Hour,
}

// sampleCadence is the live-value cadence (the register poll / 150 B
// telemetry: one sample per 5 s) — the expected count for a full window.
const sampleCadence = 5 * time.Second

// averageBody is GET /power/average: the mean power over a trailing
// window. Power is null when the window holds no samples (never zeros
// that read as "nothing flowing").
type averageBody struct {
	Window   string    `json:"window"`
	From     time.Time `json:"from"`
	To       time.Time `json:"to"`
	Samples  int       `json:"samples"`
	Expected int       `json:"expected_samples"`
	Power    *Power    `json:"power"`
}

// handlePowerAverage is GET /power/average?window=5m|15m|30m|1h
// (read-only): the "Now" panel's averaged view — smooths spikes for
// reading the current state and for extrapolating (time to empty/full).
func (s *Server) handlePowerAverage(w http.ResponseWriter, r *http.Request, _ Token) {
	label := r.URL.Query().Get("window")
	d, ok := averageWindows[label]
	if !ok {
		writeProblem(w, http.StatusBadRequest, badField("window", "must be one of 5m, 15m, 30m, 1h"))
		return
	}
	to := s.clock()
	from := to.Add(-d)
	body := averageBody{Window: label, From: from, To: to, Expected: int(d / sampleCadence)}
	a, ok, err := s.st.WindowAverage(from, to)
	if err != nil {
		s.internal(w, err)
		return
	}
	if ok {
		body.Samples = a.N
		body.Power = &Power{
			PVW: a.SolAvg, GridW: a.GridAvg, LoadW: a.HausAvg,
			AcPortW: a.AcportAvg, BatteryW: a.BattAvg, EpsW: a.EpsAvg,
			Meter2W: nullInt(a.Meter2),
		}
	}
	writeJSON(w, http.StatusOK, body)
}
