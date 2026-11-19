package framesvc

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"time"

	"foxesslocal/internal/compat"
	"foxesslocal/internal/driver"
	"foxesslocal/internal/frame"
	"foxesslocal/internal/gateway"
	"foxesslocal/internal/identity"
)

// Live-value polling (2026-11-16): the inverter sends no 5 s telemetry
// under the current cloud handshake (profile §4 note), so the service
// reads the KH live-value registers itself through the gateway (C-003:
// fixed reads, OriginPoll — they yield to operator commands) and records
// each poll as an ordinary telemetry row + EventTelemetry. /state,
// online (DEF-001), history and SSE consume it unchanged.
const (
	pollEvery  = 5 * time.Second  // the cloud-era 5 s telemetry cadence
	pollSettle = 15 * time.Second // after the session's first keepalive: never mid-handshake
	pollWait   = 3 * time.Second  // per read (observed responses: 27–36 ms)

	fnHoldingRead = 3 // the KH live-value blocks are holding registers

	// settingsEvery: the settings blocks change only on a write, and an
	// acked operator write is already a newer /state source; the re-read
	// picks up the device's own value (it rounds some limits — profile §5)
	// and any change made at the inverter's panel.
	settingsEvery = 5 * time.Minute

	// energyEvery: the lifetime energy totals (the foxess-control local
	// backend paces its feed-in target from them and checks every 60 s).
	energyEvery = 30 * time.Second
)

// identityBlocks are read when a session is classified (its start, then
// every settingsEvery): the foxess_modbus model / version reads (D-020).
// Both are optional — another family answers with an exception.
var identityBlocks = []struct{ addr, count int }{
	{identity.ModelAddr, identity.ModelCount},     // model string
	{identity.VersionAddr, identity.VersionCount}, // master/slave/manager fw
}

// settingsBlocks are the KH_PRE133 holding-register blocks behind
// /state's settings — the same reads the cloud made before its config
// pushes (profile §5): work mode, the 41007 readback (limits at
// 41021/41023, meter compensation at 41029, current limits), the
// scheduler master and the 48010 time-group table. Never read on another
// layout (they mean something else there).
var settingsBlocks = []struct{ addr, count int }{
	{0x9C40, 6}, // the inverter's clock, year…second: compared with the host's (audit R8)
	{41000, 1},
	{41007, 40},
	{41200, 3}, // LoadLimitPower / GenerationLimitPower / TBD (profile §5)
	{48000, 10},
	{48010, 80},
	// schedule groups 1–3's parameter blocks: import / export limit and
	// PV use per window (profile §5, 2026-11-18). The cloud reads and
	// writes these three; 57150+ (groups 4–8) were never seen.
	{57000, 50},
	{57050, 50},
	{57100, 50},
}

// tentativeFailures is how many consecutive polls a tentative map may
// fail a check before the layout is dropped and re-classified.
const tentativeFailures = 3

// poller is the polling loop over its seams (tests script them).
type poller struct {
	target     func(now time.Time) (connID string, ok bool)
	read       func(ctx context.Context, fn, addr, count int) ([]uint16, error)
	record     func(connID string, ts time.Time, t frame.Tele)
	recordRegs func(ts time.Time, addr int, vals []uint16)
	observe    func(Event)
	clock      func() time.Time

	settingsConn string    // the session the last complete settings round read
	settingsAt   time.Time // when that round completed

	energyConn string    // the session the last energy read was on
	energyAt   time.Time // when it was read

	// classification (D-020/D-021): the session's register layout
	classConn  string
	classAt    time.Time
	layout     string           // driver.LayoutKHPre133, a foxess_modbus map key, or "" (none)
	cand       compat.Candidate // the tentative layout's map
	failures   int              // consecutive failed tentative polls
	classTries int              // classifications since the session started
}

// run polls every pollEvery until ctx is done.
func (p *poller) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(pollEvery):
			p.tick(ctx)
		}
	}
}

// tick is one poll: classify when due, then the layout's live reads,
// decoded, recorded, published. Any failure skips the tick (the next one
// retries).
func (p *poller) tick(ctx context.Context) {
	connID, ok := p.target(p.clock())
	if !ok {
		return
	}
	if p.classifyDue(connID) {
		p.classify(ctx, connID)
	}
	switch p.layout {
	case "":
		return
	case driver.LayoutKHPre133:
		p.tickKH(ctx, connID)
	default:
		p.tickMap(ctx, connID)
	}
}

// classifyDue: a new session, settingsEvery since the last
// classification, or — while no layout is established — every tick for
// the first classifyBurst attempts (a transient read failure must not
// cost five minutes of data), then every settingsEvery.
func (p *poller) classifyDue(connID string) bool {
	switch {
	case connID != p.classConn:
		p.classTries = 0
		return true
	case p.clock().Sub(p.classAt) >= settingsEvery:
		return true
	case p.layout == "" && p.classTries < classifyBurst:
		return true
	}
	return false
}

// classifyBurst is how many consecutive ticks classification is retried
// while no layout is established (one minute at pollEvery).
const classifyBurst = 12

// readIdentity reads the identity blocks: the model (or ""), whether
// the version block answered, and whether its read was lost.
func (p *poller) readIdentity(ctx context.Context) (model string, versions, lost bool) {
	for _, b := range identityBlocks {
		vals, err := p.read(ctx, fnHoldingRead, b.addr, b.count)
		if err != nil {
			lost = lost || b.addr == identity.VersionAddr
			continue
		}
		p.recordRegs(p.clock(), b.addr, vals)
		switch b.addr {
		case identity.ModelAddr:
			model = identity.ModelFromRegisters(vals)
		case identity.VersionAddr:
			versions = true
		}
	}
	return model, versions, lost
}

// classify reads the identity blocks and establishes the layout: the
// verified KH_PRE133 layout when the 30016 version block answers on a
// KH (or an unreadable model — the legacy assumption), else the first
// foxess_modbus candidate map whose live readings pass the checks.
func (p *poller) classify(ctx context.Context, connID string) {
	p.classConn, p.classAt = connID, p.clock()
	p.classTries++
	model, versions, lost := p.readIdentity(ctx)
	if versions && (model == "" || khModel.MatchString(model)) {
		p.layout = driver.LayoutKHPre133
		return
	}
	if lost && p.layout == driver.LayoutKHPre133 {
		// a lost read on a lossy link (an OpenStick on Wi-Fi, 2026-11-19)
		// is no evidence of another layout: the session keeps its own (a
		// different inverter comes with a new session)
		return
	}
	if p.layout != "" && p.layout != driver.LayoutKHPre133 && p.cand.Key != "" && p.failures == 0 {
		return // an established tentative layout stays while it checks out
	}
	p.layout, p.cand, p.failures = "", compat.Candidate{}, 0
	for _, c := range compat.Candidates(model) {
		get, ok := p.readPlan(ctx, c)
		if !ok {
			continue
		}
		if compat.Classifiable(compat.LiveChecks(compat.FromMap(c.Map, get), nil)) {
			p.layout, p.cand = c.Key, c
			return
		}
	}
}

// readPlan reads a candidate's live-value blocks, recording each.
func (p *poller) readPlan(ctx context.Context, c compat.Candidate) (func(int) (uint16, bool), bool) {
	regs := map[int]uint16{}
	for _, b := range c.Plan() {
		vals, err := p.read(ctx, c.Fn, b.Addr, b.Count)
		if err != nil {
			return nil, false
		}
		p.recordRegs(p.clock(), b.Addr, vals)
		for i, v := range vals {
			regs[b.Addr+i] = v
		}
	}
	return func(a int) (uint16, bool) { v, ok := regs[a]; return v, ok }, true
}

// tickKH is the verified layout's poll: both live blocks and, when due,
// the settings round.
func (p *poller) tickKH(ctx context.Context, connID string) {
	a, err := p.read(ctx, fnHoldingRead, frame.KHRealtimeAAddr, frame.KHRealtimeACount)
	if err != nil {
		return
	}
	b, err := p.read(ctx, fnHoldingRead, frame.KHRealtimeBAddr, frame.KHRealtimeBCount)
	if err != nil {
		return
	}
	r, err := frame.ParseKHRealtime(a, b)
	if err != nil {
		return
	}
	now := p.clock()
	// the raw blocks too: the compatibility checks (D-020) run on the
	// measured values (PV V×A = W, grid V/Hz), not the derived row
	p.recordRegs(now, frame.KHRealtimeAAddr, a)
	p.recordRegs(now, frame.KHRealtimeBAddr, b)
	tele := r.Tele()
	p.record(connID, now, tele)
	p.observe(Event{Kind: EventTelemetry, ConnID: connID, TS: now, Tele: &tele})
	p.energy(ctx, connID)
	p.settings(ctx, connID)
}

// energy reads the lifetime energy totals when due (a new session, or
// energyEvery since the last read). A failed read stays due.
func (p *poller) energy(ctx context.Context, connID string) {
	if connID == p.energyConn && p.clock().Sub(p.energyAt) < energyEvery {
		return
	}
	vals, err := p.read(ctx, fnHoldingRead, frame.KHEnergyAddr, frame.KHEnergyCount)
	if err != nil {
		return
	}
	e, err := frame.ParseKHEnergy(vals)
	if err != nil {
		return
	}
	now := p.clock()
	p.recordRegs(now, frame.KHEnergyAddr, vals)
	p.energyConn, p.energyAt = connID, now
	p.observe(Event{Kind: EventEnergy, ConnID: connID, Energy: &e})
}

// tickMap is a tentative layout's poll through its foxess_modbus map. A
// poll whose values fail a check is not recorded; repeated failures drop
// the layout so the next classification starts over.
func (p *poller) tickMap(ctx context.Context, connID string) {
	get, ok := p.readPlan(ctx, p.cand)
	if !ok {
		return
	}
	lv := compat.FromMap(p.cand.Map, get)
	if compat.AnyFailed(compat.LiveChecks(lv, nil)) {
		p.failures++
		if p.failures >= tentativeFailures {
			p.layout, p.cand, p.failures = "", compat.Candidate{}, 0
		}
		return
	}
	p.failures = 0
	tele := teleOf(lv)
	now := p.clock()
	p.record(connID, now, tele)
	p.observe(Event{Kind: EventTelemetry, ConnID: connID, TS: now, Tele: &tele})
	p.tentativeSettings(ctx, connID)
}

// tentativeSettings reads the settings a tentative map covers that the
// app can act on (the work-mode select) once per settings round —
// single registers, honouring the map's invalid ranges.
func (p *poller) tentativeSettings(ctx context.Context, connID string) {
	if connID == p.settingsConn && p.clock().Sub(p.settingsAt) < settingsEvery {
		return
	}
	for _, a := range compat.SettingAddrs(p.cand) {
		vals, err := p.read(ctx, fnHoldingRead, a, 1)
		if err != nil {
			return
		}
		p.recordRegs(p.clock(), a, vals)
	}
	p.settingsConn, p.settingsAt = connID, p.clock()
}

// teleOf maps tentative live values onto the telemetry row.
func teleOf(v compat.LiveValues) frame.Tele {
	w := func(p *float64) int32 {
		if p == nil {
			return 0
		}
		return int32(math.Round(*p))
	}
	var sol float64
	for _, s := range v.PV {
		if s.Present {
			sol += s.W
		}
	}
	t := frame.Tele{
		Sol: int32(math.Round(sol)), Grid: w(v.GridImportW), Haus: w(v.LoadW),
		Acport: w(v.InverterW), Batt: w(v.BatteryW),
	}
	if v.SocPct != nil && *v.SocPct >= 0 && *v.SocPct <= 100 {
		t.Soc = byte(math.Round(*v.SocPct))
	}
	t.IDGrid = int(t.Grid - (t.Haus + t.Batt - t.Sol))
	t.IDAcport = int(t.Acport - (t.Sol - t.Batt))
	return t
}

// settings reads the KH settings blocks when a round is due (a new
// session, or settingsEvery since the last complete round). Each block
// read is recorded as it lands; a failed block leaves the round due, so
// the next tick retries it.
func (p *poller) settings(ctx context.Context, connID string) {
	if connID == p.settingsConn && p.clock().Sub(p.settingsAt) < settingsEvery {
		return
	}
	for _, b := range settingsBlocks {
		vals, err := p.read(ctx, fnHoldingRead, b.addr, b.count)
		if err != nil {
			return
		}
		p.recordRegs(p.clock(), b.addr, vals)
	}
	p.settingsConn, p.settingsAt = connID, p.clock()
}

// khModel is the foxess_modbus KH model pattern.
var khModel = regexp.MustCompile(`^KH([\d.]+)`)

// RunPoller polls the live-value registers on the active, established
// session until ctx is done.
func (s *Service) RunPoller(ctx context.Context) {
	(&poller{
		target:     s.pollTarget,
		read:       s.pollRead,
		record:     s.st.RecordTelemetry,
		recordRegs: s.st.RecordRegisters,
		observe:    s.observe,
		clock:      s.clock,
	}).run(ctx)
}

// pollTarget is the active flow (local or mirrored) once it has been
// established for pollSettle (first keepalive seen) — the handshake is
// never disturbed.
func (s *Service) pollTarget(now time.Time) (string, bool) {
	s.mu.Lock()
	id := s.active
	fl := s.flows[id]
	s.mu.Unlock()
	if id == "" || fl == nil {
		return "", false
	}
	// a mirrored session (telemetry_only, full, a maintenance window) is
	// polled too: every answer goes to whoever asked (route.go — D-024),
	// so a cloud answer never acks our read and ours never reach the cloud
	fl.mu.Lock()
	first := fl.firstKeepalive
	fl.mu.Unlock()
	if first.IsZero() || now.Sub(first) < pollSettle {
		return "", false
	}
	return id, true
}

// pollRead is one holding-register block read through the gateway.
func (s *Service) pollRead(ctx context.Context, fn, addr, count int) ([]uint16, error) {
	id, err := s.gw.Submit(ctx, gateway.Command{
		Kind:   gateway.KindRead,
		Params: map[gateway.Param]int{gateway.ParamFn: fn, gateway.ParamAddr: addr, gateway.ParamCount: count},
		Origin: gateway.OriginPoll,
	})
	if err != nil {
		return nil, err
	}
	wctx, cancel := context.WithTimeout(ctx, pollWait)
	defer cancel()
	res, err := s.gw.Wait(wctx, id)
	if err != nil {
		return nil, err
	}
	if res.Status != gateway.StatusAcked {
		return nil, fmt.Errorf("poll read @%d: %s %s", addr, res.Status, res.Detail)
	}
	if len(res.Registers) != count {
		return nil, fmt.Errorf("poll read @%d: %d registers, want %d", addr, len(res.Registers), count)
	}
	out := make([]uint16, count)
	for i, rv := range res.Registers {
		out[i] = rv.Value
	}
	return out, nil
}
