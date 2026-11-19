package forward

import (
	"context"
	"fmt"
	"time"

	"foxesslocal/internal/gateway"
	"foxesslocal/internal/store"
)

const (
	// forceChargeWM is the wire work mode for force charge (the sim's
	// wmForceCharge; the gateway's enabled range is 1..7).
	forceChargeWM = 6
	// defaultMaxSoc is the maxSoC assumed when a slot has none stored
	// (mirrors the sim's defaultMaxSoc).
	defaultMaxSoc = 100
	// the installer schema's segmented-time ranges (dump 2026-11-16):
	// SoC fields 10..100, fd power 0..10500 W. minSocOnGrid is the floor
	// the cloud writes into its own force-charge groups.
	socFloor     = 10
	socCeil      = 100
	fdPwrMax     = 10500
	minSocOnGrid = socFloor
	// slotCount is the number of time groups in the 48010 block.
	slotCount = 8
	// opEnable/opDisable are the wire values for a slot's enable flag.
	opEnable  = 1
	opDisable = 0
	// tickPeriod is the evaluation period (windows are minute-aligned;
	// the 30 s period bounds the scheduling lag).
	tickPeriod = 30 * time.Second
	// waitTimeout bounds the wait for one push's verdict.
	waitTimeout = 30 * time.Second
	// audKindSchedule is the audit kind for scheduler pushes.
	audKindSchedule = "schedule"
	// maxBackoff caps the retry delay of a failing push (doubling from
	// tickPeriod: 30 s, 1, 2, 4, 8, 15 min) — a push that reached the
	// inverter but lost its acknowledgement must not be rewritten every
	// tick (inverter non-volatile memory wear).
	maxBackoff = 15 * time.Minute
	// alertAfter is the consecutive failures that raise one alert.
	alertAfter = 3
)

// Window is one recurring force-charge window: the inverter's slot is
// armed (work mode force charge) from Start to End, daily.
type Window struct {
	Name  string
	Slot  int // 1..8
	Start int // packed HHMM (HH<<8|MM)
	End   int // packed HHMM, strictly after Start (no midnight wrap)
	FdSoc int // forced-charge SoC, 10..100 (installer range)
	FdPwr int // forced-charge power cap W, 0..10500 (installer range)
}

type phase int

const (
	phaseIdle   phase = iota // slot armed off (or never opened this day)
	phaseActive              // the open push was acked; the close is pending
)

// Scheduler arms and disarms force-charge windows in the inverter's
// 48010 time-slot table. Every attempted push is a single
// gateway.Submit(KindSchedule) — the sole validated upstream→inverter
// path (C-003) — and leaves exactly one audit row (kind "schedule"):
//
//	"opened <name>: acked" | "opened <name>: submit: <err>" |
//	"opened <name>: wait: <err>" | "opened <name>: <status>[: <detail>]"
//
// (and the "closed …" analogues).
//
// A failed push (submit error — including ErrInFlight — or a
// non-acked verdict) changes no phase; the window retries with a
// doubling backoff (30 s … maxBackoff), and the alertAfter-th
// consecutive failure raises one alert. Success, or midnight, resets it.
//
// All mutable state is owned by the goroutine running Run (the
// constructor output is immutable); no locking is needed.
type Scheduler struct {
	st     *store.Store
	gw     *gateway.Gateway
	clock  func() time.Time
	wins   []Window
	states []phase
	day    time.Time
	fails  []int       // consecutive failed pushes per window
	retry  []time.Time // no push before this, per window (backoff)
	alert  func(string)
	// wall is the inverter's wall time now (OnWallClock); nil: clock()
	wall func() (time.Time, bool)
}

// OnWallClock sets where the windows' times are read: the inverter's
// wall time (its clock, else the site zone); false = unknown, and the
// tick does nothing. Without it the injected clock's zone is used
// (tests). Set before Run.
func (s *Scheduler) OnWallClock(f func() (time.Time, bool)) { s.wall = f }

// tick evaluates once, on the wall time when it is known.
func (s *Scheduler) tick(ctx context.Context) {
	if s.wall == nil {
		s.evaluate(ctx, s.clock())
		return
	}
	if now, ok := s.wall(); ok {
		s.evaluate(ctx, now)
	}
}

// OnAlert installs the alert sink (the UI's system alert feed; nil =
// audit only). Set before Run.
func (s *Scheduler) OnAlert(f func(string)) { s.alert = f }

// NewScheduler validates the windows (slot range, packed HHMM,
// end strictly after start, fd SoC / power in the installer ranges,
// no same-slot overlap — the gateway would
// reject any of them on every tick) and builds the scheduler.
func NewScheduler(st *store.Store, gw *gateway.Gateway, clock func() time.Time, windows []Window) (*Scheduler, error) {
	s := &Scheduler{
		st:     st,
		gw:     gw,
		clock:  clock,
		wins:   windows,
		states: make([]phase, len(windows)),
		fails:  make([]int, len(windows)),
		retry:  make([]time.Time, len(windows)),
	}
	for _, w := range windows {
		if err := checkWindow(w); err != nil {
			return nil, err
		}
	}
	if err := checkWindowOverlap(windows); err != nil {
		return nil, err
	}
	return s, nil
}

// checkWindow validates one window's shape (slot range, packed HHMM,
// end strictly after start).
func checkWindow(w Window) error {
	if w.Slot < 1 || w.Slot > slotCount {
		return fmt.Errorf("forward: window %q: slot %d outside 1..%d", w.Name, w.Slot, slotCount)
	}
	if err := checkHHMM("start", w.Start); err != nil {
		return fmt.Errorf("forward: window %q: %w", w.Name, err)
	}
	if err := checkHHMM("end", w.End); err != nil {
		return fmt.Errorf("forward: window %q: %w", w.Name, err)
	}
	if hhmmMin(w.End) <= hhmmMin(w.Start) {
		return fmt.Errorf("forward: window %q: end must be strictly after start", w.Name)
	}
	if w.FdSoc < socFloor || w.FdSoc > socCeil {
		return fmt.Errorf("forward: window %q: fd_soc %d outside %d..%d", w.Name, w.FdSoc, socFloor, socCeil)
	}
	if w.FdPwr < 0 || w.FdPwr > fdPwrMax {
		return fmt.Errorf("forward: window %q: fd_pwr %d outside 0..%d", w.Name, w.FdPwr, fdPwrMax)
	}
	return nil
}

// checkWindowOverlap rejects two windows on the same slot with
// intersecting spans (containment included — the slot is one table row).
func checkWindowOverlap(windows []Window) error {
	for i := range windows {
		for j := i + 1; j < len(windows); j++ {

			a, b := windows[i], windows[j]
			if a.Slot != b.Slot {
				continue
			}
			alo, ahi := hhmmMin(a.Start), hhmmMin(a.End)
			blo, bhi := hhmmMin(b.Start), hhmmMin(b.End)
			if alo < bhi && blo < ahi {
				return fmt.Errorf("forward: windows %q and %q overlap on slot %d", a.Name, b.Name, a.Slot)
			}
		}
	}
	return nil
}

// Run drives the scheduler until ctx is done: an immediate evaluation,
// then one every tickPeriod (the ticker is runtime-virtualized under
// synctest — C-004; no wall-clock sleeps).
func (s *Scheduler) Run(ctx context.Context) {
	tick := time.NewTicker(tickPeriod)
	defer tick.Stop()
	s.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.tick(ctx)
		}
	}
}

// evaluate is one scheduler tick: a midnight rollover resets any stuck
// phase (windows cannot wrap midnight, so a still-active window at
// midnight is stuck on a failed close — the next cycle re-arms and
// closes it), then each window advances at most one phase:
// idle → active when the current minute enters [start, end) and the
// open push is acked; active → idle when it reaches end and the close
// push is acked. A minute past a window's end that was never opened
// does nothing (close only reverses a successful open).
func (s *Scheduler) evaluate(ctx context.Context, now time.Time) {
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	if s.day != today {
		s.day = today
		for i := range s.states {
			s.states[i] = phaseIdle
			s.fails[i], s.retry[i] = 0, time.Time{}
		}
	}
	st, storedAny := s.storedState()
	mins := now.Hour()*60 + now.Minute()
	for i := range s.wins {
		s.advanceWindow(ctx, i, mins, st, storedAny)
	}
}

// advanceWindow attempts at most one phase transition for one window
// (a push lands only on an acked verdict).
func (s *Scheduler) advanceWindow(ctx context.Context, i, mins int, st store.State, storedAny bool) {
	if s.clock().Before(s.retry[i]) {
		return // backing off after a failed push
	}
	w := s.wins[i]
	lo, hi := hhmmMin(w.Start), hhmmMin(w.End)
	switch s.states[i] {
	case phaseIdle:
		if mins >= lo && mins < hi {
			if s.attempt(ctx, i, "opened", openTarget(w, st, storedAny), st, storedAny) {
				s.states[i] = phaseActive
			}
		}
	case phaseActive:
		if mins >= hi {
			if s.attempt(ctx, i, "closed", closeTarget(), st, storedAny) {
				s.states[i] = phaseIdle
			}
		}
	}
}

// attempt runs one push and keeps the window's backoff: success resets
// it; a failure doubles the delay (from tickPeriod, capped at
// maxBackoff) and the alertAfter-th consecutive one alerts.
func (s *Scheduler) attempt(ctx context.Context, i int, action string, target slotTarget, st store.State, storedAny bool) bool {
	w := s.wins[i]
	ok, detail := s.push(ctx, action, w, target, st, storedAny)
	if ok {
		s.fails[i], s.retry[i] = 0, time.Time{}
		return true
	}
	s.fails[i]++
	delay := min(tickPeriod<<min(s.fails[i]-1, 10), maxBackoff) //nolint:mnd // shift cap: no overflow
	s.retry[i] = s.clock().Add(delay)
	if s.fails[i] == alertAfter && s.alert != nil {
		s.alert(fmt.Sprintf("scheduler: %s %q failed %d times (%s); retrying with backoff, at most every %s",
			action, w.Name, s.fails[i], detail, maxBackoff))
	}
	return false
}

// storedState is the last-decoded 48010 table (zero when none seen).
func (s *Scheduler) storedState() (store.State, bool) {
	st, err := s.st.QueryState()
	if err != nil {
		return store.State{}, false
	}
	return st, !st.SchedTS.IsZero()
}

// slotTarget is one time group's wire values for a push.
type slotTarget struct {
	en, start, end, wm, maxsoc, minsoc, fdsoc, fdpwr int
}

// openTarget arms the window's slot in force charge, preserving the
// stored maxSoC (the default when none is stored).
func openTarget(w Window, st store.State, storedAny bool) slotTarget {
	return slotTarget{
		en:     opEnable,
		start:  w.Start,
		end:    w.End,
		wm:     forceChargeWM,
		maxsoc: storedMaxSoc(st, storedAny, w.Slot),
		minsoc: minSocOnGrid,
		fdsoc:  w.FdSoc,
		fdpwr:  w.FdPwr,
	}
}

// closeTarget clears the window's slot (the inverter disarms on
// enable=0; the gateway writes a cleared slot in the cloud's
// unused-group shape).
func closeTarget() slotTarget { return slotTarget{en: opDisable} }

// storedTarget copies one group from the last-decoded table, preserving
// a stored non-default maxSoC (the sim's rule verbatim).
func storedTarget(st store.State, storedAny bool, k int) slotTarget {
	if k > len(st.Schedule) || !st.Schedule[k-1].Enabled {
		return slotTarget{en: opDisable}
	}
	sl := st.Schedule[k-1]
	return slotTarget{
		en: opEnable,

		start: sl.StartH<<8 | sl.StartM,

		end:    sl.EndH<<8 | sl.EndM,
		wm:     sl.WorkMode,
		maxsoc: storedMaxSoc(st, storedAny, k),
		minsoc: sl.MinSoc,
		fdsoc:  sl.FDSoc,
		fdpwr:  sl.FDPower,
	}
}

// storedMaxSoc preserves the last-observed maxSoC of a stored slot
// (default when nothing is stored).
func storedMaxSoc(st store.State, storedAny bool, k int) int {
	if storedAny && k <= len(st.Schedule) && st.Schedule[k-1].MaxSoc > 0 {
		return st.Schedule[k-1].MaxSoc
	}
	return defaultMaxSoc
}

// params builds the full 8-group schedule param map: the target slot at
// its target, every other slot copied from the last-decoded table (the
// gateway's schedule write is read-modify-write, and a full table is
// the only shape whose effect is fully known).
func (s *Scheduler) params(w Window, target slotTarget, st store.State, storedAny bool) map[gateway.Param]int {
	params := make(map[gateway.Param]int, 1+slotCount*len(gateway.SlotFields))
	enabled := opDisable
	for k := 1; k <= slotCount; k++ {
		key := func(f gateway.SlotField) gateway.Param { return gateway.SlotParam(k, f) }
		tg := target
		if k != w.Slot {
			tg = storedTarget(st, storedAny, k)
		}
		if tg.en != opEnable {
			params[key(gateway.SlotEnabled)] = opDisable
			continue
		}
		enabled = opEnable
		params[key(gateway.SlotEnabled)] = opEnable
		params[key(gateway.SlotStart)] = tg.start
		params[key(gateway.SlotEnd)] = tg.end
		params[key(gateway.SlotWorkMode)] = tg.wm
		params[key(gateway.SlotMinSoc)] = tg.minsoc
		params[key(gateway.SlotFdSoc)] = tg.fdsoc
		params[key(gateway.SlotFdPwr)] = tg.fdpwr
		params[key(gateway.SlotMaxSoc)] = tg.maxsoc
	}
	params[gateway.ParamEnabled] = enabled
	return params
}

// push sends one full-table push for a window transition and reports
// whether the gateway acked it (the caller advances the phase only
// then) and the outcome's detail. Exactly one audit row per attempt.
func (s *Scheduler) push(ctx context.Context, action string, w Window, target slotTarget, st store.State, storedAny bool) (bool, string) {
	cmdID, err := s.gw.Submit(ctx, gateway.Command{
		Kind:   gateway.KindSchedule,
		Params: s.params(w, target, st, storedAny),
		Origin: "scheduler:" + w.Name,
	})
	if err != nil {
		detail := "submit: " + err.Error()
		s.audit(action, w.Name, detail)
		return false, detail
	}
	wctx, cancel := context.WithTimeout(ctx, waitTimeout)
	defer cancel()
	res, err := s.gw.Wait(wctx, cmdID)
	if err != nil {
		detail := "wait: " + err.Error()
		s.audit(action, w.Name, detail)
		return false, detail
	}
	if res.Status != gateway.StatusAcked {
		detail := string(res.Status)
		if res.Detail != "" {
			detail += ": " + res.Detail
		}
		s.audit(action, w.Name, detail)
		return false, detail
	}
	detail := "acked"
	if res.Unchanged {
		detail = "acked (unchanged, not written)"
	}
	s.audit(action, w.Name, detail)
	return true, detail
}

func (s *Scheduler) audit(action, name, detail string) {
	s.st.RecordAudit(s.clock(), audKindSchedule,
		fmt.Sprintf("%s %s: %s", action, name, detail))
}

// hhmmMin converts packed HHMM to minutes of the day.
func hhmmMin(v int) int { return v>>8*60 + v&0xFF }

// checkHHMM rejects values that are not packed HHMM (the gateway
// rejects them too; the constructor refuses them up front instead of
// failing every tick).
func checkHHMM(field string, v int) error {
	if v>>8 > 23 || v&0xFF > 59 {
		return fmt.Errorf("%s = %#04x is not packed HHMM", field, v)
	}
	return nil
}
