package state

import (
	"testing"
	"time"

	"foxesslocal/internal/wizard/health"
	"foxesslocal/internal/wizard/led"
	"foxesslocal/internal/wizard/netmgr"
)

// inverterCheck is foxess-local's readiness check id (health pins it).
const inverterCheck = "inverter_connected"

var t0 = time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)

func at(d time.Duration) time.Time { return t0.Add(d) }

// home is an observation of a stick on the home network with a healthy
// foxess-local that sees the inverter.
func home(now time.Time) Observation {
	return Observation{
		Now:             now,
		HomeKnown:       true,
		Role:            netmgr.RoleHome,
		Health:          health.Report{Live: true, Ready: "ready", Checks: map[string]string{inverterCheck: "ok"}},
		InverterEver:    true,
		HomeAddrPresent: true,
	}
}

func offHome(now time.Time) Observation {
	o := home(now)
	o.Role = netmgr.RoleNone
	o.HomeAddrPresent = false
	return o
}

func step(t *testing.T, m *Machine, o Observation, want State, wantRadio Radio) Decision {
	t.Helper()
	d := m.Step(o)
	if d.State != want || d.Radio != wantRadio {
		t.Fatalf("at +%s: state %v radio %v, want %v %v (problems %v)", o.Now.Sub(t0), d.State, d.Radio, want, wantRadio, d.Problems)
	}
	return d
}

func TestUnconfiguredOpensHotspotAtOnce(t *testing.T) {
	m := New()
	o := Observation{Now: t0}
	d := step(t, m, o, Unconfigured, RadioHotspot)
	if d.LED != (led.Pattern{Red: led.Slow}) {
		t.Fatalf("LED %+v", d.LED)
	}
	o.Role, o.Operator = netmgr.RoleHotspot, true
	d = step(t, m, o, Unconfigured, RadioHotspot)
	if d.LED != (led.Pattern{Red: led.On}) {
		t.Fatalf("operator connected: LED %+v", d.LED)
	}
	// it stays open indefinitely
	o.Now, o.Operator = at(24*time.Hour), false
	step(t, m, o, Unconfigured, RadioHotspot)
}

func TestNormal(t *testing.T) {
	m := New()
	d := step(t, m, home(t0), Normal, RadioHome)
	if d.LED != (led.Pattern{Green: led.On}) || len(d.Problems) != 0 {
		t.Fatalf("decision %+v", d)
	}
}

// TestHomeLost: 10 minutes of retries, then a 5-minute hotspot window
// and a 2-minute retry, alternating; the window is held while an
// operator is connected; the home network coming back ends it all.
func TestHomeLost(t *testing.T) {
	m := New()
	step(t, m, home(t0), Normal, RadioHome)
	d := step(t, m, offHome(at(time.Minute)), HomeLost, RadioHome)
	if d.LED != (led.Pattern{Green: led.Fast}) {
		t.Fatalf("retrying LED %+v", d.LED)
	}
	step(t, m, offHome(at(10*time.Minute+59*time.Second)), HomeLost, RadioHome)
	step(t, m, offHome(at(11*time.Minute)), Hotspot, RadioHotspot)
	step(t, m, offHome(at(15*time.Minute+59*time.Second)), Hotspot, RadioHotspot)
	step(t, m, offHome(at(16*time.Minute)), HomeLost, RadioHome)
	step(t, m, offHome(at(17*time.Minute+59*time.Second)), HomeLost, RadioHome)
	step(t, m, offHome(at(18*time.Minute)), Hotspot, RadioHotspot)
	// an operator keeps the window open past its 5 minutes
	o := offHome(at(23 * time.Minute))
	o.Operator = true
	step(t, m, o, Hotspot, RadioHotspot)
	o.Now = at(40 * time.Minute)
	step(t, m, o, Hotspot, RadioHotspot)
	// they leave: the window ends, the retry starts
	o.Operator, o.Now = false, at(40*time.Minute+15*time.Second)
	step(t, m, o, HomeLost, RadioHome)
	// the home network is back
	step(t, m, home(at(41*time.Minute)), Normal, RadioHome)
	// and a later loss starts the 10 minutes afresh
	step(t, m, offHome(at(50*time.Minute)), HomeLost, RadioHome)
	step(t, m, offHome(at(59*time.Minute)), HomeLost, RadioHome)
}

// TestRequestedHotspot: three power-cycles (or the page) open the
// hotspot on a working home network, for 5 minutes or while an
// operator is connected; then the stick goes home again.
func TestRequestedHotspot(t *testing.T) {
	m := New()
	o := home(t0)
	o.Requested = true
	step(t, m, o, Hotspot, RadioHotspot)
	o.Requested, o.Role, o.HomeAddrPresent = false, netmgr.RoleHotspot, false
	o.Now = at(4 * time.Minute)
	step(t, m, o, Hotspot, RadioHotspot)
	o.Operator, o.Now = true, at(9*time.Minute)
	step(t, m, o, Hotspot, RadioHotspot)
	o.Operator, o.Now = false, at(9*time.Minute+15*time.Second)
	step(t, m, o, HomeLost, RadioHome)
	step(t, m, home(at(10*time.Minute)), Normal, RadioHome)
}

func TestDegradedAppDown(t *testing.T) {
	m := New()
	step(t, m, home(t0), Normal, RadioHome)
	o := home(at(time.Minute))
	o.Health = health.Report{UnitFailed: true}
	step(t, m, o, Normal, RadioHome) // hysteresis
	o.Now = at(2*time.Minute + 59*time.Second)
	step(t, m, o, Normal, RadioHome)
	o.Now = at(3 * time.Minute)
	d := step(t, m, o, Degraded, RadioHome) // never the hotspot
	if d.LED != (led.Pattern{Green: led.On, Red: led.Slow}) || !d.Has(ProblemAppDown) {
		t.Fatalf("decision %+v", d)
	}
	// it recovers at once
	step(t, m, home(at(4*time.Minute)), Normal, RadioHome)
}

func TestDegradedInverterSilent(t *testing.T) {
	m := New()
	step(t, m, home(t0), Normal, RadioHome)
	o := home(at(time.Minute))
	o.Health.Checks = map[string]string{inverterCheck: "fail"}
	step(t, m, o, Normal, RadioHome)
	o.Now = at(10*time.Minute + 59*time.Second)
	step(t, m, o, Normal, RadioHome)
	o.Now = at(11 * time.Minute)
	if d := step(t, m, o, Degraded, RadioHome); !d.Has(ProblemNoInverter) {
		t.Fatalf("problems %v", d.Problems)
	}
}

// TestAwaitingInverter: before the inverter has ever connected, there
// is nothing to wait out: the stick is waiting for the data logger step.
func TestAwaitingInverter(t *testing.T) {
	m := New()
	o := home(t0)
	o.InverterEver = false
	o.Health.Checks = map[string]string{inverterCheck: "fail"}
	d := step(t, m, o, AwaitingInverter, RadioHome)
	if d.LED != (led.Pattern{Green: led.On, Blue: led.Slow}) {
		t.Fatalf("LED %+v", d.LED)
	}
}

// TestBusyHoldsTheRadio: while an operator action (a join) runs, the
// machine asks for nothing, so it doesn't fight the action.
func TestBusyHoldsTheRadio(t *testing.T) {
	m := New()
	o := Observation{Now: t0, Busy: true, Role: netmgr.RoleJoining}
	d := step(t, m, o, Joining, RadioHold)
	if d.LED != (led.Pattern{Green: led.Fast}) {
		t.Fatalf("LED %+v", d.LED)
	}
}

// TestNoAddressIsNotHome: associated but without an address (DHCP
// failing) counts as off the home network.
func TestNoAddressIsNotHome(t *testing.T) {
	m := New()
	o := home(t0)
	o.HomeAddrPresent = false
	step(t, m, o, HomeLost, RadioHome)
}

func TestJoinedClosesWindow(t *testing.T) {
	m := New()
	o := home(t0)
	o.Requested = true
	step(t, m, o, Hotspot, RadioHotspot)
	m.Joined()
	step(t, m, home(at(time.Minute)), Normal, RadioHome)
}
