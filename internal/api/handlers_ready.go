package api

import (
	"net/http"
)

// Readiness statuses (the /readyz body's status).
const (
	readyReady    = "ready"     // every check ok (or pending)
	readyDegraded = "degraded"  // a warning: serving, but something needs attention
	readyNot      = "not_ready" // a failing check: 503
)

// WithReadyChecks adds the host's own readiness checks (the inverter
// listener, disk space) — facts only the host can see.
func WithReadyChecks(f func() []Check) ServerOption {
	return func(s *Server) { s.readyChecks = f }
}

// WithNoInverter tells the server whether the host was installed without
// an inverter address (installer --no-inverter, D-031): until one is
// set, "not connected" is expected, a warning rather than a failure.
func WithNoInverter(f func() bool) ServerOption {
	return func(s *Server) { s.noInverter = f }
}

type readyView struct {
	Status string  `json:"status"`
	Checks []Check `json:"checks"`
}

// handleReady is GET /readyz (F3, audit R2): readiness, unlike /healthz
// (liveness: the process answers). 503 while any check fails — the
// inverter has not connected or its readings went stale, the store
// cannot write, a host check (the inverter listener) fails; 200 with
// status "degraded" on a warning. Open like /healthz for monitors: a
// tokenless request sees each check's id, title and status, any valid
// token also its detail and fix.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	checks, err := s.readiness()
	if err != nil {
		s.internal(w, err)
		return
	}
	out := readyView{Status: readyReady, Checks: checks}
	for _, c := range checks {
		switch c.Status {
		case checkFail:
			out.Status = readyNot
		case checkWarn:
			if out.Status == readyReady {
				out.Status = readyDegraded
			}
		}
	}
	if _, ok := s.authenticate(r); !ok {
		for i := range out.Checks {
			out.Checks[i].Detail, out.Checks[i].Fix = "", ""
		}
	}
	code := http.StatusOK
	if out.Status == readyNot {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, out)
}

// readiness gathers the checks: the host's first (the listener comes
// before anything it feeds), then the inverter, the store and the clock.
func (s *Server) readiness() ([]Check, error) {
	qs, err := s.st.QueryState()
	if err != nil {
		return nil, err
	}
	st, err := s.buildState(qs)
	if err != nil {
		return nil, err
	}
	var out []Check
	if s.readyChecks != nil {
		out = append(out, s.readyChecks()...)
	}
	clock := s.checkClock()
	switch {
	case s.siteZone() == nil:
		clock.Status = checkPending // a setup choice, not a fault
	case clock.Status == checkFail:
		clock.Status = checkWarn // the inverter's clock never makes this service "not ready"
	}
	return append(out, s.checkConnected(st), s.checkStore(), clock), nil
}

// checkStore: every batch write has landed (the first failure sticks:
// a store that failed once may have lost data).
func (s *Server) checkStore() Check {
	c := Check{ID: "store", Title: "Database writes"}
	if err := s.st.Err(); err != nil {
		c.Status, c.Detail = checkFail, "a write failed: "+err.Error()
		c.Fix = "Check free space and the journal (`journalctl -u foxess-local`), then restart the service."
		return c
	}
	c.Status, c.Detail = checkOK, "every write has landed"
	return c
}
