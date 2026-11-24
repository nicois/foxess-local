// Package state decides what the stick should be doing (design D-031,
// "States" and "When the hotspot opens"). It is pure: each Step takes
// an observation, with the time in it, and returns a decision. The
// stick has no battery-backed clock, so the times are the process's
// monotonic clock, never wall time.
package state

import (
	"slices"
	"time"

	"foxesslocal/internal/wizard/health"
	"foxesslocal/internal/wizard/led"
	"foxesslocal/internal/wizard/netmgr"
)

// The timings from the design.
const (
	homeRetry      = 10 * time.Minute // retry before the first hotspot window
	hotspotWindow  = 5 * time.Minute  // held longer while an operator is connected
	windowRetry    = 2 * time.Minute  // between windows
	appDownGrace   = 2 * time.Minute
	inverterSilent = 10 * time.Minute
)

// State is what the wizard reports.
type State int

// The states. Phase 1 has no logger setup, revert or stopped state.
const (
	Unconfigured     State = iota // no home credentials
	Joining                       // an operator's join is running
	Normal                        // on the home network, all well
	Degraded                      // on the home network, something needs attention
	HomeLost                      // credentials known, home network unreachable, retrying
	Hotspot                       // the hotspot is open for an operator
	AwaitingInverter              // on the home network; the inverter has never connected
)

func (s State) String() string {
	switch s {
	case Unconfigured:
		return "unconfigured"
	case Joining:
		return "joining"
	case Normal:
		return "normal"
	case Degraded:
		return "degraded"
	case HomeLost:
		return "home_lost"
	case Hotspot:
		return "hotspot"
	case AwaitingInverter:
		return "awaiting_inverter"
	}
	return "unknown"
}

// Radio is what the radio should do.
type Radio int

// The radio requests.
const (
	RadioHold    Radio = iota // leave it: an operator action owns it
	RadioHome                 // the hotspot down; NetworkManager autoconnects home
	RadioHotspot              // the hotspot up
)

// Problem is one thing the status page explains.
type Problem string

// The problems.
const (
	ProblemAppDown    Problem = "app_down"    // foxess-local not answering, or its unit failed
	ProblemNoInverter Problem = "no_inverter" // the inverter stopped sending readings
)

// Observation is what the loop saw.
type Observation struct {
	Now             time.Time
	HomeKnown       bool        // home credentials are stored
	Role            netmgr.Role // the radio's current role
	HomeAddrPresent bool        // the home connection has an IPv4 address
	Health          health.Report
	InverterEver    bool // the inverter has connected at least once (persisted)
	Operator        bool // an operator has a session on the hotspot
	Requested       bool // three power-cycles, or the page, asked for the hotspot
	Busy            bool // an operator action owns the radio
}

// Decision is what the stick should do and show.
type Decision struct {
	State    State
	Radio    Radio
	LED      led.Pattern
	Problems []Problem
}

// Has reports whether p is among the decision's problems.
func (d Decision) Has(p Problem) bool { return slices.Contains(d.Problems, p) }

// Machine carries the timers between steps.
type Machine struct {
	lostSince     time.Time // start of the first retry after losing home; zero at home
	retrySince    time.Time // start of a retry between hotspot windows
	windowSince   time.Time // the open hotspot window's start; zero when closed
	appDownSince  time.Time
	inverterSince time.Time // inverter silent since
}

// New is a machine at rest.
func New() *Machine { return &Machine{} }

// Step decides for one observation.
func (m *Machine) Step(o Observation) Decision {
	switch {
	case o.Busy:
		return Decision{State: Joining, Radio: RadioHold, LED: led.Pattern{Green: led.Fast}}
	case !o.HomeKnown:
		return Decision{State: Unconfigured, Radio: RadioHotspot, LED: hotspotLED(o)}
	case o.Requested:
		m.openWindow(o.Now)
	}
	if !m.windowSince.IsZero() {
		if o.Operator || o.Now.Before(m.windowSince.Add(hotspotWindow)) {
			return Decision{State: Hotspot, Radio: RadioHotspot, LED: hotspotLED(o)}
		}
		m.windowSince = time.Time{}
		m.retrySince = o.Now
	}
	if o.Role == netmgr.RoleHome && o.HomeAddrPresent {
		m.lostSince, m.retrySince = time.Time{}, time.Time{}
		return m.atHome(o)
	}
	return m.offHome(o)
}

// Joined closes any hotspot window and its retries: an operator has
// just put the stick on a network that works.
func (m *Machine) Joined() {
	m.windowSince, m.retrySince, m.lostSince = time.Time{}, time.Time{}, time.Time{}
}

func (m *Machine) openWindow(now time.Time) {
	m.windowSince = now
	m.retrySince = time.Time{}
}

func (m *Machine) offHome(o Observation) Decision {
	retrying := Decision{State: HomeLost, Radio: RadioHome, LED: led.Pattern{Green: led.Fast}}
	if !m.retrySince.IsZero() {
		if o.Now.Before(m.retrySince.Add(windowRetry)) {
			return retrying
		}
	} else {
		if m.lostSince.IsZero() {
			m.lostSince = o.Now
		}
		if o.Now.Before(m.lostSince.Add(homeRetry)) {
			return retrying
		}
	}
	m.openWindow(o.Now)
	return Decision{State: Hotspot, Radio: RadioHotspot, LED: hotspotLED(o)}
}

func (m *Machine) atHome(o Observation) Decision {
	var problems []Problem
	if !o.Health.Live || o.Health.UnitFailed {
		if since(&m.appDownSince, o.Now) >= appDownGrace {
			problems = append(problems, ProblemAppDown)
		}
		m.inverterSince = time.Time{} // unknowable while the app is down
	} else {
		m.appDownSince = time.Time{}
		switch {
		case o.Health.InverterSeen():
			m.inverterSince = time.Time{}
		case !o.InverterEver:
			return Decision{State: AwaitingInverter, Radio: RadioHome, LED: led.Pattern{Green: led.On, Blue: led.Slow}}
		case since(&m.inverterSince, o.Now) >= inverterSilent:
			problems = append(problems, ProblemNoInverter)
		}
	}
	if len(problems) > 0 {
		return Decision{State: Degraded, Radio: RadioHome, LED: led.Pattern{Green: led.On, Red: led.Slow}, Problems: problems}
	}
	return Decision{State: Normal, Radio: RadioHome, LED: led.Pattern{Green: led.On}}
}

// since starts *t at now if it is unset, and returns how long ago it is.
func since(t *time.Time, now time.Time) time.Duration {
	if t.IsZero() {
		*t = now
	}
	return now.Sub(*t)
}

func hotspotLED(o Observation) led.Pattern {
	if o.Operator {
		return led.Pattern{Red: led.On}
	}
	return led.Pattern{Red: led.Slow}
}
