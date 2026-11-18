package api

// Config-event sections (the spec's ConfigEvent.changed vocabulary): the
// client re-renders what a section names.
const (
	SectionWorkMode          = "work-mode"
	SectionLimits            = "limits"
	SectionMeterCompensation = "meter-compensation"
	SectionSchedule          = "schedule"
	SectionIdentity          = "identity"
	SectionForwardMode       = "forward-mode"
)

// Identity registers (the model string 30000 ×15, the versions
// 30016–30018).
const (
	regIdentityFirst = 30000
	regIdentityLast  = 30018
	slotTableRegs    = 80
)

// sectionOrder is the stable order SectionsOf reports in.
var sectionOrder = []string{SectionWorkMode, SectionLimits, SectionMeterCompensation, SectionSchedule, SectionIdentity}

// SectionsOf maps changed register addresses to the config-event
// sections they belong to (each once, in a stable order); live values and
// unmapped registers map to nothing.
func SectionsOf(addrs []int) []string {
	hit := map[string]bool{}
	for _, a := range addrs {
		if s := sectionOf(a); s != "" {
			hit[s] = true
		}
	}
	var out []string
	for _, s := range sectionOrder {
		if hit[s] {
			out = append(out, s)
		}
	}
	return out
}

func sectionOf(a int) string {
	switch {
	case a == regWorkModeAddr:
		return SectionWorkMode
	case a >= regImportLimAddr && a <= regExportLimAddr+1, a == regImportCurAddr, a == regExportCurAddr,
		a == regMaxChgCurAddr, a == regMaxChgCurAddr+1, a == regEpsOutputAddr, a == regPowerLimAddr,
		a == regLoadLimAddr, a == regLoadLimAddr+1:
		// the Control page's limits (its numbered markers 1–5)
		return SectionLimits
	case a == regMeterCompAddr:
		return SectionMeterCompensation
	case a >= regModeEnAddr && a < regSlotTableAddr+slotTableRegs:
		return SectionSchedule
	case a >= regIdentityFirst && a <= regIdentityLast:
		return SectionIdentity
	}
	return ""
}
