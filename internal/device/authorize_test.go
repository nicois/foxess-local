package device

import (
	"errors"
	"testing"

	"foxesslocal/internal/compat"
	"foxesslocal/internal/driver"
	"foxesslocal/internal/identity"
)

const fWM = "work_mode"

var refFW = identity.Firmware{Master: "1.73", Slave: "1.03", Manager: "1.71"}

// view builds a device view: model, firmware, layout, and one feature's
// level.
func view(model string, fw identity.Firmware, layout string, feature string, lvl compat.Level) View {
	return View{
		Model: model, Firmware: fw,
		Report: compat.Report{Layout: layout, Features: []compat.Feature{{Name: feature, Level: lvl}}},
	}
}

// TestAuthorize: every write outcome is a distinct error, so callers can
// tell "impossible" (unmapped / not supported) from "untested" (needs
// the installer override) from "not yet identified" from "the map looks
// wrong" (checks failing — not overridable).
func TestAuthorize(t *testing.T) {
	bumped := identity.Firmware{Master: "1.74", Slave: "1.03", Manager: "1.71"}
	grantBumped := &Override{Model: "KH10", Firmware: bumped}
	grantRef := &Override{Model: "KH10", Firmware: refFW}
	kh := driver.LayoutKHPre133
	cases := []struct {
		name    string
		feature string
		v       View
		ov      *Override
		want    error
	}{
		{"verified device", fWM, view("KH10", refFW, kh, fWM, compat.LevelExact), nil, nil},
		{"compatible firmware, no override", fWM, view("KH10", bumped, kh, fWM, compat.LevelCompatible), nil, ErrWriteUnverified},
		{"compatible firmware, override for it", fWM, view("KH10", bumped, kh, fWM, compat.LevelCompatible), grantBumped, nil},
		{"override for other firmware", fWM, view("KH10", bumped, kh, fWM, compat.LevelCompatible), grantRef, ErrWriteUnverified},
		{"not identified", fWM, view("", identity.Firmware{}, "", fWM, compat.LevelUnknown), nil, ErrNotIdentified},
		{"layout pending", fWM, view("KH10", refFW, "", fWM, compat.LevelPending), nil, ErrNotIdentified},
		{"checks failing", "power_limits", view("KH10", refFW, kh, "power_limits", compat.LevelUnknown), grantRef, ErrChecksFailing},
		{"unrecognised model", fWM, view("H1-5.0-E", identity.Firmware{Master: "1.80"}, "", fWM, compat.LevelUnknown), nil, ErrWriteUnmapped},
		{"hardware lacks it", "eps_recover_soc", view("KH10", refFW, kh, "eps_recover_soc", compat.LevelExact), grantRef, ErrWriteNotSupported},
	}
	for _, c := range cases {
		err := Authorize(c.feature, c.v, c.ov)
		if c.want == nil && err != nil || c.want != nil && !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	// both impossible kinds are also ErrWriteUnsupported
	for _, e := range []error{ErrWriteUnmapped, ErrWriteNotSupported} {
		if !errors.Is(e, ErrWriteUnsupported) {
			t.Errorf("%v is not ErrWriteUnsupported", e)
		}
	}
}

// TestAuthorizeTentative: a foxess_modbus layout's work mode (untested
// encoding) needs the installer's grant for that exact device; a feature
// the map does not cover is impossible (unmapped), grant or not.
func TestAuthorizeTentative(t *testing.T) {
	fw := identity.Firmware{Master: "1.44", Slave: "1.02", Manager: "1.30"}
	grant := &Override{Model: "H1-5.0-E-G2", Firmware: fw}
	v := View{Model: "H1-5.0-E-G2", Firmware: fw, Report: compat.Report{
		Layout: "H1_G2_144-HOLDING",
		Features: []compat.Feature{
			{Name: fWM, Level: compat.LevelTentative},
			{Name: "power_limits", Level: compat.LevelUnknown},
		},
	}}
	if err := Authorize(fWM, v, nil); !errors.Is(err, ErrWriteUnverified) {
		t.Errorf("tentative work mode, no grant: %v", err)
	}
	if err := Authorize(fWM, v, grant); err != nil {
		t.Errorf("tentative work mode, granted: %v", err)
	}
	if err := Authorize("power_limits", v, grant); !errors.Is(err, ErrWriteUnmapped) {
		t.Errorf("unmapped power limits: %v", err)
	}
}
