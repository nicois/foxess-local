package api

import (
	"testing"
)

// snap01Fixture is a 141 B device-info payload in the live layout: SN,
// four firmware strings, model "KH10" space-padded (profile §4).
func snap01Fixture(fw ...string) []byte {
	p := make([]byte, 141)
	copy(p[0:15], "TESTSN000000001")
	for i, at := range []int{33, 39, 45, 51} {
		copy(p[at:at+4], fw[i])
	}
	copy(p[93:109], "KH10            ")
	return p
}

// TestStateIdentity: /state identifies the inverter (D-020) — the SN
// from the device-info snapshot, model and firmware from the
// 30000/30016 register reads when present (else the snapshot), and the
// verified profile only on an exact model + firmware match.
func TestStateIdentity(t *testing.T) {
	h := newHarness(t)
	h.seedTele(testBase, 1000, -50, 900, 950, 0, 90, 2)

	// nothing observed yet: empty strings, no profile
	if id := h.getState(t).Identity; id.SN != "" || id.Model != "" || id.VerifiedProfile != nil {
		t.Fatalf("before any identity data: %+v", id)
	}

	// the snapshot alone identifies the reference device
	h.seedFrame(testBase, dirC2S, "snap01", decisionForwarded, snap01Fixture("1.73", "1.03", "1.71", "0.00"), nil)
	id := h.getState(t).Identity
	if id.SN != "TESTSN000000001" || id.Model != refModel || id.Firmware.Master != "1.73" ||
		id.Firmware.Slave != "1.03" || id.Firmware.Manager != "1.71" {
		t.Fatalf("from snap01: %+v", id)
	}
	if id.VerifiedProfile == nil || *id.VerifiedProfile != "kh10-fw1.73" {
		t.Fatalf("reference device not matched: %v", id.VerifiedProfile)
	}

	// the register reads win: a firmware bump is no longer the
	// verified profile
	h.st.RecordRegisters(testBase, 30000, []uint16{0x4B48, 0x3130, 0x2020, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	h.st.RecordRegisters(testBase, 30016, []uint16{0x174, 0x103, 0x171})
	h.st.Flush()
	id = h.getState(t).Identity
	if id.Model != refModel || id.Firmware.Master != "1.74" || id.VerifiedProfile != nil {
		t.Fatalf("from registers (fw 1.74): %+v profile %v", id, id.VerifiedProfile)
	}
}

// TestStateWritesFollowConfidence: a verified encoding on a device that
// is not the exact verified profile reports "untested" (the gate needs
// the installer's grant), so the UI never promises a write the gate
// refuses.
func TestStateWritesFollowConfidence(t *testing.T) {
	h := newHarness(t)
	h.seedTele(testBase, 1000, -50, 900, 950, 0, 90, 2)
	h.seedReferenceSettings()
	if w := h.getState(t).Identity.Writes; w[jsonFieldWorkMode] != supportVerified {
		t.Fatalf("reference: %v", w)
	}
	h.setMaster(0x174)
	if w := h.getState(t).Identity.Writes; w[jsonFieldWorkMode] != "untested" || w["power_limits"] != "untested" {
		t.Fatalf("fw 1.74: %v", w)
	}
}

// supportVerified is identity.writes for a verified encoding.
const supportVerified = "verified"
