// Package policy is the s2c (upstream→inverter) verdict engine: the C-001
// default-deny core. The forward mode is the dial — full is full
// bidirectional parity (the explicit, installer-gated opt-in),
// telemetry_only passes only requests that cannot change the inverter
// (well-formed Modbus reads — owner, 2026-11-17: the cloud keeps getting
// answers to its own polls) and denies everything else, and off has no
// cloud leg at all.
// An active maintenance window overrides the mode with transparent
// pass-through. It is pure — no I/O, no clock, no config loading (config
// wiring is the host's). Decide is called only for s2c frames; c2s
// (inverter→cloud) is never gated.
//
// Firmware (owner, 2026-11-18): full does not let a firmware push
// through; only a maintenance window does. Firmware frames have never
// been seen on this inverter, so they cannot be recognised. Instead full
// passes the kinds the cloud is known to send (fullKinds: every s2c frame
// in the capture corpus and in a live full-mode session) and holds
// anything else until a window is open.
package policy

import (
	"fmt"

	"foxesslocal/internal/frame"
)

// ForwardMode is the frame-forwarding mode (C-001/C-007).
type ForwardMode string

// The forward modes.
const (
	// ModeOff: no cloud leg at all — the inverter's uplink terminates
	// locally (hook-generated frames only); no s2c frames exist and
	// Decide is never consulted for them.
	ModeOff ForwardMode = "off"
	// ModeTelemetryOnly: the cloud sees the inverter's data and gets
	// answers to its reads, but can never write back: a well-formed Modbus
	// read request passes; every other upstream frame is dropped and
	// logged.
	ModeTelemetryOnly ForwardMode = "telemetry_only"
	// ModeFull: the cloud may read and write as before — configuration,
	// schedules, the clock — but an unrecognised frame (a firmware push,
	// say) is held until a maintenance window is open. The explicit,
	// persistent, installer-gated, audited opt-in.
	ModeFull ForwardMode = "full"
)

// Action is the verdict on a single s2c frame.
type Action int

// The verdict actions.
const (
	// ActionPass forwards the frame to the inverter.
	ActionPass Action = iota
	// ActionDrop discards the frame (C-001 default deny) and logs it.
	ActionDrop
)

// String renders the action for logs and the audit trail.
func (a Action) String() string {
	switch a {
	case ActionPass:
		return "pass"
	case ActionDrop:
		return "drop"
	default:
		return fmt.Sprintf("action(%d)", int(a))
	}
}

// Verdict is the decision on one s2c frame plus its human-readable,
// audit-log-stable reason.
type Verdict struct {
	Action Action
	Reason string
}

// Posture is the effective forwarding policy for the channel.
type Posture struct {
	Mode ForwardMode
	// Window reports that an active maintenance window overrides the
	// mode: transparent pass-through regardless of the configured mode.
	Window bool
}

// offReason is the documented off-guard verdict: defensive, because the
// hook never calls Decide for s2c in off mode.
const offReason = "off-mode: no s2c frames exist (hook-generated only)"

// windowReason is the maintenance-window verdict (overrides every mode).
const windowReason = "maintenance-window"

// fullReason is the full-mode verdict for a frame the cloud is known to
// send.
const fullReason = "mode:full"

// fullKinds are the frame kinds full passes: everything the cloud has
// been seen to send — session control, Modbus reads and writes (config
// and schedule pushes, the clock) and the serial query. Measured: every
// s2c frame of the capture corpus (2,925 streams, 2026-11-04…10) and of
// the owner's 2026-11-18 full-mode session is one of these.
var fullKinds = map[frame.Kind]bool{
	frame.KindBootstrapResp: true,
	frame.KindMbread:        true,
	frame.KindMbWrite:       true,
	frame.KindSerialQuery:   true,
}

// Decide judges one s2c frame, in this order:
//
//  1. maintenance window — active → Pass (the window is a transparent
//     pass-through over whatever mode is configured);
//  2. full → Pass a kind the cloud is known to send; hold anything else
//     (a firmware push, say) until a maintenance window is open;
//  3. off → Drop (defensive — no s2c frames exist in off mode);
//  4. telemetry_only → Pass a well-formed Modbus read request (it cannot
//     change the inverter), Drop everything else (no write ever reaches it);
//  5. any other mode (a construction defect) → Drop.
func Decide(p Posture, f frame.Frame, k frame.Kind) Verdict {
	if p.Window {
		return Verdict{Action: ActionPass, Reason: windowReason}
	}
	switch p.Mode {
	case ModeFull:
		if fullKinds[k] {
			return Verdict{Action: ActionPass, Reason: fullReason}
		}
		return Verdict{Action: ActionDrop, Reason: "mode:full hold:" + describe(k, f) +
			" (not a kind the cloud is known to send — a firmware push, say: needs a maintenance window)"}
	case ModeOff:
		return Verdict{Action: ActionDrop, Reason: offReason}
	case ModeTelemetryOnly:
		if isReadRequest(f, k) {
			return Verdict{Action: ActionPass, Reason: "mode:telemetry_only read:" + describe(k, f)}
		}
		return Verdict{Action: ActionDrop, Reason: "mode:telemetry_only deny:" + describe(k, f)}
	default: // unknown mode (a construction defect): deny all
		return Verdict{Action: ActionDrop, Reason: "mode:unknown deny:" + describe(k, f)}
	}
}

// Modbus read request shape: unit 1, fn03 / fn04, BE16 address, BE16
// count — exactly six bytes.
const (
	readPDULen   = 6
	modbusUnit   = 0x01
	fnReadHold   = 0x03
	fnReadInput  = 0x04
	maxReadCount = 125 // the Modbus limit for one read
	respBit      = 0x80
)

// isReadRequest reports whether a frame is a well-formed Modbus read
// request: classified as a read, no response bit, exactly the six-byte
// PDU for unit 1 with fn03 or fn04 and 1–125 registers. Anything less
// exact is not recognised and stays denied.
func isReadRequest(f frame.Frame, k frame.Kind) bool {
	p := f.Payload
	if k != frame.KindMbread || f.Device[0]&respBit != 0 || len(p) != readPDULen || p[0] != modbusUnit {
		return false
	}
	if p[1] != fnReadHold && p[1] != fnReadInput {
		return false
	}
	n := int(p[4])<<8 | int(p[5])
	return n >= 1 && n <= maxReadCount
}

// describe renders an unmatched frame for the deny reason: kind always;
// fn/addr (+ count for fn03/04/10, value for fn06) when the payload is a
// parseable Modbus request PDU.
func describe(k frame.Kind, f frame.Frame) string {
	m, err := frame.ParseModbus(f.Payload)
	switch {
	case err != nil:
		return "kind=" + string(k)
	case m.Fn == 0x06:
		return fmt.Sprintf("kind=%s fn=0x06 addr=%d value=%d", k, m.Addr, m.Value)
	default:
		return fmt.Sprintf("kind=%s fn=0x%02x addr=%d count=%d", k, m.Fn, m.Addr, m.Count)
	}
}
