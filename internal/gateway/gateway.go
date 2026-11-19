// Package gateway is the C-003 command path: every operator command to the
// inverter passes this one validated, range-clamping gateway. It validates
// commands against the inverter's real register map (profile §5), builds the
// wire frames (Modbus PDUs in 7f7f envelopes), correlates the inverter's
// ACKs, and queues commands with a TTL when no inverter flow is active.
// Nothing invalid ever reaches the wire.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"foxesslocal/internal/frame"
)

// defaultQueueTTL is how long a command may wait in the no-flow queue.
const defaultQueueTTL = 5 * time.Minute

// defaultEnvFn is the envelope func the real cloud used in the 11-04
// capture (profile §5: per-session constant; 0xc7 in the 11-05 session).
const defaultEnvFn = 0x10

// CommandKind is the operator write surface (the API's four inverter write
// endpoints). metersoc/minsoc are deliberately absent: they have no
// endpoint and no identified global wire register (min/max SoC live as
// packed per-slot fields inside schedule).
type CommandKind string

// The four command kinds (the API's inverter write surface).
const (
	// KindWorkMode sets the immediate work mode (fn06 @41000).
	KindWorkMode CommandKind = "workmode"
	// KindLimits sets the import/export power limits (fn10 ×2 @41021/41023).
	KindLimits CommandKind = "limits"
	// KindMeterCompensation sets the signed meter compensation (fn06 @41029).
	KindMeterCompensation CommandKind = "metercompensation"
	// KindSchedule replaces the 8-group table (read-modify-write push).
	KindSchedule CommandKind = "schedule"
	// KindRead reads a register block (fn03 holding / fn04 input) and
	// writes nothing — the live-value poller and the installer probe.
	KindRead CommandKind = "read"
)

// OriginPoll marks the live-value poller's reads: they yield to
// operator commands (never block one, never queue) — see Submit.
const OriginPoll = "poller"

// inFlightTTL is the default bound on how long a command may wait for
// the inverter's response (observed responses: 27–36 ms). Past it the
// command fails and the slot frees — an unanswered command must never
// wedge the gateway (checked lazily on Submit/Pending/Wait, injected
// clock). WithInFlightTTL overrides it.
const inFlightTTL = 10 * time.Second

// maxReadCount is the Modbus PDU limit for one read (125 registers);
// regSpace is the 16-bit register address space a block must fit in.
const (
	maxReadCount = 125
	regSpace     = 0x10000
)

// Command is one operator write. Params keys (validated, unknown keys are
// an error):
//   - workmode:          "mode" 0 self-use / 1 feed-in priority
//   - limits:            "import" and/or "export" watts 0..30000
//   - metercompensation: "value" signed watts -500..500
//   - read:              "fn" 3 (holding) / 4 (input), "addr" 0..65535,
//     "count" 1..125 (the block must not run past 65535)
//   - schedule:          "enabled" 0/1 plus, per slot k (1..8),
//     "<k>_en", "<k>_start", "<k>_end" (packed HHMM: (h<<8)|m),
//     "<k>_workmode" (wire enum 1..7), "<k>_maxsoc", "<k>_minsoc",
//     "<k>_fdsoc" (0..100), "<k>_fdpwr" (0..30000). A missing slot field
//     defaults to 0 (missing "<k>_en" disables the slot with all other
//     fields zero).
type Command struct {
	Kind   CommandKind
	Params map[Param]int // the kind's parameters (params.go)
	Origin string
}

// RegisterValue is one Modbus register read (address, value).
type RegisterValue struct {
	Addr  uint16
	Value uint16
}

// Injector pushes one raw s2c frame onto the active inverter flow. Task 6
// supplies it via the frame hook's HookConn.
type Injector interface {
	InjectS2C(connID string, raw []byte) error
}

// Status is a command's lifecycle state.
type Status string

// Command lifecycle states.
const (
	StatusQueued   Status = "queued"
	StatusInFlight Status = "in-flight"
	StatusAcked    Status = "acked"
	StatusExpired  Status = "expired"
	StatusFailed   Status = "failed"
)

// PendingCommand is one operator-visible command (queued, in-flight,
// expired, or failed — never silently dropped).
type PendingCommand struct {
	ID         string
	Kind       CommandKind
	Origin     string
	Status     Status
	EnqueuedAt time.Time
	Detail     string // set when failed
}

// Sentinel errors (the API maps these to problem+json codes).
var (
	ErrUnknownKind     = errors.New("gateway: unknown command kind")
	ErrUnknownParam    = errors.New("gateway: unknown command param")
	ErrMissingParam    = errors.New("gateway: missing required param")
	ErrOutOfRange      = errors.New("gateway: value out of range")
	ErrNoFields        = errors.New("gateway: limits command requires at least one field")
	ErrScheduleOverlap = errors.New("gateway: schedule_overlap: enabled windows overlap")
	ErrScheduleWindow  = errors.New("gateway: schedule_window: end <= start")
	ErrMissingCurrent  = errors.New("gateway: missing current register data")
	ErrInFlight        = errors.New("gateway: a command is already in flight")
	ErrUnknownCommand  = errors.New("gateway: unknown command id")
	ErrEchoMismatch    = errors.New("gateway: echo does not match the in-flight command")
	ErrModbusException = errors.New("gateway: modbus exception")
	ErrNoFlow          = errors.New("gateway: no active inverter flow")
)

// Gateway is the sole operator write path (C-003).
type Gateway struct {
	mu     sync.Mutex
	inj    Injector
	clock  func() time.Time
	ttl    time.Duration
	envFn  byte
	flow   string // active inverter flow connID ("" = none)
	queued []*rtCommand
	stale  []*rtCommand // terminal but operator-visible (expired / failed)
	rt     *rtCommand   // in-flight command (nil = slot free)
	acked  []*rtCommand // acked, Wait-consumable (Pending drops them)
	seq    int
	obs    ResultObserver // terminal-state observer (nil = none)
	authz  Authorizer     // C-008 write gate (nil = none)
	flTTL  time.Duration  // in-flight response bound (inFlightTTL)

	// wear protections (wear.go)
	skipUnchanged bool
	budget        int
	budgetFor     map[CommandKind]int // per-setting overrides of budget
	budgetWindow  time.Duration
	// budgetMu guards written on its own: Budget is read by the observer,
	// which the gateway calls under mu (order: mu, then budgetMu)
	budgetMu sync.Mutex
	written  map[CommandKind][]time.Time // real write phases per kind
}

// New creates a gateway. The clock is injected (C-004); the queue TTL
// defaults to 5 min (SetTTL) and the envelope func to 0x10 (SetEnvFn).
func New(inj Injector, clock func() time.Time, opts ...Option) *Gateway {
	g := &Gateway{inj: inj, clock: clock, ttl: defaultQueueTTL, envFn: defaultEnvFn, flTTL: inFlightTTL}
	for _, o := range opts {
		o(g)
	}
	return g
}

// fireLocked notifies the observer of one terminal transition. It runs
// under the lock — see ResultObserver for the callback contract.
func (g *Gateway) fireLocked(rt *rtCommand) {
	if g.obs != nil {
		g.obs(rt.result())
	}
}

// SetTTL changes the no-flow queue TTL.
func (g *Gateway) SetTTL(d time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ttl = d
}

// SetEnvFn changes the envelope func used for built frames (the real
// cloud's value is a per-session constant; the hook sets it per session).
func (g *Gateway) SetEnvFn(fn byte) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.envFn = fn
}

// SetFlow sets the active inverter flow ("" = no flow). An in-flight
// command fails when its flow is lost or replaced (the operator resubmits);
// a new flow flushes the queue (expired commands stay visible, unsubmitted).
func (g *Gateway) SetFlow(connID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if connID == g.flow {
		return
	}
	if g.rt != nil {
		g.failCommandLocked(g.rt, "flow lost")
	}
	g.flow = connID
	g.flushLocked()
}

// HasFlow reports whether an inverter flow is active.
func (g *Gateway) HasFlow() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.flow != ""
}

// Submit validates the command (errors never reach the wire) and either
// starts it on the active flow or queues it: no flow → queued with TTL; a
// command already in flight → ErrInFlight, except behind a poll read,
// where an operator command queues (it starts when the read completes).
// A poll (OriginPoll) never queues: no flow → ErrNoFlow, anything in
// flight → ErrInFlight. It returns the command id.
func (g *Gateway) Submit(ctx context.Context, c Command) (string, error) {
	if err := g.admit(ctx, c); err != nil {
		return "", err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.expireInFlightLocked()
	if c.Kind != KindRead {
		if err := g.budgetCheckLocked(c.Kind); err != nil {
			return "", err
		}
	}
	poll := c.Origin == OriginPoll
	if (poll || c.Kind == KindClock) && g.flow == "" {
		return "", ErrNoFlow // a poll and a clock value are only worth sending now
	}
	if poll && g.rt != nil {
		return "", fmt.Errorf("%w: %s", ErrInFlight, g.rt.id)
	}
	g.seq++
	id := fmt.Sprintf("cmd-%d", g.seq)
	rt := &rtCommand{
		id: id, cmd: c, enqAt: g.clock(), status: StatusQueued,
		done: make(chan struct{}),
	}
	if g.flow == "" {
		g.queued = append(g.queued, rt)
		return id, nil
	}
	if g.rt != nil {
		if g.rt.cmd.Origin == OriginPoll {
			g.queued = append(g.queued, rt)
			return id, nil
		}
		return "", fmt.Errorf("%w: %s", ErrInFlight, g.rt.id)
	}
	if err := g.startLocked(rt); err != nil {
		g.failCommandLocked(rt, err.Error())
		return id, err
	}
	return id, nil
}

// admit runs the checks outside the lock: validation, the context, and
// the C-008 write gate (the authorizer reads the store).
func (g *Gateway) admit(ctx context.Context, c Command) error {
	if err := validate(c); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.Kind != KindRead && g.authz != nil {
		return g.authz(c)
	}
	return nil
}

// Pending returns the operator-visible commands: the in-flight one (if any),
// everything queued, and the terminal-but-visible ones (expired / failed),
// in order. Acked commands are dropped. Nothing is silently dropped.
func (g *Gateway) Pending() []PendingCommand {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.expireInFlightLocked()
	g.expireLocked()
	out := make([]PendingCommand, 0, len(g.queued)+len(g.stale)+1)
	if g.rt != nil {
		out = append(out, g.rt.pending())
	}
	for _, rt := range g.queued {
		out = append(out, rt.pending())
	}
	for _, rt := range g.stale {
		out = append(out, rt.pending())
	}
	return out
}

// startLocked injects the command's opening frames on the active flow.
// schedule: the read phase (fn03); other kinds: the write frames.
func (g *Gateway) startLocked(rt *rtCommand) error {
	writing := false // the frames below are the command's writes (budgeted)
	var frs []frame.Frame
	switch rt.cmd.Kind {
	case KindSchedule:
		rt.reads = scheduleReads()
		frs = readFrames(rt.cmd, g.envFn)
	case KindRead:
		p := rt.cmd.Params
		rt.reads = []readReq{{p[ParamAddr], p[ParamCount]}}
		//nolint:gosec // G115: fn validated to 3 or 4.
		frs = []frame.Frame{modbusFrame(g.envFn, readPDU(byte(p[ParamFn]), p[ParamAddr], p[ParamCount]))}
	case KindClock:
		// never read first: the clock is always behind or ahead by now
		frs, rt.writes = clockFrames(rt.cmd, g.envFn)
		writing = true
	case KindWorkMode, KindLimits, KindMeterCompensation,
		KindCurrentLimits, KindBatteryCurrents, KindPowerLimit, KindPortLimits, KindEPSMode:
		if g.skipUnchanged {
			// read first: the write phase is built (or skipped) when
			// the read answers (ackReadLocked)
			rt.reads = directReads(rt.cmd)
			for _, r := range rt.reads {
				frs = append(frs, modbusFrame(g.envFn, fn03PDU(r.addr, r.cnt)))
			}
			break
		}
		var err error
		frs, rt.writes, err = writeFrames(rt.cmd, nil, g.envFn)
		if err != nil {
			return err
		}
		writing = true
	}
	for i := range frs {
		if err := g.inj.InjectS2C(g.flow, frs[i].Raw); err != nil {
			return fmt.Errorf("gateway: inject: %w", err)
		}
		if writing && i == 0 {
			g.noteWriteLocked(rt.cmd.Kind) // counted once a frame reached the session
		}
	}
	rt.status = StatusInFlight
	rt.startAt = g.clock()
	g.rt = rt
	return nil
}

// expireInFlightLocked fails an in-flight command that has had no
// response within inFlightTTL and starts the next queued one.
func (g *Gateway) expireInFlightLocked() {
	if g.rt == nil || g.clock().Sub(g.rt.startAt) < g.flTTL {
		return
	}
	g.failCommandLocked(g.rt, fmt.Sprintf("no response from the inverter within %s", g.flTTL))
	g.flushLocked()
}

// flushLocked starts queued commands while the slot is free and a flow is
// active. Expired commands are never submitted (they stay visible in
// Pending; nothing is silently dropped).
func (g *Gateway) flushLocked() {
	if g.flow == "" {
		return
	}
	g.expireLocked()
	for g.rt == nil && len(g.queued) > 0 {
		rt := g.queued[0]
		g.queued = g.queued[1:]
		if err := g.startLocked(rt); err != nil {
			g.failCommandLocked(rt, err.Error())
		}
	}
}

// expireLocked lazily moves queued commands past their TTL into the visible
// stale set as expired (injected clock, no timers — C-004).
func (g *Gateway) expireLocked() {
	now := g.clock()
	kept := g.queued[:0]
	for _, rt := range g.queued {
		if now.Sub(rt.enqAt) >= g.ttl {
			rt.status = StatusExpired
			close(rt.done)
			g.stale = append(g.stale, rt)
			g.fireLocked(rt)
		} else {
			kept = append(kept, rt)
		}
	}
	g.queued = kept
}

func (g *Gateway) failCommandLocked(rt *rtCommand, detail string) {
	rt.status = StatusFailed
	rt.detail = detail
	close(rt.done)
	if g.rt == rt {
		g.rt = nil
	}
	g.stale = append(g.stale, rt)
	g.fireLocked(rt)
}

func (rt *rtCommand) pending() PendingCommand {
	return PendingCommand{
		ID:         rt.id,
		Kind:       rt.cmd.Kind,
		Origin:     rt.cmd.Origin,
		Status:     rt.status,
		EnqueuedAt: rt.enqAt,
		Detail:     rt.detail,
	}
}

// ---------- validation ----------

// slot is one decoded schedule group (10 registers on the wire).
type slot struct {
	en, start, end, wm, maxsoc, minsoc, fdsoc, fdpwr int
}

// hhmmMax is the largest valid packed HHMM (23:59).
const hhmmMax = 23<<8 | 59

// parseSlots extracts the eight schedule slots; absent fields default to 0
// (a missing "<k>_en" disables the slot with all other fields zero).
func parseSlots(c Command) []slot {
	slots := make([]slot, slotCount)
	for k := range slots {
		g := func(f SlotField) int { return c.Params[SlotParam(k+1, f)] }
		s := &slots[k]
		s.en = g(SlotEnabled)
		s.start = g(SlotStart)
		s.end = g(SlotEnd)
		s.wm = g(SlotWorkMode)
		s.maxsoc = g(SlotMaxSoc)
		s.minsoc = g(SlotMinSoc)
		s.fdsoc = g(SlotFdSoc)
		s.fdpwr = g(SlotFdPwr)
	}
	return slots
}

// hhmmMin converts packed HHMM to minutes.
func hhmmMin(v int) int { return v>>8*60 + v&0xFF }

// enabledSlotBounds applies the installer's ranges to an enabled slot
// (disabled slots keep the looser wire bounds: the inverter holds values
// there that a round-trip must be able to write back).
func enabledSlotBounds(pfx string, s slot) error {
	for _, f := range []struct {
		name string
		v    int
	}{{"maxsoc", s.maxsoc}, {"minsoc", s.minsoc}, {"fdsoc", s.fdsoc}} {
		if err := rangeInt(pfx+f.name, f.v, slotSocMin, socMax); err != nil {
			return err
		}
	}
	return rangeInt(pfx+"fdpwr", s.fdpwr, 0, slotFdPwrMax)
}

func rangeInt(field string, v, lo, hi int) error {
	if v < lo || v > hi {
		return fmt.Errorf("%w: %s = %d outside %d..%d", ErrOutOfRange, field, v, lo, hi)
	}
	return nil
}

func validateHHMM(field string, v int) error {
	if err := rangeInt(field, v, 0, hhmmMax); err != nil {
		return err
	}
	if v>>8 > 23 || v&0xFF > 59 {
		return fmt.Errorf("%w: %s = %#04x is not packed HHMM", ErrOutOfRange, field, v)
	}
	return nil
}

// scheduleParamKeys is the allowed param key set for the schedule kind.
func scheduleParamKeys() map[Param]bool {
	keys := map[Param]bool{ParamEnabled: true}
	for k := range slotCount {
		for _, f := range SlotFields {
			keys[SlotParam(k+1, f)] = true
		}
	}
	return keys
}

// validate checks a command against the register map's ranges (C-003):
// unknown kind / unknown param / out-of-range are all errors at Submit,
// never on the wire.
func validate(c Command) error {
	check := func(allowed map[Param]bool, f func(Command) error) error {
		for k := range c.Params {
			if !allowed[k] {
				return fmt.Errorf("%w: %q (kind %s)", ErrUnknownParam, k, c.Kind)
			}
		}
		return f(c)
	}
	switch c.Kind {
	case KindWorkMode:
		return check(map[Param]bool{ParamMode: true}, validateWorkMode)
	case KindLimits:
		return check(map[Param]bool{ParamImport: true, ParamExport: true}, validateLimits)
	case KindMeterCompensation:
		return check(map[Param]bool{ParamValue: true}, validateMeterComp)
	case KindSchedule:
		return check(scheduleParamKeys(), validateSchedule)
	case KindRead:
		return check(map[Param]bool{ParamFn: true, ParamAddr: true, ParamCount: true}, validateRead)
	case KindCurrentLimits, KindBatteryCurrents, KindPowerLimit, KindPortLimits, KindEPSMode:
		return validateSetting(c)
	case KindClock:
		allowed := map[Param]bool{}
		for _, k := range clockParams {
			allowed[k] = true
		}
		return check(allowed, validateClock)
	default:
		return fmt.Errorf("%w: %q", ErrUnknownKind, c.Kind)
	}
}

func validateRead(c Command) error {
	for _, k := range []Param{ParamFn, ParamAddr, ParamCount} {
		if _, ok := c.Params[k]; !ok {
			return fmt.Errorf("%w: %s", ErrMissingParam, k)
		}
	}
	if fn := c.Params[ParamFn]; fn != fnRead && fn != fnInputRead {
		return fmt.Errorf("%w: fn %d (want 3 holding or 4 input)", ErrOutOfRange, fn)
	}
	if err := rangeInt("count", c.Params[ParamCount], 1, maxReadCount); err != nil {
		return err
	}
	return rangeInt("addr", c.Params[ParamAddr], 0, regSpace-c.Params[ParamCount])
}

func validateWorkMode(c Command) error {
	if _, ok := c.Params[ParamMode]; !ok {
		return fmt.Errorf("%w: mode", ErrMissingParam)
	}
	return rangeInt("mode", c.Params[ParamMode], 0, 1)
}

func validateLimits(c Command) error {
	if _, ok := c.Params[ParamImport]; !ok {
		if _, ok := c.Params[ParamExport]; !ok {
			return ErrNoFields
		}
		return rangeInt("export", c.Params[ParamExport], 0, limitMax)
	}
	if err := rangeInt("import", c.Params[ParamImport], 0, limitMax); err != nil {
		return err
	}
	if v, ok := c.Params[ParamExport]; ok {
		return rangeInt("export", v, 0, limitMax)
	}
	return nil
}

func validateMeterComp(c Command) error {
	if _, ok := c.Params[ParamValue]; !ok {
		return fmt.Errorf("%w: value", ErrMissingParam)
	}
	return rangeInt("value", c.Params[ParamValue], compMin, compMax)
}

// validateSchedule checks the 8-group table (openapi setSchedule rules:
// overlapping enabled windows, end <= start; disabled slots skip the
// window checks — the inverter holds non-zero fields on disabled groups).
func validateSchedule(c Command) error {
	if _, ok := c.Params[ParamEnabled]; !ok {
		return fmt.Errorf("%w: enabled", ErrMissingParam)
	}
	if err := rangeInt("enabled", c.Params[ParamEnabled], 0, 1); err != nil {
		return err
	}
	slots := parseSlots(c)
	var wins []window
	for k := range slots {
		win, err := validateSlot(fmt.Sprintf("%d_", k+1), slots[k])
		if err != nil {
			return err
		}
		if win.lo >= 0 {
			wins = append(wins, win)
		}
	}
	return checkOverlap(wins)
}

// window is an enabled slot's half-open [lo, hi) window in minutes.
type window struct{ lo, hi int }

// validateSlot checks one group's fields. Disabled slots skip the
// window/workmode checks (the inverter holds non-zero fields on disabled
// groups); enabled slots must have end > start.
func validateSlot(pfx string, s slot) (window, error) {
	if err := validateHHMM(pfx+"start", s.start); err != nil {
		return window{}, err
	}
	if err := validateHHMM(pfx+"end", s.end); err != nil {
		return window{}, err
	}
	if err := rangeInt(pfx+"maxsoc", s.maxsoc, 0, socMax); err != nil {
		return window{}, err
	}
	if err := rangeInt(pfx+"minsoc", s.minsoc, 0, socMax); err != nil {
		return window{}, err
	}
	if err := rangeInt(pfx+"fdsoc", s.fdsoc, 0, socMax); err != nil {
		return window{}, err
	}
	if err := rangeInt(pfx+"fdpwr", s.fdpwr, 0, limitMax); err != nil {
		return window{}, err
	}
	if s.en == 0 {
		return window{lo: -1}, rangeInt(pfx+"workmode", s.wm, 0, slotWMMax)
	}
	if err := rangeInt(pfx+"workmode", s.wm, 1, slotWMMax); err != nil {
		return window{}, err
	}
	if err := enabledSlotBounds(pfx, s); err != nil {
		return window{}, err
	}
	lo, hi := hhmmMin(s.start), hhmmMin(s.end)
	if hi <= lo {
		return window{}, fmt.Errorf("%w: %s end <= start", ErrScheduleWindow, pfx)
	}
	return window{lo, hi}, nil
}

// checkOverlap rejects partially overlapping enabled windows. Full
// containment is legal — the device's own live state is the 00:00–23:59
// default group containing the narrower charge/discharge windows (profile
// §5). Adjacent half-open windows [start, end) are fine.
func checkOverlap(wins []window) error {
	for i := range wins {
		for j := i + 1; j < len(wins); j++ {
			a, b := wins[i], wins[j]
			partial := a.lo < b.hi && b.lo < a.hi
			aContainsB := a.lo <= b.lo && b.hi <= a.hi
			bContainsA := b.lo <= a.lo && a.hi <= b.hi
			if partial && !aContainsB && !bContainsA {
				return ErrScheduleOverlap
			}
		}
	}
	return nil
}
