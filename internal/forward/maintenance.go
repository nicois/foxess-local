package forward

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"foxesslocal/internal/policy"
	"foxesslocal/internal/store"
)

// Maintenance window: the installer-gated, time-boxed pass-through
// (2026-11-13 redesign — replaces the retired C-002 firmware hold).
// While active, the channel is a transparent bidirectional pass-through
// over whatever forward mode is configured: upstream default-allow (the
// host sets the policy's Window flag from Active) and downstream
// full-forward (the host maps Active to the mirror serve decision for
// new connections). The configured mode is untouched — the window
// overrides at the seams and the configured mode is what is in force on
// expiry or close (and after a restart: the window is process-local,
// never persisted).
//
// Every transition is audited (kind "maintenance") and emitted through
// the host sink (the host wires it to the bus — this package never
// imports internal/api, both seams are satisfied by structure).
const (
	// DefaultMaintenanceDuration applies when the open request names no
	// (or a zero/negative) duration.
	DefaultMaintenanceDuration = 30 * time.Minute
	// MaxMaintenanceDuration is the per-call cap: longer requests are
	// refused (the operator re-arms instead of one long window).
	MaxMaintenanceDuration = 4 * time.Hour
	// maintTickPeriod bounds the expiry-detection lag (the ticker is
	// runtime-virtualized under synctest — C-004; no wall-clock sleeps).
	maintTickPeriod = time.Second
	// audKindMaintenance is the audit kind for maintenance transitions.
	audKindMaintenance = "maintenance"
)

// The spec's SystemEvent.type vocabulary for maintenance transitions
// (internal/api re-declares the same strings as typed constants; the
// spec is the single source of truth for the wire values).
const (
	EventMaintenanceOpened   = "maintenance-opened"
	EventMaintenanceExtended = "maintenance-extended"
	EventMaintenanceExpired  = "maintenance-expired"
	EventMaintenanceClosed   = "maintenance-closed"
)

// ErrNotActive is returned by Close when no window is active (the API's
// 409).
var ErrNotActive = errors.New("maintenance: window not active")

// ErrDurationTooLong is returned by Open when the requested duration
// exceeds MaxMaintenanceDuration (the API's 400).
var ErrDurationTooLong = errors.New("maintenance: duration exceeds the " +
	MaxMaintenanceDuration.String() + " per-call cap")

// EmitFunc is the host-injected event sink: event is the spec's
// SystemEvent.type, detail the human-readable line.
type EmitFunc func(event, detail string)

// Maintenance is the window state machine. Open/Close are called from
// the API goroutine; Run (the expiry loop) mutates from its own
// goroutine — the mutex is the whole synchronization.
type Maintenance struct {
	st    *store.Store
	clock func() time.Time
	mode  func() policy.ForwardMode // the configured forward mode
	emit  EmitFunc

	mu      sync.Mutex
	active  bool
	opens   time.Time
	expires time.Time
	rearms  int
	base    policy.ForwardMode // the mode overridden at open (the restore report)
}

// NewMaintenance builds the machine. mode reads the configured forward
// mode (the host wires it to the forward-mode state); emit receives
// every transition event (nil disables emission — tests of the pure
// transitions pass nil and assert the audit rows instead).
func NewMaintenance(st *store.Store, clock func() time.Time, mode func() policy.ForwardMode, emit EmitFunc) *Maintenance {
	if emit == nil {
		emit = func(string, string) {}
	}
	return &Maintenance{st: st, clock: clock, mode: mode, emit: emit}
}

// View is one snapshot of the window (the API's GET body source).
// Remaining is 0 when inactive.
type View struct {
	Active    bool
	Opens     time.Time
	Expires   time.Time
	Rearms    int
	Base      policy.ForwardMode
	Remaining time.Duration
}

// View returns the current window state.
func (m *Maintenance) View() View {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.active {
		return View{}
	}
	return View{
		Active:    true,
		Opens:     m.opens,
		Expires:   m.expires,
		Rearms:    m.rearms,
		Base:      m.base,
		Remaining: m.expires.Sub(m.clock()),
	}
}

// Active reports whether the window is currently open.
func (m *Maintenance) Active() bool { return m.View().Active }

// Open opens the window, or — when one is already active — resets its
// deadline (re-arm; the original open time is kept, the rearm counted).
// A zero/negative duration uses DefaultMaintenanceDuration; a duration
// above MaxMaintenanceDuration is refused (no state change). It
// returns the new deadline.
func (m *Maintenance) Open(d time.Duration, acting string) (time.Time, error) {
	if d <= 0 {
		d = DefaultMaintenanceDuration
	}
	if d > MaxMaintenanceDuration {
		return time.Time{}, ErrDurationTooLong
	}
	now := m.clock()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active {
		m.expires = now.Add(d)
		m.rearms++
		m.auditLocked(fmt.Sprintf("re-armed (%s) by %s — now expires %s", d, acting, m.expires.Format(time.RFC3339)))
		m.emitLocked(EventMaintenanceExtended,
			fmt.Sprintf("re-armed by %s (now expires %s)", acting, m.expires.Format(time.RFC3339)))
		return m.expires, nil
	}
	m.active = true
	m.opens = now
	m.expires = now.Add(d)
	m.rearms = 0
	m.base = m.mode()
	m.auditLocked(fmt.Sprintf("opened (%s) by %s — mode %s overridden until %s", d, acting, m.base, m.expires.Format(time.RFC3339)))
	m.emitLocked(EventMaintenanceOpened,
		fmt.Sprintf("opened %s by %s (mode %s → pass-through until %s)", d, acting, m.base, m.expires.Format(time.RFC3339)))
	return m.expires, nil
}

// Close closes an active window immediately (the configured mode is
// back in force). ErrNotActive when no window is active.
func (m *Maintenance) Close(acting string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.active {
		return ErrNotActive
	}
	m.active = false
	m.auditLocked(fmt.Sprintf("closed by %s — mode %s restored", acting, m.base))
	m.emitLocked(EventMaintenanceClosed,
		fmt.Sprintf("closed by %s (mode %s restored)", acting, m.base))
	return nil
}

// expireLocked expires the window at its deadline (Run's tick). The
// caller holds m.mu.
func (m *Maintenance) expireLocked() {
	if !m.active || m.clock().Before(m.expires) {
		return
	}
	m.active = false
	m.auditLocked(fmt.Sprintf("expired — mode %s restored", m.base))
	m.emitLocked(EventMaintenanceExpired,
		fmt.Sprintf("expired (mode %s restored)", m.base))
}

// Run drives the expiry loop until ctx is done: an immediate check,
// then one every maintTickPeriod (the ticker is runtime-virtualized
// under synctest — C-004; no wall-clock sleeps). Open/Close from the
// API race the tick only through the mutex.
func (m *Maintenance) Run(ctx context.Context) {
	tick := time.NewTicker(maintTickPeriod)
	defer tick.Stop()
	m.mu.Lock()
	m.expireLocked()
	m.mu.Unlock()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			m.mu.Lock()
			m.expireLocked()
			m.mu.Unlock()
		}
	}
}

func (m *Maintenance) auditLocked(detail string) {
	m.st.RecordAudit(m.clock(), audKindMaintenance, detail)
}

func (m *Maintenance) emitLocked(event, detail string) {
	m.emit(event, detail)
}
