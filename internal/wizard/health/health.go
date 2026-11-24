// Package health is the wizard's view of foxess-local (design D-031,
// "Triggers for degraded"): is the process answering, is its unit
// failed, and does /readyz see the inverter. It reads the tokenless
// views only: the wizard holds no foxess-local token.
package health

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultURL is foxess-local's API on the stick.
const DefaultURL = "http://127.0.0.1:18080"

// Unit is foxess-local's systemd unit.
const Unit = "foxess-local.service"

// The /readyz id and statuses the wizard reads (pinned to internal/api
// by TestPinnedToAPI).
const (
	checkInverter = "inverter_connected"
	statusOK      = "ok"
	statusPending = "pending"
)

// requestTimeout bounds each probe request; foxess-local answers
// locally in milliseconds.
const requestTimeout = 5 * time.Second

// maxBody bounds what the wizard reads from /readyz.
const maxBody = 64 << 10

// Runner runs a command and returns its combined output.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// Prober probes foxess-local.
type Prober struct {
	BaseURL string       // DefaultURL
	HTTP    *http.Client // nil: a client with requestTimeout
	Run     Runner
}

// Report is one probe's result.
type Report struct {
	Live       bool              // /healthz answered
	UnitFailed bool              // systemd reports the unit failed
	Ready      string            // /readyz status; "" when unreachable
	Checks     map[string]string // check id → status
}

// InverterSeen reports whether foxess-local has live readings, or has
// just had some.
func (r Report) InverterSeen() bool {
	s := r.Checks[checkInverter]
	return s == statusOK || s == statusPending
}

// Probe asks foxess-local and systemd. It never fails: what it can't
// learn stays at its zero value.
func (p Prober) Probe(ctx context.Context) Report {
	var r Report
	if p.Run != nil {
		out, _ := p.Run.Run(ctx, "systemctl", "is-failed", Unit)
		r.UnitFailed = strings.TrimSpace(string(out)) == "failed"
	}
	if code, _, err := p.get(ctx, "/healthz"); err == nil && code == http.StatusOK {
		r.Live = true
	}
	code, body, err := p.get(ctx, "/readyz")
	if err != nil || (code != http.StatusOK && code != http.StatusServiceUnavailable) {
		return r
	}
	var v struct {
		Status string `json:"status"`
		Checks []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"checks"`
	}
	if json.Unmarshal(body, &v) != nil {
		return r
	}
	r.Ready = v.Status
	r.Checks = make(map[string]string, len(v.Checks))
	for _, c := range v.Checks {
		r.Checks[c.ID] = c.Status
	}
	return r
}

func (p Prober) get(ctx context.Context, path string) (int, []byte, error) {
	c := p.HTTP
	if c == nil {
		c = &http.Client{Timeout: requestTimeout}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.BaseURL+path, nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	return resp.StatusCode, b, err
}
