package gateway

import "strconv"

// Param names one Command parameter. Its field is unexported, so the
// values below (and SlotParam's) are the only ones that exist outside
// this package: a misspelt parameter does not compile. (A named string
// type would not do — Go converts an untyped string literal such as
// "exprot" to it silently.)
type Param struct{ name string }

// String is the parameter's wire name (errors, logs).
func (p Param) String() string { return p.name }

// The parameters, by command kind.
var (
	// KindRead: function code (3 holding / 4 input), first register,
	// register count.
	ParamFn    = Param{"fn"}
	ParamAddr  = Param{"addr"}
	ParamCount = Param{"count"}

	// KindWorkMode: the work mode; KindEPSMode: the EPS port mode.
	ParamMode = Param{"mode"}
	// KindMeterCompensation: the offset, W.
	ParamValue = Param{"value"}
	// KindLimits (W) and KindCurrentLimits (0.1 A): the grid import and
	// export limits.
	ParamImport = Param{"import"}
	ParamExport = Param{"export"}
	// KindSchedule: the scheduler master switch (with the SlotParam
	// fields).
	ParamEnabled = Param{"enabled"}
	// KindBatteryCurrents: the max charge / discharge current, 0.1 A.
	ParamCharge    = Param{"charge"}
	ParamDischarge = Param{"discharge"}
	// KindPowerLimit: % of the rating.
	ParamPct = Param{"pct"}
	// KindPortLimits: the grid-port load / generation limit, W.
	ParamLoad       = Param{"load"}
	ParamGeneration = Param{"generation"}
)

// SlotField names one field of a schedule slot (see SlotParam).
type SlotField struct{ name string }

// String is the field's wire name.
func (f SlotField) String() string { return f.name }

// The schedule slot fields.
var (
	SlotEnabled  = SlotField{"en"}       // opEnable / opDisable
	SlotStart    = SlotField{"start"}    // packed HHMM
	SlotEnd      = SlotField{"end"}      // packed HHMM
	SlotWorkMode = SlotField{"workmode"} // wire enum 1..7
	SlotMaxSoc   = SlotField{"maxsoc"}
	SlotMinSoc   = SlotField{"minsoc"}
	SlotFdSoc    = SlotField{"fdsoc"}
	SlotFdPwr    = SlotField{"fdpwr"}
)

// SlotFields are every slot field, in wire order.
var SlotFields = []SlotField{SlotEnabled, SlotStart, SlotEnd, SlotWorkMode, SlotMaxSoc, SlotMinSoc, SlotFdSoc, SlotFdPwr}

// SlotParam is the KindSchedule parameter for one field of slot (1..8).
func SlotParam(slot int, f SlotField) Param {
	return Param{strconv.Itoa(slot) + "_" + f.name}
}
