package gateway

import (
	"errors"
	"fmt"
	"time"

	"foxesslocal/internal/frame"
)

// Inverter-wear protections (2026-11-17). Reads never wear the inverter;
// writes land in its non-volatile memory, whose endurance is finite and
// unknown for this hardware. Two options bound them:
//
//   - WithSkipUnchanged: every write reads its registers first and is
//     skipped when the inverter already holds the value (the command is
//     acknowledged as Unchanged). An automation re-sending the current
//     state, or a retried write that had landed, costs one read.
//   - WithWriteBudget: real writes per setting are capped per rolling
//     window; over the cap a write is refused up front (ErrWriteBudget).

// ErrWriteBudget refuses a write over the per-setting budget (the error is
// a *BudgetError; errors.Is matches this).
var ErrWriteBudget = errors.New("gateway: write budget exhausted for this setting")

// BudgetError is a refusal over the budget: the setting, its limit and
// when the oldest counted write leaves the window.
type BudgetError struct {
	Kind     CommandKind
	Limit    int
	Window   time.Duration
	NextFree time.Time
}

func (e *BudgetError) Error() string {
	return fmt.Sprintf("%v: %d writes in the last %s (next allowed at %s)", ErrWriteBudget,
		e.Limit, e.Window, e.NextFree.Format(time.RFC3339))
}

// Is makes errors.Is(err, ErrWriteBudget) hold.
func (e *BudgetError) Is(target error) bool { return target == ErrWriteBudget }

// BudgetState is one setting's budget now.
type BudgetState struct {
	Limit     int
	Remaining int
	Window    time.Duration
	NextFree  *time.Time // when Remaining next goes up; nil while it is Limit
}

// writeKinds are the budgeted settings (every write; reads are free).
var writeKinds = []CommandKind{
	KindWorkMode, KindLimits, KindMeterCompensation, KindSchedule,
	KindCurrentLimits, KindBatteryCurrents, KindPowerLimit, KindPortLimits, KindEPSMode,
}

// limitBlock is the 41021..41024 read covering both power limits.
const limitBlock = 2 * limitRegs

// WithSkipUnchanged reads before every write and skips writes the
// inverter already holds (a schedule push only when the whole table is
// unchanged: a changed push keeps the complete cloud-verified sequence).
func WithSkipUnchanged() Option {
	return func(g *Gateway) { g.skipUnchanged = true }
}

// WithWriteBudget caps real writes per setting (command kind) at n per
// rolling window.
func WithWriteBudget(n int, window time.Duration) Option {
	return func(g *Gateway) { g.budget, g.budgetWindow = n, window }
}

// WithWriteBudgetFor overrides the per-window cap for some settings (the
// rest keep WithWriteBudget's n; 0 leaves a setting unbudgeted).
func WithWriteBudgetFor(n map[CommandKind]int) Option {
	return func(g *Gateway) { g.budgetFor = n }
}

// limitFor is a setting's cap (0: unbudgeted).
func (g *Gateway) limitFor(k CommandKind) int {
	if n, ok := g.budgetFor[k]; ok {
		return n
	}
	return g.budget
}

// Budget is every budgeted setting's state now (unbudgeted settings are
// absent).
func (g *Gateway) Budget() map[CommandKind]BudgetState {
	g.budgetMu.Lock()
	defer g.budgetMu.Unlock()
	out := map[CommandKind]BudgetState{}
	for _, k := range writeKinds {
		limit := g.limitFor(k)
		if limit <= 0 || g.budgetWindow <= 0 {
			continue
		}
		kept := g.pruneLocked(k)
		st := BudgetState{Limit: limit, Remaining: max(0, limit-len(kept)), Window: g.budgetWindow}
		if len(kept) > 0 {
			t := kept[0].Add(g.budgetWindow)
			st.NextFree = &t
		}
		out[k] = st
	}
	return out
}

// pruneLocked drops a setting's writes that left the window and returns
// the rest, oldest first (budgetMu held).
func (g *Gateway) pruneLocked(k CommandKind) []time.Time {
	if g.written == nil {
		g.written = map[CommandKind][]time.Time{}
	}
	cut := g.clock().Add(-g.budgetWindow)
	kept := g.written[k][:0]
	for _, t := range g.written[k] {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	g.written[k] = kept
	return kept
}

// directReads is the read before a direct write (work mode, meter
// compensation, limits).
func directReads(c Command) []readReq {
	switch c.Kind {
	case KindWorkMode:
		return []readReq{{regWorkMode, 1}}
	case KindMeterCompensation:
		return []readReq{{regMeterComp, 1}}
	case KindLimits:
		return []readReq{{regImportLim, limitBlock}}
	case KindCurrentLimits, KindBatteryCurrents, KindPowerLimit, KindPortLimits, KindEPSMode:
		return settingReads(c)
	case KindSchedule, KindRead:
	}
	return nil
}

// budgetCheckLocked refuses a write over the budget (pruning the window).
func (g *Gateway) budgetCheckLocked(k CommandKind) error {
	limit := g.limitFor(k)
	if limit <= 0 {
		return nil
	}
	g.budgetMu.Lock()
	defer g.budgetMu.Unlock()
	if kept := g.pruneLocked(k); len(kept) >= limit {
		return &BudgetError{Kind: k, Limit: limit, Window: g.budgetWindow, NextFree: kept[0].Add(g.budgetWindow)}
	}
	return nil
}

// noteWriteLocked counts one write phase that reaches the inverter.
func (g *Gateway) noteWriteLocked(k CommandKind) {
	if g.limitFor(k) <= 0 {
		return
	}
	g.budgetMu.Lock()
	defer g.budgetMu.Unlock()
	if g.written == nil {
		g.written = map[CommandKind][]time.Time{}
	}
	g.written[k] = append(g.written[k], g.clock())
}

// changedWrites drops the writes whose registers already hold the
// written values (frs and wrs are index-aligned). A schedule push is
// all-or-nothing.
func changedWrites(k CommandKind, frs []frame.Frame, wrs []writeReq, current []RegisterValue) ([]frame.Frame, []writeReq) {
	cur := curMap(current)
	var (
		outF []frame.Frame
		outW []writeReq
	)
	for i := range frs {
		if !holdsWrite(frs[i].Payload, cur) {
			outF = append(outF, frs[i])
			outW = append(outW, wrs[i])
		}
	}
	if k == KindSchedule && len(outF) > 0 {
		return frs, wrs
	}
	return outF, outW
}

// holdsWrite reports whether every register a write PDU sets already
// holds its value (an unparsable PDU never counts as held).
func holdsWrite(p []byte, cur map[uint16]uint16) bool {
	mb, err := frame.ParseModbus(p)
	if err != nil {
		return false
	}
	switch mb.Fn {
	case fnWriteOne:
		v, ok := cur[regAddr(mb.Addr)]
		return ok && int(v) == mb.Value
	case fnWriteMulti:
		for i := range mb.Count {
			v, ok := cur[regAddr(mb.Addr+i)]
			if !ok || int(v) != int(mb.Data[bytesPerReg*i])<<byteShift|int(mb.Data[bytesPerReg*i+1]) {
				return false
			}
		}
		return true
	}
	return false
}
