// Package device combines the inverter's identity, the per-feature
// compatibility report and the layout driver into one view, and decides
// whether a write may reach the inverter (C-008).
package device

import (
	"errors"
	"fmt"
	"time"

	"foxesslocal/internal/compat"
	"foxesslocal/internal/driver"
	"foxesslocal/internal/identity"
)

// Write refusals. Each is distinct so callers (API, simulator, scheduler)
// can tell why: impossible (unmapped / not supported), untested (the
// installer override can allow it), not yet identified (retry later), or
// the checks say the mapping looks wrong (not overridable).
var (
	ErrWriteUnsupported  = errors.New("write impossible on this device")
	ErrWriteUnmapped     = fmt.Errorf("%w: no known register or encoding (unmapped)", ErrWriteUnsupported)
	ErrWriteNotSupported = fmt.Errorf("%w: the inverter does not support it (not_supported)", ErrWriteUnsupported)
	ErrWriteUnverified   = errors.New("write untested on this model / firmware: an installer can enable writes for this exact device")
	ErrNotIdentified     = errors.New("device not identified yet")
	ErrChecksFailing     = errors.New("a plausibility check for this feature is failing: the register map looks wrong")
)

// Override is the installer's grant of untested writes, bound to the
// exact identity it was granted for (C-008, amended 2026-11-16).
type Override struct {
	Model    string            `json:"model"`
	Firmware identity.Firmware `json:"firmware"`
	By       string            `json:"by"`
	At       time.Time         `json:"at"`
}

// Covers reports whether the grant applies to a device identity.
func (o *Override) Covers(model string, fw identity.Firmware) bool {
	return o != nil && model != "" && o.Model == model && o.Firmware == fw
}

// View is what the write decision needs.
type View struct {
	Model    string
	Firmware identity.Firmware
	Report   compat.Report
}

// Driver is the layout driver, if any.
func (v View) Driver() (driver.Driver, bool) { return driver.Select(v.Report.Layout) }

func (v View) level(feature string) compat.Level {
	for _, f := range v.Report.Features {
		if f.Name == feature {
			return f.Level
		}
	}
	return compat.LevelUnknown
}

// Authorize decides one write (feature = compat feature name).
func Authorize(feature string, v View, ov *Override) error {
	if v.Model == "" {
		return ErrNotIdentified
	}
	lvl := v.level(feature)
	d, ok := v.Driver()
	if !ok {
		if lvl == compat.LevelPending {
			return ErrNotIdentified
		}
		return fmt.Errorf("%w (no driver for model %q)", ErrWriteUnmapped, v.Model)
	}
	if !v.Firmware.Complete() {
		return ErrNotIdentified
	}
	switch d.WriteSupport(feature) {
	case driver.NotSupported:
		return fmt.Errorf("%w (%s on %s)", ErrWriteNotSupported, feature, d.Name())
	case driver.Unmapped:
		return fmt.Errorf("%w (%s on %s)", ErrWriteUnmapped, feature, d.Name())
	case driver.Verified, driver.Untested:
		// an encoding exists: the device's confidence decides
	}
	switch lvl {
	case compat.LevelPending:
		return ErrNotIdentified
	case compat.LevelUnknown:
		return ErrChecksFailing
	case compat.LevelExact:
		if d.WriteSupport(feature) == driver.Verified {
			return nil
		}
	case compat.LevelCompatible, compat.LevelTentative:
		// same layout with unverified firmware, or a third-party map:
		// only the installer's grant
	}
	if ov.Covers(v.Model, v.Firmware) {
		return nil
	}
	return ErrWriteUnverified
}
