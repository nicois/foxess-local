// Package forward owns the forward-mode state machine and the recurring
// force-charge window scheduler.
//
// The machine answers two interfaces: the REST API's ForwardModeState
// (Get/Set, structurally satisfied — the API republishes the
// forward-mode-changed bus event itself) and framesvc's ServePolicy
// (Decide, consulted exactly once per connection at establishment). It
// never imports internal/api: both seams are satisfied by structure.
//
// The scheduler arms and disarms one or more force-charge windows in
// the inverter's 48010 time-slot table. Every write is a single
// gateway.Submit(KindSchedule) — the sole validated upstream→inverter
// path (C-003) — and every attempted push leaves an audit row.
package forward

import (
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nicois/prox/pkg/hooks"

	"foxesslocal/internal/framesvc"
	"foxesslocal/internal/policy"
	"foxesslocal/internal/store"
)

// audKindForwardMode is the audit kind for forward-mode changes (the
// exact kind the former api.ConfigForwardMode used).
const audKindForwardMode = "forward-mode"

// State is the forward-mode state machine (plan 2 task 9).
//
// Set applies atomically and records one audit row per successful
// change in the exact shape the former api.ConfigForwardMode wrote
// (kind "forward-mode", detail "→ <mode> by <acting>"); the audit rows
// are the log surface. Set never publishes bus events — the API's
// handler does that for the REST surface.
type State struct {
	st       *store.Store
	clock    func() time.Time
	atom     atomic.Pointer[held]
	onChange atomic.Pointer[func(policy.ForwardMode, string) error]
}

// OnChange installs the hook Set calls after a valid change (the host
// persists the mode with it, so a restart keeps it — F12). A hook error
// is returned by Set; the change itself has applied.
func (s *State) OnChange(f func(mode policy.ForwardMode, acting string) error) {
	s.onChange.Store(&f)
}

type held struct {
	mode policy.ForwardMode
	ts   time.Time
}

// New builds the machine; an empty initial mode defaults to
// telemetry_only (the cutover-safe default: the cloud sees everything,
// writes nothing back).
// Set/Get satisfy api.ForwardModeState and Decide satisfies
// framesvc.ServePolicy structurally.
func New(st *store.Store, clock func() time.Time, initial policy.ForwardMode) *State {
	if initial == "" {
		initial = policy.ModeTelemetryOnly
	}
	s := &State{st: st, clock: clock}
	s.atom.Store(&held{mode: initial, ts: clock()})
	return s
}

// Get implements api.ForwardModeState.
func (s *State) Get() (policy.ForwardMode, time.Time, error) {
	h := s.atom.Load()
	return h.mode, h.ts, nil
}

// Set implements api.ForwardModeState: an unknown mode is refused
// (error, no state change, no audit row); a valid mode applies
// atomically and records one audit row.
func (s *State) Set(mode policy.ForwardMode, acting string) error {
	if !validMode(mode) {
		return fmt.Errorf("forward mode: unknown mode %q", mode)
	}
	s.atom.Store(&held{mode: mode, ts: s.clock()})
	s.st.RecordAudit(s.clock(), audKindForwardMode, fmt.Sprintf("→ %s by %s", mode, acting))
	if f := s.onChange.Load(); f != nil {
		if err := (*f)(mode, acting); err != nil {
			return fmt.Errorf("forward mode %s applied but not saved: %w", mode, err)
		}
	}
	return nil
}

// Decide implements framesvc.ServePolicy: the serve decision for one
// new connection. off → ServeLocal with a fresh in-memory leg (the relay
// serves through RelayLeg and the consumer drains c2s from Drain);
// full / telemetry_only → ServeMirror, no leg. A mode that is not one of
// the three is a defect in the host's construction (Set refuses them);
// the error lets framesvc close the flow with it as the reason.
func (s *State) Decide(c *hooks.Conn) (framesvc.ServeMode, framesvc.ServeLeg, string, error) {
	switch s.atom.Load().mode {
	case policy.ModeOff:
		_ = c
		return framesvc.ServeLocal, &localLeg{l: newLeg()}, "forward-mode off: local", nil
	case policy.ModeTelemetryOnly:
		return framesvc.ServeMirror, nil, "forward-mode telemetry_only: mirror", nil
	case policy.ModeFull:
		return framesvc.ServeMirror, nil, "forward-mode full: mirror", nil
	default:
		return 0, nil, "", fmt.Errorf("forward: undecided mode %q", s.atom.Load().mode)
	}
}

func validMode(m policy.ForwardMode) bool {
	switch m {
	case policy.ModeFull, policy.ModeTelemetryOnly, policy.ModeOff:
		return true
	}
	return false
}

// leg is the in-memory server leg for one ServeLocal connection: two
// io.Pipe pairs wired to prox's ServeLocal contract
// (hooks.ServeDecision.Local): the relay side Write receives c2s (the
// relay writes the client→upstream bytes; framesvc's drain goroutine
// consumes them) and Read provides s2c. No s2c is ever written through
// the leg — hook injection (hc.WriteS2C) bypasses the leg and writes
// straight into the relay — so the s2c pipe exists only for the relay
// to idle on instead of EOF-ing the client session.
type leg struct {
	once sync.Once
	c2sW *io.PipeWriter
	c2sR *io.PipeReader
	s2cW *io.PipeWriter
	s2cR *io.PipeReader
}

func newLeg() *leg {
	c2sR, c2sW := io.Pipe()
	s2cR, s2cW := io.Pipe()
	return &leg{c2sW: c2sW, c2sR: c2sR, s2cW: s2cW, s2cR: s2cR}
}

// close tears the leg down exactly once: the writers first (so the
// drain reader on the c2s pipe sees EOF), then the readers (further
// operations fail). Idempotent — the relay closes the relay leg and
// framesvc closes the drain leg as the connection ends, both mapping to
// this one (the close-with-connection dependency the design relies on).
func (l *leg) close() error {
	l.once.Do(func() {
		_ = l.c2sW.Close()
		_ = l.s2cW.Close()
		_ = l.c2sR.Close()
		_ = l.s2cR.Close()
	})
	return nil
}

// localLeg implements framesvc.ServeLeg on one leg.
type localLeg struct{ l *leg }

// RelayLeg implements framesvc.ServeLeg: the view the ladder serves
// through (prox contract: Write = c2s, Read = s2c).
func (x *localLeg) RelayLeg() io.ReadWriteCloser { return relayLeg{x.l} }

// Drain implements framesvc.ServeLeg: the c2s reader framesvc consumes
// in a goroutine.
func (x *localLeg) Drain() io.ReadCloser { return drainLeg{x.l} }

// relayLeg is the leg side prox's localConn adapts to the relay's
// upstream leg.
type relayLeg struct{ l *leg }

func (r relayLeg) Read(p []byte) (int, error)  { return r.l.s2cR.Read(p) }
func (r relayLeg) Write(p []byte) (int, error) { return r.l.c2sW.Write(p) }
func (r relayLeg) Close() error                { return r.l.close() }

// drainLeg is framesvc's view of the leg: it reads the c2s bytes the
// relay writes.
type drainLeg struct{ l *leg }

func (d drainLeg) Read(p []byte) (int, error) { return d.l.c2sR.Read(p) }
func (d drainLeg) Close() error               { return d.l.close() }
