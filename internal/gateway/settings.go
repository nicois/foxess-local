package gateway

import (
	"fmt"
	"sort"

	"foxesslocal/internal/frame"
)

// The installer settings: single-register fn06 writes, each proven by
// a captured cloud write with a verbatim echo on the KH10 fw 1.73
// profile (2026-11-16 write-through session, every value reverted and
// re-read byte-identical — protocol profile §5). Ranges are the
// installer schema's, in wire units.
const (
	// KindCurrentLimits sets the grid import / export current limits
	// (0.1 A: 41027 / 41028).
	KindCurrentLimits CommandKind = "currentlimits"
	// KindBatteryCurrents sets the max charge / discharge current
	// (0.1 A: 41007 / 41008).
	KindBatteryCurrents CommandKind = "batterycurrents"
	// KindPowerLimit sets the inverter power limit (% of rating: 41016).
	KindPowerLimit CommandKind = "powerlimit"
	// KindPortLimits sets the grid-port load / generation limit power
	// (W: 41200 / 41201).
	KindPortLimits CommandKind = "portlimits"
	// KindEPSMode sets the EPS (backup) port mode (41015: 3 UPS, 2 EPS;
	// 1 "disabled" is presumed and was never written — refused).
	KindEPSMode CommandKind = "epsmode"
)

// regSetting is one setting register: its command param and range.
type regSetting struct {
	param    Param
	addr     int
	min, max int
}

// settingKinds is the register table, in write order per kind.
var settingKinds = map[CommandKind][]regSetting{
	KindCurrentLimits:   {{ParamImport, 41027, 20, 1000}, {ParamExport, 41028, 20, 1000}},
	KindBatteryCurrents: {{ParamCharge, 41007, 0, 500}, {ParamDischarge, 41008, 0, 500}},
	KindPowerLimit:      {{ParamPct, 41016, 0, 100}},
	KindPortLimits:      {{ParamLoad, 41200, 0, 65535}, {ParamGeneration, 41201, 0, 65535}},
	KindEPSMode:         {{ParamMode, 41015, epsModeEPS, epsModeUPS}},
}

// EPS port modes on the wire.
const (
	epsModeEPS = 2
	epsModeUPS = 3
)

// validateSetting: only the kind's params, at least one, each in range.
func validateSetting(c Command) error {
	regs := settingKinds[c.Kind]
	known := map[Param]regSetting{}
	for _, r := range regs {
		known[r.param] = r
	}
	if len(c.Params) == 0 {
		return ErrNoFields
	}
	for k, v := range c.Params {
		r, ok := known[k]
		if !ok {
			return fmt.Errorf("%w: %q (kind %s)", ErrUnknownParam, k, c.Kind)
		}
		if err := rangeInt(k.String(), v, r.min, r.max); err != nil {
			return err
		}
	}
	return nil
}

// settingFrames is one fn06 write per present param, in table order.
func settingFrames(c Command, envFn byte) ([]frame.Frame, []writeReq) {
	var (
		frs []frame.Frame
		wrs []writeReq
	)
	for _, r := range settingKinds[c.Kind] {
		v, ok := c.Params[r.param]
		if !ok {
			continue
		}
		frs = append(frs, modbusFrame(envFn, fn06PDU(r.addr, v)))
		wrs = append(wrs, writeReq{fn: fnWriteOne, addr: r.addr, val: v})
	}
	return frs, wrs
}

// settingReads is the read before the write: one block spanning the
// kind's registers (they are adjacent).
func settingReads(c Command) []readReq {
	regs := settingKinds[c.Kind]
	addrs := make([]int, 0, len(regs))
	for _, r := range regs {
		addrs = append(addrs, r.addr)
	}
	sort.Ints(addrs)
	return []readReq{{addrs[0], addrs[len(addrs)-1] - addrs[0] + 1}}
}
