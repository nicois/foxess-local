// Package policy — red-first test table for the C-001 default-deny verdict
// engine: the mode is the dial — full passes the kinds the cloud is known
// to send and holds the rest (a firmware push needs a window: D-025),
// telemetry_only passes only Modbus reads, off has no cloud leg, and an
// active maintenance window overrides the mode with pass-through.
package policy

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"foxesslocal/internal/frame"
)

// firmwareChunk is a firmware-sized payload: what a firmware push would
// carry (an unrecognised kind — held in full, dropped in telemetry_only).
var firmwareChunk = make([]byte, 2048)

// mbPDU builds a raw Modbus request PDU: unit 0x01, fn, BE16 addr, then
// count (fn03/04), value (fn06), or count+data (fn10).
func mbPDU(fn byte, addr, x int, data ...byte) []byte {
	//nolint:gosec // G115: test register values are far under 65536.
	p := []byte{0x01, fn, byte(addr >> 8), byte(addr)}
	switch fn {
	case 0x03, 0x04:
		//nolint:gosec // G115: test register values are far under 65536.
		p = append(p, byte(x>>8), byte(x))
	case 0x06:
		//nolint:gosec // G115: test register values are far under 65536.
		p = append(p, byte(x>>8), byte(x))
	case 0x10:
		cnt := len(data) / 2
		//nolint:gosec // G115: test register values are far under 65536.
		p = append(p, byte(cnt>>8), byte(cnt), byte(len(data)))
		p = append(p, data...)
	}
	return p
}

// mkFrame is the minimal frame the engine sees (Decide uses only Payload).
func mkFrame(p []byte) frame.Frame {
	return frame.Frame{Payload: append([]byte(nil), p...)}
}

// allKinds is the full class table (the named frame.Kind constants).
var allKinds = []frame.Kind{
	frame.KindTele, frame.KindKeepalive, frame.KindRSSI, frame.KindCfg40,
	frame.KindRegister, frame.KindMbecho, frame.KindCfg91, frame.KindSerialQuery,
	frame.KindMbread, frame.KindMbreadResp, frame.KindMbWrite, frame.KindBootstrapResp,
	frame.KindOther,
}

// knownCloudKinds are the cloud's frame kinds: every s2c frame in the capture
// corpus (2,925 streams, 2026-11-04…10) and in the owner's 2026-11-18
// full-mode session is one of these.
var knownCloudKinds = []frame.Kind{frame.KindBootstrapResp, frame.KindMbread, frame.KindMbWrite, frame.KindSerialQuery}

// TestFullPassesKnownKinds: full mode passes every kind of frame the
// cloud is known to send — session control, reads, writes (config and
// schedule pushes included) and the serial query.
func TestFullPassesKnownKinds(t *testing.T) {
	for _, k := range knownCloudKinds {
		for _, f := range []frame.Frame{mkFrame(nil), mkFrame(mbPDU(0x10, 41021, 1, 99, 0))} {
			v := Decide(Posture{Mode: ModeFull}, f, k)
			require.Equal(t, ActionPass, v.Action, "kind=%s", k)
			require.Equal(t, "mode:full", v.Reason, "kind=%s", k)
		}
	}
}

// TestFullHoldsUnrecognised (owner, 2026-11-18): full mode does not let
// firmware through. Firmware frames have never been seen on this
// inverter, so they cannot be recognised; instead every kind the cloud is
// not known to send — a firmware push among them — is held, and needs a
// maintenance window. The reason says so.
func TestFullHoldsUnrecognised(t *testing.T) {
	for _, k := range allKinds {
		if slices.Contains(knownCloudKinds, k) {
			continue
		}
		v := Decide(Posture{Mode: ModeFull}, mkFrame(firmwareChunk), k)
		require.Equal(t, ActionDrop, v.Action, "kind=%s", k)
		require.Contains(t, v.Reason, "maintenance window", "kind=%s", k)
		v = Decide(Posture{Mode: ModeFull, Window: true}, mkFrame(firmwareChunk), k)
		require.Equal(t, ActionPass, v.Action, "kind=%s in a window", k)
	}
}

// TestTelemetryOnlyDeniesEveryKind: telemetry_only — the cloud sees
// everything (c2s is unfiltered) but can never write back: every upstream
// frame is dropped, including firmware-sized payloads (nothing is held —
// dropped, logged, done).
func TestTelemetryOnlyDeniesEveryKind(t *testing.T) {
	for _, k := range allKinds {
		for _, f := range []frame.Frame{mkFrame(nil), mkFrame(firmwareChunk), mkFrame(mbPDU(0x10, 41021, 1, 99, 0))} {
			v := Decide(Posture{Mode: ModeTelemetryOnly}, f, k)
			require.Equal(t, ActionDrop, v.Action, "kind=%s", k)
			require.True(t, strings.HasPrefix(v.Reason, "mode:telemetry_only deny:"), "kind=%s: %s", k, v.Reason)
		}
	}
}

// TestOffGuard: off mode has no cloud leg; the guard is defensive (the
// hook never consults Decide for s2c in off) and drops with the stable
// off-mode reason.
func TestOffGuard(t *testing.T) {
	v := Decide(Posture{Mode: ModeOff}, mkFrame(firmwareChunk), frame.KindOther)
	require.Equal(t, ActionDrop, v.Action)
	require.Equal(t, "off-mode: no s2c frames exist (hook-generated only)", v.Reason)
}

// TestWindowOverridesEveryMode: the active maintenance window is a
// transparent pass-through over whatever mode is configured — full,
// telemetry_only, or off — and the reason is stable.
func TestWindowOverridesEveryMode(t *testing.T) {
	for _, m := range []ForwardMode{ModeFull, ModeTelemetryOnly, ModeOff} {
		v := Decide(Posture{Mode: m, Window: true}, mkFrame(firmwareChunk), frame.KindMbWrite)
		require.Equal(t, ActionPass, v.Action, "mode=%s", m)
		require.Equal(t, "maintenance-window", v.Reason, "mode=%s", m)
	}
}

// TestTelemetryOnlyDenyReasonNamesFrame: the drop reason names the
// unmatched frame — Modbus PDUs by fn/addr (+ count, or value for fn06),
// non-Modbus payloads by kind alone — so the audit trail says what was
// denied.
func TestTelemetryOnlyDenyReasonNamesFrame(t *testing.T) {
	cases := []struct {
		kind   frame.Kind
		frame  frame.Frame
		reason string
	}{
		{
			frame.KindMbWrite, mkFrame(mbPDU(0x10, 41021, 1, 99, 0)),
			"mode:telemetry_only deny:kind=mbwrite fn=0x10 addr=41021 count=1",
		},
		{
			// a read of 0 registers is not a well-formed request: denied
			frame.KindMbread, mkFrame(mbPDU(0x03, 48010, 0)),
			"mode:telemetry_only deny:kind=mbread fn=0x03 addr=48010 count=0",
		},
		{
			frame.KindMbWrite, mkFrame(mbPDU(0x06, 40001, 42)),
			"mode:telemetry_only deny:kind=mbwrite fn=0x06 addr=40001 value=42",
		},
		{
			frame.KindTele, mkFrame(firmwareChunk),
			"mode:telemetry_only deny:kind=tele",
		},
	}
	for _, c := range cases {
		v := Decide(Posture{Mode: ModeTelemetryOnly}, c.frame, c.kind)
		require.Equal(t, ActionDrop, v.Action, c.reason)
		require.Equal(t, c.reason, v.Reason)
	}
}

// TestActionString renders the verdict actions for the audit trail.
func TestActionString(t *testing.T) {
	require.Equal(t, "pass", ActionPass.String())
	require.Equal(t, "drop", ActionDrop.String())
	require.Equal(t, "action(9)", Action(9).String())
}

// TestTelemetryOnlyPassesReads (owner, 2026-11-17): in telemetry_only the
// cloud keeps getting answers to requests that cannot change the
// inverter — a well-formed Modbus read (fn03 holding / fn04 input, 1–125
// registers, a request not a response) passes; every write, including
// the cloud's RTC sync (fn10 @0x9C40), stays denied.
func TestTelemetryOnlyPassesReads(t *testing.T) {
	pass := []frame.Frame{
		{Payload: mbPDU(0x03, 48000, 10)},  // the scheduler master poll
		{Payload: mbPDU(0x03, 48010, 80)},  // the time-group table
		{Payload: mbPDU(0x04, 10110, 28)},  // the 10110 input block
		{Payload: mbPDU(0x03, 0x9C40, 6)},  // the RTC read
		{Payload: mbPDU(0x03, 31000, 125)}, // the Modbus maximum
	}
	for _, f := range pass {
		v := Decide(Posture{Mode: ModeTelemetryOnly}, f, frame.KindMbread)
		require.Equal(t, ActionPass, v.Action, "%x", f.Payload)
		require.True(t, strings.HasPrefix(v.Reason, "mode:telemetry_only read:"), v.Reason)
	}
	rtcWrite := mbPDU(0x10, 0x9C40, 0, 0x07, 0xea, 0, 0x0b, 0, 0x11, 0, 0x17, 0, 0x2a, 0, 0x05)
	resp := frame.Frame{Payload: mbPDU(0x03, 48000, 10)}
	resp.Device[0] = 0x91 // a response, not a request
	deny := []struct {
		kind frame.Kind
		f    frame.Frame
	}{
		{frame.KindMbWrite, frame.Frame{Payload: rtcWrite}},
		{frame.KindMbWrite, frame.Frame{Payload: mbPDU(0x06, 41000, 1)}},
		{frame.KindMbread, resp},
		{frame.KindMbread, frame.Frame{Payload: mbPDU(0x03, 31000, 126)}},                    // over the Modbus maximum
		{frame.KindMbread, frame.Frame{Payload: append(mbPDU(0x03, 31000, 2), 0x00)}},        // trailing bytes
		{frame.KindMbread, frame.Frame{Payload: []byte{0x02, 0x03, 0xbb, 0x80, 0x00, 0x0a}}}, // another unit
		{frame.KindOther, frame.Frame{Payload: mbPDU(0x03, 48000, 10)}},                      // not classified a read
		{frame.KindSerialQuery, frame.Frame{Payload: make([]byte, 57)}},
	}
	for _, d := range deny {
		v := Decide(Posture{Mode: ModeTelemetryOnly}, d.f, d.kind)
		require.Equal(t, ActionDrop, v.Action, "kind=%s %x", d.kind, d.f.Payload)
	}
}
