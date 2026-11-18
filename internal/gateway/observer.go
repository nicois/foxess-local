package gateway

import "time"

// ResultObserver fires exactly once per command, at its terminal
// transition (acked / failed / expired) — the same moment a Wait
// consumer sees it. It is called under the gateway lock: the adapter
// must be cheap (publish to a bus, write a row) and must never call
// back into the gateway (Submit/Wait/Ack/SetFlow) — that would deadlock.
type ResultObserver func(Result)

// Option customises the gateway at construction.
type Option func(*Gateway)

// WithInFlightTTL overrides the in-flight response bound (default
// inFlightTTL).
func WithInFlightTTL(d time.Duration) Option {
	return func(g *Gateway) { g.flTTL = d }
}

// Authorizer decides whether a write may reach the inverter (C-008);
// a non-nil error refuses it before anything is queued or sent.
type Authorizer func(Command) error

// WithAuthorizer installs the C-008 write gate. Reads are never gated.
func WithAuthorizer(a Authorizer) Option {
	return func(g *Gateway) { g.authz = a }
}

// WithObserver installs the terminal-state observer.
func WithObserver(o ResultObserver) Option {
	return func(g *Gateway) { g.obs = o }
}
