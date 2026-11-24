// Package loop runs the wizard (design D-031): it observes the radio,
// foxess-local and the operator every tick, asks the state machine,
// and applies the decision to the radio and the LEDs. Operator actions
// (a join, a rescan, a hotspot request) run inside the loop, so only
// one thing ever drives the radio.
package loop

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"time"

	"foxesslocal/internal/wizard/bootcount"
	"foxesslocal/internal/wizard/health"
	"foxesslocal/internal/wizard/led"
	"foxesslocal/internal/wizard/netmgr"
	"foxesslocal/internal/wizard/state"
)

// markerMode is the inverter-seen marker's mode.
const markerMode = 0o600

// DefaultTick is how often the loop looks (the design's 15 s checks).
const DefaultTick = 15 * time.Second

// Radio is the network adapter (netmgr.NM).
type Radio interface {
	Active(ctx context.Context) (netmgr.Status, error)
	HomeKnown(ctx context.Context) (bool, error)
	Scan(ctx context.Context) ([]netmgr.Network, error)
	JoinHome(ctx context.Context, ssid, psk string) error
	HotspotUp(ctx context.Context, ssid, psk string) error
	HotspotDown(ctx context.Context) error
}

// Portal is the captive portal, up while the hotspot is (captive.Portal).
type Portal interface {
	Up(ctx context.Context) error
	Down()
}

// Lights shows a pattern (led.Driver).
type Lights interface{ Set(p led.Pattern) error }

// Prober probes foxess-local (health.Prober).
type Prober interface {
	Probe(ctx context.Context) health.Report
}

// Loop is the wizard's main loop.
type Loop struct {
	Radio        Radio
	Lights       Lights
	Probe        Prober
	Boots        *bootcount.Store
	Uptime       func() time.Duration // the system's uptime (/proc/uptime)
	InverterSeen string               // a marker file: the inverter has connected once
	HotspotSSID  string
	HotspotPSK   string
	Operator     func() bool // an operator has a live session on the hotspot
	Portal       Portal      // optional
	Tick         time.Duration
	Log          *slog.Logger

	m        *state.Machine
	reqs     chan request
	wake     chan struct{}
	mu       sync.Mutex
	snap     Snapshot
	homeKnow bool
	marked   time.Duration // the last boot mark recorded
	pending  bool          // a hotspot request not yet seen by the machine
}

// Snapshot is what the status page shows.
type Snapshot struct {
	Decision state.Decision
	Status   netmgr.Status
	Health   health.Report
	Networks []netmgr.Network // the last scan, made before the hotspot went up
	ScanAt   time.Time
	Join     *JoinResult // the last operator join, if any
}

// JoinResult is an operator join's outcome, kept for the page: the
// phone is off the hotspot while the join runs.
type JoinResult struct {
	SSID string
	At   time.Time
	Err  string // empty on success
	err  error
}

// NewJoinResult builds a result (the demo page's; the loop makes its own).
func NewJoinResult(ssid string, at time.Time, err error) *JoinResult {
	r := &JoinResult{SSID: ssid, At: at, err: err}
	if err != nil {
		r.Err = err.Error()
	}
	return r
}

// Error is the join's error, for errors.Is.
func (j *JoinResult) Error() error { return j.err }

type requestKind int

const (
	reqJoin requestKind = iota
	reqRescan
	reqHotspot
)

type request struct {
	kind      requestKind
	ssid, psk string
}

// Run loops until ctx ends.
func (l *Loop) Run(ctx context.Context) error {
	l.init()
	if trig, err := l.Boots.Begin(); err != nil {
		l.log().Warn("boot record", "err", err)
	} else if trig {
		l.log().Info("three power-cycles: opening the hotspot")
		l.pending = true
	}
	tick := l.Tick
	if tick == 0 {
		tick = DefaultTick
	}
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		case <-l.wake:
		case r := <-l.reqs:
			l.handle(ctx, r)
		}
		l.step(ctx, true)
		l.markBoot()
		t.Reset(tick)
	}
}

func (l *Loop) init() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.m == nil {
		l.m = state.New()
		l.reqs = make(chan request, 1)
		l.wake = make(chan struct{}, 1)
	}
}

// Snapshot is the current state, for the page.
func (l *Loop) Snapshot() Snapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.snap
}

// StartJoin asks the loop to join ssid. It returns at once; the result
// lands in Snapshot().Join. False when another request is pending.
func (l *Loop) StartJoin(ssid, psk string) bool {
	return l.send(request{kind: reqJoin, ssid: ssid, psk: psk})
}

// Rescan asks for a fresh scan; on the hotspot it drops for ~5 s.
func (l *Loop) Rescan() bool { return l.send(request{kind: reqRescan}) }

// RequestHotspot opens a hotspot window (the page's button; three
// power-cycles do the same at boot).
func (l *Loop) RequestHotspot() bool { return l.send(request{kind: reqHotspot}) }

func (l *Loop) send(r request) bool {
	l.init()
	select {
	case l.reqs <- r:
		return true
	default:
		return false
	}
}

func (l *Loop) handle(ctx context.Context, r request) {
	switch r.kind {
	case reqHotspot:
		l.pending = true
	case reqRescan:
		l.rescan(ctx)
	case reqJoin:
		l.join(ctx, r.ssid, r.psk)
	}
}

func (l *Loop) join(ctx context.Context, ssid, psk string) {
	l.publishBusy()
	if st, err := l.Radio.Active(ctx); err == nil && st.Role == netmgr.RoleHotspot {
		if err := l.hotspotDown(ctx); err != nil {
			l.log().Warn("hotspot down for a join", "err", err)
		}
	}
	err := l.Radio.JoinHome(ctx, ssid, psk)
	res := &JoinResult{SSID: ssid, At: time.Now(), err: err}
	if err != nil {
		res.Err = err.Error()
		l.log().Info("join failed", "ssid", ssid, "err", err)
		// a fresh window, so the operator can read why
		l.pending = true
	} else {
		l.log().Info("joined", "ssid", ssid)
		l.m.Joined()
		l.pending = false
	}
	l.mu.Lock()
	l.snap.Join = res
	l.mu.Unlock()
}

func (l *Loop) rescan(ctx context.Context) {
	st, err := l.Radio.Active(ctx)
	onHotspot := err == nil && st.Role == netmgr.RoleHotspot
	if onHotspot {
		if err := l.hotspotDown(ctx); err != nil {
			l.log().Warn("hotspot down for a scan", "err", err)
		}
	}
	l.scan(ctx)
	if onHotspot {
		if err := l.hotspotUp(ctx); err != nil {
			l.log().Warn("hotspot up after a scan", "err", err)
		}
	}
}

func (l *Loop) scan(ctx context.Context) {
	nets, err := l.Radio.Scan(ctx)
	if err != nil {
		l.log().Warn("scan", "err", err)
		return
	}
	l.mu.Lock()
	l.snap.Networks, l.snap.ScanAt = nets, time.Now()
	l.mu.Unlock()
}

// publishBusy shows the joining state while the join runs.
func (l *Loop) publishBusy() {
	d := l.m.Step(state.Observation{Now: time.Now(), Busy: true})
	l.show(d)
	l.mu.Lock()
	l.snap.Decision = d
	l.mu.Unlock()
}

// step observes, decides and applies. After a radio change it looks
// once more, so a quick reconnect shows at once.
func (l *Loop) step(ctx context.Context, again bool) {
	o := l.observe(ctx)
	d := l.m.Step(o)
	l.pending = false
	l.show(d)
	l.mu.Lock()
	l.snap.Decision, l.snap.Status, l.snap.Health = d, netmgr.Status{Role: o.Role, Addr: l.snap.Status.Addr}, o.Health
	l.mu.Unlock()
	if l.apply(ctx, d, o.Role) && again {
		l.step(ctx, false)
	}
}

func (l *Loop) observe(ctx context.Context) state.Observation {
	o := state.Observation{Now: time.Now(), Requested: l.pending}
	if known, err := l.Radio.HomeKnown(ctx); err == nil {
		l.homeKnow = known
	} else {
		l.log().Warn("profiles", "err", err)
	}
	o.HomeKnown = l.homeKnow
	if st, err := l.Radio.Active(ctx); err == nil {
		o.Role, o.HomeAddrPresent = st.Role, st.Addr.IsValid()
		l.mu.Lock()
		l.snap.Status.Addr = st.Addr
		l.mu.Unlock()
	} else {
		l.log().Warn("radio status", "err", err)
	}
	o.Health = l.Probe.Probe(ctx)
	o.InverterEver = l.inverterEver(o.Health)
	if l.Operator != nil {
		o.Operator = l.Operator()
	}
	return o
}

// inverterEver reads, and on a first sighting writes, the marker.
func (l *Loop) inverterEver(h health.Report) bool {
	if _, err := os.Stat(l.InverterSeen); err == nil {
		return true
	}
	if !h.InverterSeen() {
		return false
	}
	if err := os.WriteFile(l.InverterSeen, nil, markerMode); err != nil {
		l.log().Warn("inverter-seen marker", "err", err)
	}
	return true
}

// apply makes the radio match the decision; it reports a change.
func (l *Loop) apply(ctx context.Context, d state.Decision, role netmgr.Role) bool {
	switch {
	case d.Radio == state.RadioHotspot && role != netmgr.RoleHotspot:
		l.scan(ctx) // no scanning once the hotspot is up (S2)
		if err := l.hotspotUp(ctx); err != nil {
			l.log().Error("hotspot up", "err", err)
			return false
		}
		return true
	case d.Radio == state.RadioHotspot && l.Portal != nil:
		// already a hotspot: left up by a previous run (a restart), or
		// its portal failed to start. Up is a no-op while it runs.
		if err := l.Portal.Up(ctx); err != nil {
			l.log().Error("captive portal", "err", err)
		}
	case d.Radio == state.RadioHome && role == netmgr.RoleHotspot:
		if err := l.hotspotDown(ctx); err != nil {
			l.log().Error("hotspot down", "err", err)
			return false
		}
		return true
	}
	return false
}

// hotspotUp raises the hotspot, then its captive portal (dnsmasq needs
// the address to bind).
func (l *Loop) hotspotUp(ctx context.Context) error {
	if err := l.Radio.HotspotUp(ctx, l.HotspotSSID, l.HotspotPSK); err != nil {
		return err
	}
	if l.Portal != nil {
		if err := l.Portal.Up(ctx); err != nil {
			l.log().Error("captive portal", "err", err)
		}
	}
	return nil
}

// hotspotDown stops the portal, then drops the hotspot.
func (l *Loop) hotspotDown(ctx context.Context) error {
	if l.Portal != nil {
		l.Portal.Down()
	}
	return l.Radio.HotspotDown(ctx)
}

func (l *Loop) show(d state.Decision) {
	if l.Lights == nil {
		return
	}
	if err := l.Lights.Set(d.LED); err != nil {
		l.log().Warn("led", "err", err)
	}
}

// markBoot records the boot's uptime when it passes a mark.
func (l *Loop) markBoot() {
	if l.Uptime == nil {
		return
	}
	up := l.Uptime()
	var mark time.Duration
	for _, m := range bootcount.Marks {
		if up >= m {
			mark = m
		}
	}
	if mark <= l.marked {
		return
	}
	if err := l.Boots.Mark(mark); err != nil && !errors.Is(err, os.ErrNotExist) {
		l.log().Warn("boot mark", "err", err)
	}
	l.marked = mark
}

func (l *Loop) log() *slog.Logger {
	if l.Log == nil {
		return slog.Default()
	}
	return l.Log
}
