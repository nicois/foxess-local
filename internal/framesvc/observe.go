package framesvc

import (
	"foxesslocal/internal/frame"
	"foxesslocal/internal/store"
)

// Event kinds: the spec's SSE map minus the api-handler-owned events
// (forward-mode-changed, the maintenance transitions — those paths have
// no hook).
const (
	EventSessionUp   = "session-up"   // OnOpen admitted (serve decided, not rejected)
	EventSessionDown = "session-down" // OnClose (Reason = the close reason)
	EventTLS         = "tls"          // OnTLSResult (Host + Result; success | mint_failed | client_rejected)
	EventTelemetry   = "telemetry"    // live values: a 150 B c2s frame or a register poll (Tele holds it)
	EventWrite       = "write"        // a forwarded s2c Modbus write reached the inverter (PDU = its PDU)
	EventEnergy      = "energy"       // the lifetime energy totals were read (Energy holds them)
	// EventFirmwareTask: the cloud sent a firmware upgrade task (FwTask,
	// and Decision: forwarded in a maintenance window, else dropped).
	EventFirmwareTask = "firmware-task"
)

// Event is one observed stack event. The observer is called on the hook
// goroutine: it must be cheap (the host's adapter just publishes to the
// bus) and must never call back into the service or the gateway (both
// hold locks around the hook).
type Event struct {
	Kind   string
	ConnID string

	// SessionDown
	Reason string

	// TLS
	Host   string
	Result string

	// Telemetry
	Tele *frame.Tele

	// Write
	PDU []byte

	// Energy
	Energy *frame.KHEnergy

	// FirmwareTask
	FwTask   *frame.FwTask
	Decision store.Decision
}

// Observer observes stack events.
type Observer func(Event)

// WithObserver installs the event observer (nil = none, the tests'
// default behaviour).
func WithObserver(o Observer) Option {
	return func(s *Service) { s.obs = o }
}

// observe fires one event (nil-safe).
func (s *Service) observe(ev Event) {
	if s.obs != nil {
		s.obs(ev)
	}
}
