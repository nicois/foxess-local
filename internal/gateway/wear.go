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

// ErrWriteBudget refuses a write over the per-setting budget.
var ErrWriteBudget = errors.New("gateway: write budget exhausted for this setting")

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
	if g.budget <= 0 {
		return nil
	}
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
	if len(kept) >= g.budget {
		return fmt.Errorf("%w: %d writes in the last %s (next allowed at %s)", ErrWriteBudget,
			len(kept), g.budgetWindow, kept[0].Add(g.budgetWindow).Format(time.RFC3339))
	}
	return nil
}

// noteWriteLocked counts one write phase that reaches the inverter.
func (g *Gateway) noteWriteLocked(k CommandKind) {
	if g.budget <= 0 {
		return
	}
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
