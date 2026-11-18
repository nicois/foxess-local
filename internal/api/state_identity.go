package api

import (
	"foxesslocal/internal/compat"
	"foxesslocal/internal/device"
	"foxesslocal/internal/driver"
	"foxesslocal/internal/identity"
	"foxesslocal/internal/store"
)

// Identity is the spec's Identity schema (design D-020): who the
// inverter says it is, and whether that is a verified profile.
type Identity struct {
	SN       string            `json:"sn"`
	Model    string            `json:"model"`
	Firmware identity.Firmware `json:"firmware"`
	// VerifiedProfile names the exactly matching verified profile; null
	// for any other model or firmware.
	VerifiedProfile *string `json:"verified_profile"`
	// Compatibility is the per-feature confidence level (D-020); the
	// checks behind it are GET /compatibility.
	Compatibility map[string]string `json:"compatibility"`
	// Writes is each write feature's support on this device's layout
	// (verified / untested / unmapped / not_supported; unmapped while
	// no layout is established) — the UI's "not available here".
	Writes map[string]string `json:"writes"`
}

// writeFeatures are the features the API can write.
var writeFeatures = []string{
	compat.FeatureWorkMode, compat.FeaturePowerLimits, compat.FeatureMeterCompensation, compat.FeatureSchedule,
	compat.FeatureCurrentLimits, compat.FeatureBatteryCurrents, compat.FeaturePowerLimit, compat.FeaturePortLimits, compat.FeatureEPSMode,
}

// stateIdentity is /state's identity with the per-feature summary (the
// same device view the C-008 write gate decides on).
func (s *Server) stateIdentity(st store.State) (Identity, error) {
	snap, err := device.LoadWith(s.st, st)
	if err != nil {
		return Identity{}, err
	}
	return identityOf(snap), nil
}

func identityOf(snap device.Snapshot) Identity {
	id := Identity{
		SN: snap.SN, Model: snap.Model, Firmware: snap.Firmware, Compatibility: compatSummary(snap.Report),
		Writes: map[string]string{},
	}
	d, ok := snap.Driver()
	for _, f := range writeFeatures {
		id.Writes[f] = string(driver.Unmapped)
		if !ok {
			continue
		}
		sup := d.WriteSupport(f)
		// a verified encoding is only verified on the exact profile
		if sup == driver.Verified && snap.Report.Profile == "" {
			sup = driver.Untested
		}
		id.Writes[f] = string(sup)
	}
	// an identity fact (model + firmware), independent of whether the
	// register layout has been established yet
	if p, ok := identity.MatchVerified(snap.Model, snap.Firmware); ok {
		name := p.Name
		id.VerifiedProfile = &name
	}
	return id
}

// compatSummary is the feature → level map /state carries.
func compatSummary(rep compat.Report) map[string]string {
	out := make(map[string]string, len(rep.Features))
	for _, f := range rep.Features {
		out[f.Name] = string(f.Level)
	}
	return out
}
