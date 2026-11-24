package loop

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"foxesslocal/internal/wizard/bootcount"
	"foxesslocal/internal/wizard/health"
	"foxesslocal/internal/wizard/led"
	"foxesslocal/internal/wizard/netmgr"
	"foxesslocal/internal/wizard/state"
)

// fakeRadio is one radio with NetworkManager's autoconnect: when the
// hotspot drops and a home profile exists and is in range, it is home.
type fakeRadio struct {
	mu        sync.Mutex
	role      netmgr.Role
	homeKnown bool
	inRange   bool // the home network answers
	joinErr   error
	log       []string
}

func (r *fakeRadio) note(s string) { r.log = append(r.log, s) }

func (r *fakeRadio) Active(context.Context) (netmgr.Status, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := netmgr.Status{Role: r.role}
	if r.role == netmgr.RoleHome {
		st.Addr = netmgrAddr
	}
	return st, nil
}

func (r *fakeRadio) HomeKnown(context.Context) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.homeKnown, nil
}

func (r *fakeRadio) Scan(context.Context) ([]netmgr.Network, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.role == netmgr.RoleHotspot {
		return nil, errors.New("fake: no scan in AP mode")
	}
	r.note("scan")
	return []netmgr.Network{{SSID: "home", Signal: 70, Secured: true}}, nil
}

func (r *fakeRadio) HotspotUp(_ context.Context, ssid, psk string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ssid == "" || psk == "" {
		return errors.New("fake: no hotspot credentials")
	}
	r.note("hotspot up")
	r.role = netmgr.RoleHotspot
	return nil
}

func (r *fakeRadio) HotspotDown(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.note("hotspot down")
	r.role = netmgr.RoleNone
	if r.homeKnown && r.inRange {
		r.role = netmgr.RoleHome
	}
	return nil
}

func (r *fakeRadio) JoinHome(_ context.Context, ssid, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.note("join " + ssid)
	time.Sleep(7 * time.Second) // the join takes a while (S2: 6.5 s to refuse)
	if r.joinErr != nil {
		r.role = netmgr.RoleNone
		return r.joinErr
	}
	r.role, r.homeKnown, r.inRange = netmgr.RoleHome, true, true
	return nil
}

func (r *fakeRadio) seen(s string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Contains(r.log, s)
}

type fakeLights struct {
	mu   sync.Mutex
	last led.Pattern
}

func (l *fakeLights) Set(p led.Pattern) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.last = p
	return nil
}

type fakeProbe struct {
	mu sync.Mutex
	r  health.Report
}

func (p *fakeProbe) Probe(context.Context) health.Report {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.r
}

var netmgrAddr = netip.MustParsePrefix("192.0.2.40/24")

var healthy = health.Report{Live: true, Ready: "ready", Checks: map[string]string{"inverter_connected": "ok"}}

type rig struct {
	l     *Loop
	radio *fakeRadio
	led   *fakeLights
	probe *fakeProbe
	dir   string
	stop  func()
}

func newRig(t *testing.T, radio *fakeRadio) *rig {
	t.Helper()
	return newRigAt(t, radio, t.TempDir())
}

func newRigAt(t *testing.T, radio *fakeRadio, dir string) *rig {
	t.Helper()
	return newRigWith(t, radio, dir, nil)
}

func newRigWith(t *testing.T, radio *fakeRadio, dir string, tweak func(*Loop)) *rig {
	t.Helper()
	r := &rig{radio: radio, led: &fakeLights{}, probe: &fakeProbe{r: healthy}, dir: dir}
	start := time.Now()
	r.l = &Loop{
		Radio:        radio,
		Lights:       r.led,
		Probe:        r.probe,
		Boots:        &bootcount.Store{Path: filepath.Join(dir, "boots")},
		Uptime:       func() time.Duration { return time.Since(start) },
		InverterSeen: filepath.Join(dir, "inverter-seen"),
		HotspotSSID:  "foxess-1a2b",
		HotspotPSK:   "stick-password",
		Operator:     func() bool { return false },
	}
	if tweak != nil {
		tweak(r.l)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = r.l.Run(ctx); close(done) }()
	r.stop = func() { cancel(); <-done }
	return r
}

func TestUnconfiguredScansThenOpensHotspot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		radio := &fakeRadio{}
		r := newRig(t, radio)
		defer r.stop()
		synctest.Wait()
		s := r.l.Snapshot()
		if s.Decision.State != state.Unconfigured || radio.role != netmgr.RoleHotspot {
			t.Fatalf("state %v role %v", s.Decision.State, radio.role)
		}
		if len(s.Networks) != 1 || !radio.seen("scan") {
			t.Fatalf("no scan before the hotspot: %v %v", s.Networks, radio.log)
		}
		if r.led.last != (led.Pattern{Red: led.Slow}) {
			t.Fatalf("LED %+v", r.led.last)
		}
	})
}

// TestJoinFromHotspot: the operator's join drops the hotspot, joins,
// and the stick is home; the result waits in the snapshot.
func TestJoinFromHotspot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		radio := &fakeRadio{}
		r := newRig(t, radio)
		defer r.stop()
		synctest.Wait()
		r.l.StartJoin("home", "home-password")
		time.Sleep(time.Second)
		synctest.Wait()
		if s := r.l.Snapshot(); s.Decision.State != state.Joining {
			t.Fatalf("during the join: %v", s.Decision.State)
		}
		time.Sleep(10 * time.Second)
		synctest.Wait()
		s := r.l.Snapshot()
		if s.Decision.State != state.Normal || s.Join == nil || s.Join.Err != "" || s.Join.SSID != "home" {
			t.Fatalf("after the join: %v %+v", s.Decision.State, s.Join)
		}
		if _, err := os.Stat(r.l.InverterSeen); err != nil {
			t.Fatalf("inverter-seen not persisted: %v", err)
		}
	})
}

// TestFailedJoinReopensHotspot: a wrong password brings the hotspot
// back, with the reason, so the operator sees it on rejoining.
func TestFailedJoinReopensHotspot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		radio := &fakeRadio{joinErr: netmgr.ErrWrongPassword}
		r := newRig(t, radio)
		defer r.stop()
		synctest.Wait()
		r.l.StartJoin("home", "not-the-password")
		time.Sleep(10 * time.Second)
		synctest.Wait()
		s := r.l.Snapshot()
		if radio.role != netmgr.RoleHotspot || s.Join == nil || !errors.Is(s.Join.err, netmgr.ErrWrongPassword) {
			t.Fatalf("role %v join %+v", radio.role, s.Join)
		}
	})
}

// TestFailedJoinKeepsWindowWhenConfigured: re-configuring a stick whose
// old network still exists: a failed join opens a fresh window rather
// than going home before the operator can read why.
func TestFailedJoinKeepsWindowWhenConfigured(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		radio := &fakeRadio{role: netmgr.RoleHome, homeKnown: true, inRange: true, joinErr: netmgr.ErrNotFound}
		r := newRig(t, radio)
		defer r.stop()
		synctest.Wait()
		r.l.RequestHotspot()
		time.Sleep(time.Second)
		synctest.Wait()
		if radio.role != netmgr.RoleHotspot {
			t.Fatalf("requested hotspot not opened: %v", radio.role)
		}
		time.Sleep(4 * time.Minute)
		r.l.StartJoin("new-network", "a-password")
		time.Sleep(10 * time.Second)
		synctest.Wait()
		if radio.role != netmgr.RoleHotspot {
			t.Fatalf("after the failed join: role %v", radio.role)
		}
		time.Sleep(2 * time.Minute) // the old window would have closed by now
		synctest.Wait()
		if radio.role != netmgr.RoleHotspot {
			t.Fatalf("the fresh window closed early: role %v", radio.role)
		}
	})
}

// TestHomeLostOpensWindowAndReturns: the loop follows the machine's
// timings against the fake radio's autoconnect.
func TestHomeLostOpensWindowAndReturns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		radio := &fakeRadio{role: netmgr.RoleNone, homeKnown: true, inRange: false}
		r := newRig(t, radio)
		defer r.stop()
		time.Sleep(10*time.Minute + 20*time.Second)
		synctest.Wait()
		if radio.role != netmgr.RoleHotspot {
			t.Fatalf("after 10 min off home: role %v", radio.role)
		}
		radio.mu.Lock()
		radio.inRange = true
		radio.mu.Unlock()
		time.Sleep(5*time.Minute + 20*time.Second)
		synctest.Wait()
		if radio.role != netmgr.RoleHome || r.l.Snapshot().Decision.State != state.Normal {
			t.Fatalf("after the window: role %v state %v", radio.role, r.l.Snapshot().Decision.State)
		}
	})
}

// TestBootMarks: the loop records the boot's uptime at the marks.
func TestBootMarks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		radio := &fakeRadio{role: netmgr.RoleHome, homeKnown: true, inRange: true}
		r := newRig(t, radio)
		defer r.stop()
		time.Sleep(65 * time.Second)
		synctest.Wait()
		b, err := os.ReadFile(filepath.Join(r.dir, "boots"))
		if err != nil || string(b) != "60\n" {
			t.Fatalf("boots %q %v", b, err)
		}
	})
}

// TestThreeReplugsOpenHotspot: Begin's trigger reaches the machine.
func TestThreeReplugsOpenHotspot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "boots"), []byte("20\n15\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		radio := &fakeRadio{role: netmgr.RoleHome, homeKnown: true, inRange: true}
		r := newRigAt(t, radio, dir)
		defer r.stop()
		synctest.Wait()
		if radio.role != netmgr.RoleHotspot {
			t.Fatalf("three replugs: role %v", radio.role)
		}
	})
}

// TestJoinClosesWindow: re-configuring from a requested hotspot: once
// the new network is joined, the stick stays on it; the window that
// was open for the operator doesn't pull it back to the hotspot.
func TestJoinClosesWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		radio := &fakeRadio{role: netmgr.RoleHome, homeKnown: true, inRange: true}
		r := newRig(t, radio)
		defer r.stop()
		synctest.Wait()
		r.l.RequestHotspot()
		time.Sleep(time.Minute)
		synctest.Wait()
		r.l.StartJoin("new-network", "a-password")
		time.Sleep(time.Minute)
		synctest.Wait()
		if radio.role != netmgr.RoleHome || r.l.Snapshot().Decision.State != state.Normal {
			t.Fatalf("after a successful join: role %v state %v", radio.role, r.l.Snapshot().Decision.State)
		}
	})
}

type fakePortal struct {
	mu sync.Mutex
	up bool
}

func (p *fakePortal) Up(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.up = true
	return nil
}
func (p *fakePortal) Down() { p.mu.Lock(); defer p.mu.Unlock(); p.up = false }

// TestPortalFollowsHotspot: the captive portal is up exactly while the
// hotspot is.
func TestPortalFollowsHotspot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		radio := &fakeRadio{}
		dir := t.TempDir()
		portal := &fakePortal{}
		r := newRigWith(t, radio, dir, func(l *Loop) { l.Portal = portal })
		defer r.stop()
		synctest.Wait()
		if !portal.up {
			t.Fatal("hotspot up, portal down")
		}
		r.l.StartJoin("home", "home-password")
		time.Sleep(20 * time.Second)
		synctest.Wait()
		if portal.up || radio.role != netmgr.RoleHome {
			t.Fatalf("after the join: portal up %v, role %v", portal.up, radio.role)
		}
	})
}
