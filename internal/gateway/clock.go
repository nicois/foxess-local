package gateway

import (
	"fmt"
	"time"

	"foxesslocal/internal/frame"
)

// KindClock sets the inverter's clock: one fn10 write of six registers
// at 40000 (year, month, day, hour, minute, second, the site's local
// wall time — the inverter keeps no time zone). The cloud's own clock
// write, echoed by the inverter (profile §5). A clock value is right
// only when it is sent, so the command is never queued.
const KindClock CommandKind = "clock"

// The clock block: 0x9C40 ×6.
const (
	regClock    = 0x9C40
	clockRegs   = 6
	clockMinYr  = 2000 // the installer schema's range
	clockMaxYr  = 2099
	hoursPerDay = 24
	minsPerHour = 60
	monthsPerYr = 12
)

// clockParams are the KindClock params, in register order.
var clockParams = []Param{ParamYear, ParamMonth, ParamDay, ParamHour, ParamMinute, ParamSecond}

// validateClock: all six fields, each in range, and a date that exists.
func validateClock(c Command) error {
	for _, k := range clockParams {
		if _, ok := c.Params[k]; !ok {
			return fmt.Errorf("%w: %s", ErrMissingParam, k)
		}
	}
	p := c.Params
	for _, r := range []struct {
		k      Param
		lo, hi int
	}{
		{ParamYear, clockMinYr, clockMaxYr},
		{ParamMonth, 1, monthsPerYr},
		{ParamDay, 1, 31},
		{ParamHour, 0, hoursPerDay - 1},
		{ParamMinute, 0, minsPerHour - 1},
		{ParamSecond, 0, minsPerHour - 1},
	} {
		if err := rangeInt(r.k.String(), p[r.k], r.lo, r.hi); err != nil {
			return err
		}
	}
	if d := time.Date(p[ParamYear], time.Month(p[ParamMonth]), p[ParamDay], 0, 0, 0, 0, time.UTC); d.Day() != p[ParamDay] {
		return fmt.Errorf("%w: day %d is not in month %d", ErrOutOfRange, p[ParamDay], p[ParamMonth])
	}
	return nil
}

// clockFrames is the one fn10 write.
func clockFrames(c Command, envFn byte) ([]frame.Frame, []writeReq) {
	vals := make([]uint16, 0, clockRegs)
	for _, k := range clockParams {
		vals = append(vals, regVal(c.Params[k]))
	}
	return []frame.Frame{modbusFrame(envFn, fn10PDU(regClock, vals))},
		[]writeReq{{fn: fnWriteMulti, addr: regClock, cnt: clockRegs}}
}
