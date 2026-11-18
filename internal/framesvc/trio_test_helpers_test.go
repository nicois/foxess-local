package framesvc

import (
	"testing"

	"foxesslocal/internal/policy"
)

// regFrameA is the first c2s registration frame (dev0 0x2a) — the only
// one the lockstep inverter sends until a bootstrap response is
// delivered (profile §3, 2026-11-08 drop experiment).
func regFrameA(t *testing.T) []byte {
	t.Helper()
	return mkFrame(t, startC2S, devRegA, 0x5f, regPayload)
}

// cloudTrio is a real-cloud-style bootstrap-response trio with session
// bytes distinct from the time-derived local parameters (so a local
// injection would be byte-visible against it).
func cloudTrio(t *testing.T) []byte {
	t.Helper()
	var all []byte
	all = append(all, mkFrame(t, startC2S, [4]byte{0xaa, 0x6a, 0xec, 0x5c}, 0x94, []byte{0x01, 0x01, 0x01, 0, 0})...)
	all = append(all, mkFrame(t, startC2S, [4]byte{0xac, 0x6a, 0xec, 0x5c}, 0x95, nil)...)
	all = append(all, mkFrame(t, startC2S, [4]byte{0xab, 0x6a, 0xec, 0x5c}, 0x95, []byte{0x01})...)
	return all
}

// telemetryOnlyPosture is the stage-1b cutover posture: c2s is mirrored
// upstream, every s2c frame is denied (C-001).
func telemetryOnlyPosture() policy.Posture {
	return policy.Posture{Mode: policy.ModeTelemetryOnly}
}
