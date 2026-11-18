package device

import (
	"encoding/json"
	"fmt"
	"maps"

	"foxesslocal/internal/compat"
	"foxesslocal/internal/frame"
	"foxesslocal/internal/gateway"
	"foxesslocal/internal/identity"
	"foxesslocal/internal/store"
)

// Snapshot is the device view plus the serial number.
type Snapshot struct {
	SN string
	View
}

// classSnap01 is the device-info snapshot class.
const classSnap01 = frame.KindSnap01

// compatRanges are the register ranges the evaluation reads: identity,
// the KH live blocks, the settings, the scheduler.
var compatRanges = [][2]int{{30000, 30018}, {31000, 31054}, {41000, 41046}, {41200, 41202}, {48000, 48089}}

// Load builds the current device view from the store.
func Load(st *store.Store) (Snapshot, error) {
	qs, err := st.QueryState()
	if err != nil {
		return Snapshot{}, err
	}
	return LoadWith(st, qs)
}

// LoadWith is Load reusing an already-queried store state: the
// device-info snapshot gives the SN (and fallback model / firmware); the
// 30000 / 30016 register reads win when present.
func LoadWith(st *store.Store, qs store.State) (Snapshot, error) {
	p01, _, err := st.LatestPayload(classSnap01)
	if err != nil {
		return Snapshot{}, err
	}
	d := identity.FromSnapshots(p01, nil)
	out := Snapshot{SN: d.SN, Model: d.Model, Firmware: d.Firmware}

	regs := map[int]uint16{}
	for _, r := range compatRanges {
		m, err := st.RegistersRange(r[0], r[1])
		if err != nil {
			return Snapshot{}, err
		}
		maps.Copy(regs, m)
	}
	get := func(addr int) (uint16, bool) { v, ok := regs[addr]; return v, ok }
	if b, ok := block(get, identity.ModelAddr, identity.ModelCount); ok {
		if m := identity.ModelFromRegisters(b); m != "" {
			out.Model = m
		}
	}
	if b, ok := block(get, identity.VersionAddr, identity.VersionCount); ok {
		fw := identity.Firmware{
			Master:  identity.VersionString(b[0]),
			Slave:   identity.VersionString(b[1]),
			Manager: identity.VersionString(b[2]),
		}
		if fw.Complete() {
			out.Firmware = fw
		}
	}
	in := compat.Input{Model: out.Model, Firmware: out.Firmware, Regs: get}
	if !qs.SnapTS.IsZero() {
		soc := qs.SocPct
		in.SnapshotSoc = &soc
	}
	out.Report = compat.Evaluate(in)
	return out, nil
}

func block(get func(int) (uint16, bool), addr, count int) ([]uint16, bool) {
	out := make([]uint16, count)
	for i := range count {
		v, ok := get(addr + i)
		if !ok {
			return nil, false
		}
		out[i] = v
	}
	return out, true
}

// overrideKey is the settings key of the installer's write grant.
const overrideKey = "write_override"

// LoadOverride returns the persisted grant (nil when none).
func LoadOverride(st *store.Store) (*Override, error) {
	v, ok, err := st.Setting(overrideKey)
	if err != nil || !ok {
		return nil, err
	}
	var o Override
	if err := json.Unmarshal([]byte(v), &o); err != nil {
		return nil, fmt.Errorf("device: write override: %w", err)
	}
	return &o, nil
}

// SaveOverride persists a grant (replacing any previous one).
func SaveOverride(st *store.Store, o Override) error {
	b, err := json.Marshal(o)
	if err != nil {
		return err
	}
	return st.SetSetting(overrideKey, string(b))
}

// ClearOverride revokes the grant.
func ClearOverride(st *store.Store) error { return st.DeleteSetting(overrideKey) }

// Feature is the compat feature a gateway write kind belongs to.
func Feature(k gateway.CommandKind) string {
	switch k {
	case gateway.KindWorkMode:
		return compat.FeatureWorkMode
	case gateway.KindLimits:
		return compat.FeaturePowerLimits
	case gateway.KindMeterCompensation:
		return compat.FeatureMeterCompensation
	case gateway.KindSchedule:
		return compat.FeatureSchedule
	case gateway.KindCurrentLimits:
		return compat.FeatureCurrentLimits
	case gateway.KindBatteryCurrents:
		return compat.FeatureBatteryCurrents
	case gateway.KindPowerLimit:
		return compat.FeaturePowerLimit
	case gateway.KindPortLimits:
		return compat.FeaturePortLimits
	case gateway.KindEPSMode:
		return compat.FeatureEPSMode
	case gateway.KindRead:
		// reads are never gated
	}
	return string(k)
}

// GatewayAuthorizer is the store-backed C-008 write gate.
func GatewayAuthorizer(st *store.Store) gateway.Authorizer {
	return func(c gateway.Command) error {
		snap, err := Load(st)
		if err != nil {
			return fmt.Errorf("device: load: %w", err)
		}
		ov, err := LoadOverride(st)
		if err != nil {
			return err
		}
		return Authorize(Feature(c.Kind), snap.View, ov)
	}
}
