// Package framesvc is the one prox hook that ties the frame codec, the
// policy engine, the store, and the gateway to the prox relay: per-flow
// per-direction reassembly, decode, one policy Decide per s2c frame,
// verbatim chunk reconstruction, audit, bootstrap-trio generation,
// gateway injection, and the Phase-2 serve-policy seam.
//
// It registers exactly one hook on the hooks.Pipeline (Name "framesvc",
// filter DPort 14431); everything flows through that single surface.
// C-001/C-003 are enforced here by construction: the hook's OnData is
// the only place s2c bytes reach the client leg (default-deny via the
// engine — full or an active maintenance window pass, everything else
// drops), and operator commands enter only through the gateway, whose
// sole upstream path is the hook's InjectS2C.
package framesvc

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nicois/prox/pkg/capture"
	"github.com/nicois/prox/pkg/hooks"

	"foxesslocal/internal/frame"
	"foxesslocal/internal/gateway"
	"foxesslocal/internal/policy"
	"foxesslocal/internal/store"
)

// hookName is the hook's pipeline name; capture chunks it injects are
// recorded with this origin (per-injection identity — bootstrap /
// gateway / hold-release — lives in the store's frame rows instead).
const hookName = "framesvc"

// cloudPort is the inverter's cloud-channel port (OQ-1).
const cloudPort = 14431

// frameHeaderLen is the raw-frame header before the payload: start(2) +
// device(4) + fn(1) + plen(2); the frame's total length is
// frameHeaderLen + plen + crc(2) + term(2).
const frameHeaderLen = 9

// frameTailLen is the CRC(2) + terminator(2) after the payload.
const frameTailLen = 4

// plenHi/plenLo index the 2-byte big-endian payload length in a raw
// frame (raw[7:9], after start(2)+device(4)+fn(1)).
const (
	plenHi = 7
	plenLo = 8
)

// byteMask / byteShift split a 16-bit value into wire bytes (gosec G115).
const (
	byteMask  = 0xff
	byteShift = 8
)

// trioDev0 is the first device byte of each of the three c2s
// registration frames, in request order (profile §3). Only the first
// (2a) gates the local trio: the inverter sends no 2c/2b until a
// bootstrap response is delivered (profile §3, 2026-11-08 drop
// experiment).
var trioDev0 = [3]byte{0x2a, 0x2c, 0x2b}

// trioDelay is the lag between the inverter's 2a registration and the
// first bootstrap write: the real cloud's observed processing latency
// (2.5 s on the 2026-11-15 live capture, 5–8 s in the 2026-11-11
// corpus). A trio delivered on the same tick as the 2a is not picked
// up by the inverter — 32 h of 12 s re-registration loops against a
// +0 ms local trio (2026-11-15 live).
const trioDelay = 4 * time.Second

// trioRespGap spaces the three bootstrap writes: the cloud sends them
// as separate segments (255–327 ms apart in the captures).
const trioRespGap = 300 * time.Millisecond

// triggerReadGap1 spaces the trio's last write from the first trigger
// read, and triggerReadGap2 spans the two reads — the 11-15 corpus
// meta (session 1794729390376-1, t0-relative): trio frames +4.20 /
// +4.45 / +4.71 s, read1 +5.89 s (trio3 +1.18 s), read2 +10.98 s
// (read1 +5.09 s).
const (
	triggerReadGap1 = 1200 * time.Millisecond
	triggerReadGap2 = 5100 * time.Millisecond
)

// PostureSource yields the current posture for each Decide — the task-10
// config flip is a plain swap of this source; the hook re-consults it on
// every chunk.
type PostureSource func() policy.Posture

// ServeMode is the per-connection serving decision (Phase-2 seam).
type ServeMode int

const (
	// ServeMirror forwards to the real cloud (Phase-1 behavior).
	ServeMirror ServeMode = iota
	// ServeLocal serves the inverter locally without forwarding.
	ServeLocal
	// ServeReject rejects the connection.
	ServeReject
)

// ServeLeg is the local leg in the two views each side needs: RelayLeg
// is what the ladder serves through (prox's ServeLocal contract:
// Write receives c2s, Read provides s2c) and Drain is the c2s reader
// the service consumes so the relay's c2s copy never blocks (closing
// either view tears the whole leg down).
type ServeLeg interface {
	RelayLeg() io.ReadWriteCloser
	Drain() io.ReadCloser
}

// ServePolicy decides, per connection, how to serve it. Phase 1 always
// mirrors; the seam exists so Phase 2 can swap in local-serving without
// touching the hook's relay path.
type ServePolicy interface {
	// Decide returns the serving mode and — when the mode is ServeLocal —
	// the in-memory server leg: the relay serves through RelayLeg and the
	// service drains c2s from Drain in a goroutine (the leg is closed
	// with the connection).
	Decide(c *hooks.Conn) (mode ServeMode, leg ServeLeg, reason string, err error)
}

// String implements fmt.Stringer (audit lines).
func (m ServeMode) String() string {
	switch m {
	case ServeMirror:
		return "mirror"
	case ServeLocal:
		return "local"
	case ServeReject:
		return "reject"
	}
	return "unknown"
}

// MirrorServe is the Phase-1 ServePolicy: always forward.
type MirrorServe struct{}

// Decide implements ServePolicy.
func (MirrorServe) Decide(*hooks.Conn) (ServeMode, ServeLeg, string, error) {
	return ServeMirror, nil, "phase1: always mirror", nil
}

// Service is the frame hook: it implements hooks.Hook, hooks.TLSObserver,
// and gateway.Injector on one surface.
type Service struct {
	// capture, when set, sees every chunk as it arrived (maintenance-
	// window capture: WithCapture).
	capture func(connID string, s2c bool, data []byte)

	pipeline *hooks.Pipeline
	st       *store.Store
	gw       *gateway.Gateway
	posture  PostureSource
	serve    ServePolicy
	clock    func() time.Time
	obs      Observer

	mu     sync.Mutex
	flows  map[string]*flow
	active string // connID currently registered with the gateway

	// tasks counts the service's own goroutines (the bootstrap emitters,
	// the flow activation); quiesced stops new ones. Each flow carries
	// its own quit signal (made with the flow: under synctest a channel
	// made outside the bubble would never count as durably blocked).
	tasks    sync.WaitGroup
	quiesced bool
}

// goTask runs f on a tracked goroutine (Quiesce waits for it); after
// Quiesce nothing new starts.
func (s *Service) goTask(f func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.quiesced {
		return
	}
	s.tasks.Go(func() {
		f()
	})
}

// Quiesce stops the service's own goroutines — a pending bootstrap trio
// or trigger read ends without writing — and waits for them. The host
// calls it before closing the store: a registration within trioDelay of
// shutdown otherwise wrote an audit row after Close (DEF-008).
func (s *Service) Quiesce() {
	s.mu.Lock()
	s.quiesced = true
	for _, fl := range s.flows {
		fl.stop()
	}
	s.mu.Unlock()
	s.tasks.Wait()
}

// Option configures the service.
type Option func(*Service)

// WithCapture hands every chunk, as it arrived and before reassembly or
// policy, to f (s2c: cloud → inverter). The maintenance-window capture
// records them while a window asks it to; f must be cheap and never block.
func WithCapture(f func(connID string, s2c bool, data []byte)) Option {
	return func(s *Service) { s.capture = f }
}

// WithServePolicy overrides the serve policy (default MirrorServe).
func WithServePolicy(p ServePolicy) Option {
	return func(s *Service) { s.serve = p }
}

// New builds the service and registers it on the pipeline (the hook is
// the single surface; the host owns the pipeline).
func New(
	pipeline *hooks.Pipeline,
	st *store.Store,
	gw *gateway.Gateway,
	posture PostureSource,
	clock func() time.Time,
	opts ...Option,
) *Service {
	s := &Service{
		pipeline: pipeline,
		st:       st,
		gw:       gw,
		posture:  posture,
		serve:    MirrorServe{},
		clock:    clock,
		flows:    make(map[string]*flow),
	}
	for _, o := range opts {
		o(s)
	}
	pipeline.Register(s)
	return s
}

// flow is the per-connection state: the per-direction reassemblers, the
// pending bootstrap-trio flag, the session device tail observed from the
// first s2c frame, and (ServeLocal) the in-memory server leg.
type flow struct {
	mu          sync.Mutex
	hc          *hooks.HookConn
	leg         ServeLeg
	c2s         *reasm
	s2c         *reasm
	trioPending bool
	dev         [4]byte
	devSet      bool
	envFn       byte
	fnSet       bool
	seq         atomic.Int64
	// firstKeepalive marks the session established (the poller's
	// pollSettle base).
	firstKeepalive time.Time
	// quit ends this flow's emitters (Quiesce); closed once
	quit     chan struct{}
	quitOnce sync.Once
	// reqs are the requests awaiting the inverter's answer, by asker
	// (route.go); up holds the c2s bytes not yet sent to the cloud (from
	// stream offset upOff): a mirrored flow's c2s is rebuilt frame by
	// frame so answers to this app's requests stay local.
	reqs  requests
	up    []byte
	upOff int64
	// closeReason, once set (Reconnect), ends the connection at its next
	// chunk in either direction
	closeReason atomic.Pointer[string]
}

// ---------- hooks.Hook ----------

// Name implements hooks.Hook.
func (s *Service) Name() string { return hookName }

// Filter implements hooks.Hook: only the cloud-channel port.
func (s *Service) Filter() hooks.Match {
	return hooks.Match{DPort: cloudPort}
}

// OnOpen implements hooks.Hook: it audits the flow, applies the serve
// policy, and registers the connection with the gateway once the relay
// has dialed (a SetFlow before Ready would reject queued commands).
func (s *Service) OnOpen(c *hooks.Conn, hc *hooks.HookConn) hooks.Result {
	now := s.clock()
	s.st.RecordAudit(now, "flow-open", fmt.Sprintf("%s %s", c.ID, c.Dst))
	mode, leg, reason, err := s.serve.Decide(c)
	if err != nil {
		s.st.RecordAudit(now, "serve", c.ID+": "+err.Error())
		return hooks.Result{Close: true, Reason: err.Error()}
	}
	s.st.RecordAudit(now, "serve", fmt.Sprintf("%s %s: %s", c.ID, mode, reason))
	fl := &flow{hc: hc, c2s: newReasm(), s2c: newReasm(), leg: leg, quit: make(chan struct{})}
	s.mu.Lock()
	s.flows[c.ID] = fl
	if s.quiesced {
		fl.stop()
	}
	s.mu.Unlock()
	if mode == ServeLocal && leg != nil {
		go func() { _, _ = io.Copy(io.Discard, leg.Drain()) }()
	}
	if mode == ServeReject {
		return hooks.Result{Close: true, Reason: reason}
	}
	s.observe(Event{Kind: EventSessionUp, ConnID: c.ID})
	s.goTask(func() { s.activateFlow(c.ID, hc, fl.quit) })
	return hooks.Result{}
}

// Serve implements hooks.Serving: the ladder's establishment decision is
// the forward-mode decision OnOpen already made (D-011: off serves
// locally with no cloud leg; full / telemetry_only mirror). Without it
// the relay defaults to ServeMirror and dials the real cloud even in off
// mode, mirroring the uplink. OnOpen registered the flow (and its leg,
// when the decision is local) before Serve is consulted, so Serve just
// relays that decision — the same leg instance the drain goroutine runs
// on.
func (s *Service) Serve(c *hooks.Conn) (hooks.ServeDecision, error) {
	s.mu.Lock()
	fl := s.flows[c.ID]
	s.mu.Unlock()
	if fl == nil {
		return hooks.ServeDecision{Mode: hooks.ServeReject, Reason: "framesvc: no flow"}, nil
	}
	if fl.leg != nil {
		return hooks.ServeDecision{Mode: hooks.ServeLocal, Local: fl.leg.RelayLeg(), Reason: "forward-mode off: local"}, nil
	}
	return hooks.ServeDecision{Mode: hooks.ServeMirror}, nil
}

// activateFlow registers the flow with the gateway once the relay's legs
// are established (hooks.ErrNotReady guards queued commands until then).
func (s *Service) activateFlow(connID string, hc *hooks.HookConn, quit <-chan struct{}) {
	select {
	case <-hc.Ready():
	case <-hc.Done():
		return
	case <-quit:
		return
	}
	s.mu.Lock()
	s.active = connID
	s.mu.Unlock()
	s.gw.SetFlow(connID)
}

// OnData implements hooks.Hook. c2s: reassemble, decode, audit, ack the
// gateway, and (once) emit the local bootstrap trio when the posture
// will not relay the real cloud's responses (profile §3). s2c:
// reassemble, one Decide per frame, rebuild the chunk
// from exactly the passed frames.
func (s *Service) OnData(c *hooks.Conn, dir hooks.Dir, data []byte) hooks.Result {
	fl := s.flow(c.ID)
	if fl == nil {
		return hooks.Result{}
	}
	if r := fl.closeReason.Load(); r != nil {
		return hooks.Result{Close: true, Reason: *r}
	}
	if s.capture != nil {
		s.capture(c.ID, dir == hooks.DirS2C, data)
	}
	if dir == hooks.DirS2C {
		return s.onDataS2C(c.ID, fl, data)
	}
	return s.onDataC2S(c.ID, fl, data)
}

// Reconnect ends every open session at its next chunk (the inverter
// sends at least a keepalive every 30 s and answers our polls every 5 s,
// so within seconds), so the inverter reconnects at once — e.g. under a
// new forward mode: the serve decision is made per connection (R3).
// Audited per session.
func (s *Service) Reconnect(reason string) {
	s.mu.Lock()
	flows := make(map[string]*flow, len(s.flows))
	maps.Copy(flows, s.flows)
	s.mu.Unlock()
	for id, fl := range flows {
		r := reason
		if fl.closeReason.CompareAndSwap(nil, &r) {
			s.st.RecordAudit(s.clock(), "reconnect", id+": "+reason)
		}
	}
}

// OnClose implements hooks.Hook: it audits the close, records the
// reassembly residue, closes any ServeLocal leg, and unregisters the
// flow from the gateway.
func (s *Service) OnClose(c *hooks.Conn, reason string) {
	s.st.RecordAudit(s.clock(), "flow-close", c.ID+": "+reason)
	s.observe(Event{Kind: EventSessionDown, ConnID: c.ID, Reason: reason})
	fl := s.flow(c.ID)
	s.mu.Lock()
	delete(s.flows, c.ID)
	wasActive := s.active == c.ID
	if wasActive {
		s.active = ""
	}
	s.mu.Unlock()
	if fl != nil {
		fl.mu.Lock()
		resC2S, offC2S := fl.c2s.Residue()
		resS2C, offS2C := fl.s2c.Residue()
		s.recordResidue(c.ID, fl, capture.DirC2S, offC2S, resC2S)
		s.recordResidue(c.ID, fl, capture.DirS2C, offS2C, resS2C)
		leg := fl.leg
		fl.mu.Unlock()
		if leg != nil {
			_ = leg.Drain().Close()
		}
	}
	if wasActive {
		s.gw.SetFlow("")
	}
}

// flow looks up the flow state (nil when unknown or already closed).
func (s *Service) flow(connID string) *flow {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flows[connID]
}

// ---------- hooks.TLSObserver (the inverter trusts any CA — OQ-2) ----------

// OnTLSResult implements hooks.TLSObserver: the client-leg TLS outcome is
// audited.
func (s *Service) OnTLSResult(c *hooks.Conn, r hooks.TLSResult) {
	s.st.RecordAudit(s.clock(), "tls", fmt.Sprintf("%s %s %s", c.ID, r.Host, r.Result))
	s.observe(Event{Kind: EventTLS, ConnID: c.ID, Host: r.Host, Result: r.Result})
}

// ---------- c2s: telemetry up, acks down ----------

// onDataC2S reassembles c2s frames, audits each (with a telemetry row
// for 150 B frames), pins the session identity for local sessions, acks
// the gateway on echo frames, and schedules the bootstrap trio on
// every 2a registration (C-003: this is the sole
// upstream path, alongside the gateway).
func (s *Service) onDataC2S(connID string, fl *flow, data []byte) hooks.Result {
	now := s.clock()
	frames, askers, settled, mirrored := s.feedC2S(fl, data, now)
	s.recordC2S(connID, fl, frames, askers, now)
	s.pinLocalSession(fl, frames)
	s.ackInFlight(connID, frames, askers)
	s.maybeEmitTrio(connID, fl, frames)
	if !mirrored {
		return hooks.Result{}
	}
	return hooks.Result{Data: s.upstreamC2S(fl, frames, askers, settled)}
}

// feedC2S reassembles one c2s chunk and routes each answer to whoever
// asked (in stream order, before any ack); a mirrored flow also keeps
// the chunk for the cloud-bound rebuild.
func (s *Service) feedC2S(fl *flow, data []byte, now time.Time) ([]frame.Frame, []asker, int64, bool) {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	frames := fl.c2s.Feed(data)
	mirrored := fl.leg == nil
	if mirrored {
		fl.up = append(fl.up, data...)
	}
	askers := make([]asker, len(frames))
	for i := range frames {
		if kind, _ := frame.Decode(frames[i]); isAnswer(kind) {
			askers[i] = fl.reqs.route(frames[i], now)
		}
	}
	return frames, askers, fl.c2s.Settled(), mirrored
}

// keptLocalReason marks an answer to this app's own request: it acks the
// gateway and, in a mirrored flow, is not sent to the cloud.
const keptLocalReason = "answer to this app's request: kept from the cloud"

// recordC2S audits each c2s frame (with a telemetry event for 150 B
// frames) and marks the session established on its first keepalive.
func (s *Service) recordC2S(connID string, fl *flow, frames []frame.Frame, askers []asker, now time.Time) {
	for i := range frames {
		kind, _ := frame.Decode(frames[i])
		reason := ""
		if askers[i] == askerLocal {
			reason = keptLocalReason
		}
		s.st.RecordFrame(s.frameRecord(connID, fl, capture.DirC2S, now, frames[i], kind, store.DecisionForwarded, reason, ""))
		if kind == frame.KindKeepalive {
			fl.mu.Lock()
			if fl.firstKeepalive.IsZero() {
				fl.firstKeepalive = now
			}
			fl.mu.Unlock()
		}
		// a 150 B tele frame (the cloud's model, no EPS field) yields to
		// the register poll while it supplies telemetry (store.PollFresh)
		if kind == frame.KindTele && !s.st.PollFresh(now) {
			if t, err := frame.ParseTele(frames[i].Payload); err == nil {
				s.observe(Event{Kind: EventTelemetry, ConnID: connID, Tele: &t})
			}
		}
	}
}

// isAnswer reports whether a c2s frame is the inverter's answer to a
// Modbus request (read response or write echo).
func isAnswer(k frame.Kind) bool {
	return k == frame.KindCfg91 || k == frame.KindMbecho || k == frame.KindMbreadResp
}

// upstreamC2S is what goes to the cloud for this chunk: the c2s stream
// bytes up to settled (everything but a possible partial frame),
// verbatim, minus the answers to this app's own requests (the cloud did
// not ask; owner 2026-11-17). A possible partial frame is held for the
// next chunk; bytes that cannot start a frame pass at once, as before.
func (s *Service) upstreamC2S(fl *flow, frames []frame.Frame, askers []asker, settled int64) []byte {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	cut := settled
	if limit := fl.upOff + int64(len(fl.up)); cut > limit {
		cut = limit
	}
	if cut < fl.upOff {
		cut = fl.upOff
	}
	out := make([]byte, 0, cut-fl.upOff)
	pos := fl.upOff
	var end int64 // the end of the last top-level frame (a wrapper embeds one)
	for i := range frames {
		f := frames[i]
		top := f.Off >= end
		if top {
			end = f.Off + int64(len(f.Raw))
		}
		if !top || askers[i] != askerLocal {
			continue
		}
		out = append(out, fl.up[pos-fl.upOff:f.Off-fl.upOff]...)
		pos = f.Off + int64(len(f.Raw))
	}
	out = append(out, fl.up[pos-fl.upOff:cut-fl.upOff]...)
	fl.up = append([]byte(nil), fl.up[cut-fl.upOff:]...)
	fl.upOff = cut
	return out
}

// pinLocalSession pins the session identity for a locally-served flow
// from the inverter's own session-pattern c2s frame (device
// [x, 0x6a, S, ctr]): a local session has no upstream s2c to observe
// (observeSession runs only on s2c), and the gateway stamps injections
// with the session's tail — the inverter's own S byte, which the real
// cloud mirrors in its s2c frames (profile §3/§4/§5).
func (s *Service) pinLocalSession(fl *flow, frames []frame.Frame) {
	if fl.leg == nil {
		return // mirror flow: the identity comes from the upstream s2c
	}
	fl.mu.Lock()
	defer fl.mu.Unlock()
	if fl.devSet {
		return
	}
	for i := range frames {
		if frames[i].Device[1] != frame.SessionDev1 {
			continue
		}
		fl.dev = [4]byte{0x11, frames[i].Device[1], frames[i].Device[2], frames[i].Device[3]}
		fl.devSet = true
		return
	}
}

// ackInFlight matches c2s echo frames against the gateway's in-flight
// command (the ack protocol, task 5). Only answers to this app's own
// requests ack (route.go: a relayed cloud request's answer, or an
// unsolicited frame, never does). Every such echo in the chunk is acked
// in stream order: the reassembler returns every complete frame in a
// TCP segment and the inverter coalesces its responses (a schedule push
// answers 13 reads back-to-back; acking only the first wedges the
// in-flight slot). A failed Ack ends the command (the gateway moves it
// to a terminal state), so the loop stops there.
func (s *Service) ackInFlight(connID string, frames []frame.Frame, askers []asker) {
	for i := range frames {
		if askers[i] != askerLocal {
			continue
		}
		cmdID := s.inflightCmdID()
		if cmdID == "" {
			s.st.RecordAudit(s.clock(), "ack-error", connID+": echo with no in-flight command")
			return
		}
		if err := s.gw.Ack(cmdID, frames[i]); err != nil {
			s.st.RecordAudit(s.clock(), "ack-error", connID+": "+err.Error())
			break
		}
	}
}

// inflightCmdID returns the gateway's first in-flight command ID ("").
func (s *Service) inflightCmdID() string {
	for _, p := range s.gw.Pending() {
		if p.Status == gateway.StatusInFlight {
			return p.ID
		}
	}
	return ""
}

// maybeEmitTrio schedules the locally generated bootstrap trio for a
// 2a registration, but only when the posture will not relay the real
// cloud's responses (profile §3: the inverter sends no 2c/2b until a
// response is delivered — waiting for the gated frames would deadlock
// the registration; in telemetry_only / off the real responses are
// denied or nonexistent, so the local trio plus the two initial
// trigger reads is the only bootstrap the inverter ever sees). In
// full / window the real cloud owns the
// bootstrap (its responses are relayed) — no local injection, or the
// inverter would see a duplicated triple.
//
// The emission mirrors the real cloud's algorithm (2026-11-15 live
// capture): the trio lands ~trioDelay after the 2a, as three separate
// writes trioRespGap apart, and a fresh trio is scheduled for every 2a
// the inverter sends — the cloud re-emits on the inverter's 12 s
// re-registration cadence (trio fn stepping +12 per retransmission,
// 2026-11-08 captures), and the re-requests stop once the inverter is
// established.
func (s *Service) maybeEmitTrio(connID string, fl *flow, frames []frame.Frame) {
	// Posture read outside fl.mu (lock ordering).
	relays := s.postureRelaysS2C()
	fl.mu.Lock()
	defer fl.mu.Unlock()
	for i := range frames {
		if relays || fl.trioPending {
			return
		}
		if frames[i].Device[0] != trioDev0[0] {
			continue
		}
		fl.trioPending = true
		s.goTask(func() { s.emitTrio(connID, fl) })
		return
	}
}

// postureRelaysS2C reports whether the current posture relays the
// real cloud's s2c frames (full mode, or an active maintenance window
// over any mode) — the bootstrap responses included.
func (s *Service) postureRelaysS2C() bool {
	p := s.posture()
	return p.Window || p.Mode == policy.ModeFull
}

// emitTrio is the delayed, segmented bootstrap emission — it runs in
// its own goroutine (the c2s read loop must not block on the cloud's
// latency), derives S/q from the service clock at emission (profile
// §3, time-derived), and writes each response as its own chunk,
// recorded as injected / bootstrap. It ends by emitting the two
// initial trigger reads (emitTriggerReads) at the cloud's offsets
// from the 2a.
// stop closes the flow's quit signal (once).
func (fl *flow) stop() {
	fl.quitOnce.Do(func() {
		if fl.quit != nil {
			close(fl.quit)
		}
	})
}

// waitGap sleeps for d, or returns false when the flow closes first:
// the emitters must not park across a flow close — a blocked emitter
// fails the synctest group, and the late write it would attempt
// audits a spurious inject-error onto a closed flow.
func (s *Service) waitGap(fl *flow, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-fl.hc.Done():
		return false
	case <-fl.quit: // shutting down (Quiesce)
		return false
	}
}

func (s *Service) emitTrio(connID string, fl *flow) {
	if !s.waitGap(fl, trioDelay) {
		return
	}
	trio := frame.BootstrapTrio(trioDev0, frame.TrioParamsAt(s.clock()))
	fl.mu.Lock()
	for i := range trio {
		if i > 0 {
			fl.mu.Unlock()
			if !s.waitGap(fl, trioRespGap) {
				return
			}
			fl.mu.Lock()
		}
		if err := s.writeS2C(fl, trio[i]); err != nil {
			fl.trioPending = false
			fl.mu.Unlock()
			s.st.RecordAudit(s.clock(), "inject-error", connID+": bootstrap trio: "+err.Error())
			return
		}
		s.recordInjected(connID, fl, s.clock(), trio[i], "bootstrap")
	}
	fl.trioPending = false
	fl.mu.Unlock()
	s.emitTriggerReads(connID, fl)
}

// emitTriggerReads is the delayed injection of the two initial cloud
// reads (frame.TriggerReads) that follows the bootstrap trio: the
// inverter arms its push mode (the fn=64 wrapper stream) only after
// receiving them (cutover doc §3.6). It runs in the trio's goroutine,
// so the reads land at the cloud's offsets from the 2a (+5.8 s /
// +10.9 s) and are recorded as injected / trigread — the same class as
// the trio (local, no upstream bytes: C-001/C-002; fixed validated
// reads, not operator input: C-003).
func (s *Service) emitTriggerReads(connID string, fl *flow) {
	if !s.waitGap(fl, triggerReadGap1) {
		return
	}
	for i := range frame.TriggerReads {
		if i > 0 {
			if !s.waitGap(fl, triggerReadGap2) {
				return
			}
		}
		s.trackLocal(fl, frame.TriggerReads[i])
		if err := s.writeS2C(fl, frame.TriggerReads[i]); err != nil {
			s.st.RecordAudit(s.clock(), "inject-error", connID+": trigger read: "+err.Error())
			return
		}
		s.recordInjected(connID, fl, s.clock(), frame.TriggerReads[i], "trigread")
	}
}

// ---------- s2c: the policy gate (C-001) ----------

// onDataS2C reassembles s2c frames, runs one Decide per frame, and
// rebuilds the chunk from exactly the passed frames — the relay copies
// the returned slice verbatim, so this is the gate. Delivery is
// frame-atomic: a chunk that yields no complete frame is dropped
// (its bytes are held in the reassembler until the frame completes),
// so a denied frame can never leak a prefix to the inverter and the
// client stream stays byte-identical to the cloud's frame sequence.
func (s *Service) onDataS2C(connID string, fl *flow, data []byte) hooks.Result {
	fl.mu.Lock()
	frames := fl.s2c.Feed(data)
	fl.mu.Unlock()
	if len(frames) == 0 {
		return hooks.Result{Data: []byte{}} // held: no complete frame yet
	}
	posture := s.posture()
	now := s.clock()
	// The rebuild tiles: Split reports every frame it finds, scanning
	// after each accepted frame's start (a wrapper embeds a complete
	// frame in its payload — mon150 parity), so a passed frame that
	// lies inside an already-passed frame's span is audited but not
	// re-emitted — a double-emit would be a silent downlink injection.
	var out []byte
	var end int64
	dropReason := ""
	for i := range frames {
		f := frames[i]
		kind, _ := frame.Decode(f)
		s.observeSession(fl, f, kind)
		v := policy.Decide(posture, f, kind)
		// The decision vocabulary matches the capture chunk decisions
		// (forwarded / dropped); the reason is the verdict's verbatim
		// audit string.
		rec := s.frameRecord(connID, fl, capture.DirS2C, now, f, kind, "", v.Reason, "")
		switch v.Action {
		case policy.ActionPass:
			rec.Decision = store.DecisionForwarded
			if f.Off >= end {
				out = append(out, f.Raw...)
				end = f.Off + int64(len(f.Raw))
				fl.mu.Lock()
				fl.reqs.add(f, askerCloud, now) // its answer goes back to the cloud
				fl.mu.Unlock()
			}
			// A Modbus write that reached the inverter is a confirmed
			// register change (a cloud push; ours goes through the
			// gateway and never reaches this path).
			if kind == frame.KindMbWrite {
				s.observe(Event{Kind: EventWrite, ConnID: connID, PDU: f.Payload})
			}
		case policy.ActionDrop:
			rec.Decision = store.DecisionDropped
			if dropReason == "" {
				dropReason = v.Reason
			}
		}
		s.st.RecordFrame(rec)
		s.observeFirmwareTask(connID, kind, f.Payload, rec.Decision)
	}
	if len(out) == 0 {
		return hooks.Result{Data: []byte{}, Reason: dropReason}
	}
	return hooks.Result{Data: out}
}

// observeFirmwareTask reports a cloud firmware upgrade task and what
// happened to it (profile §6).
func (s *Service) observeFirmwareTask(connID string, kind frame.Kind, payload []byte, d store.Decision) {
	if kind != frame.KindOther {
		return
	}
	if t, ok := frame.ParseFwTask(payload); ok {
		s.observe(Event{Kind: EventFirmwareTask, ConnID: connID, FwTask: &t, Decision: d})
	}
}

// observeSession pins the session identity from the first s2c frames:
// the device field (for gateway injection) and the Modbus envelope fn.
func (s *Service) observeSession(fl *flow, f frame.Frame, kind frame.Kind) {
	fl.mu.Lock()
	if !fl.devSet {
		fl.dev = f.Device
		fl.devSet = true
	}
	var setEnv bool
	if !fl.fnSet && (kind == frame.KindMbread || kind == frame.KindMbWrite) {
		fl.envFn = f.Fn
		fl.fnSet = true
		setEnv = true
	}
	fl.mu.Unlock()
	if setEnv {
		s.gw.SetEnvFn(f.Fn)
	}
}

// ---------- gateway.Injector (C-003) ----------

// InjectS2C is the gateway's injection point: it writes one raw s2c
// frame onto the flow's client leg (captured as an injected chunk,
// origin framesvc) after substituting the session's device tail — the
// gateway built the frame from its reference device; the live session's
// tail comes from the first s2c frame observed.
func (s *Service) InjectS2C(connID string, raw []byte) error {
	fl := s.flow(connID)
	if fl == nil {
		return fmt.Errorf("framesvc: no flow %s", connID)
	}
	fl.mu.Lock()
	dev := fl.dev
	fl.mu.Unlock()
	b := substituteDeviceTail(raw, dev)
	s.trackLocal(fl, b)
	if err := s.writeS2C(fl, b); err != nil {
		s.st.RecordAudit(s.clock(), "inject-error", connID+": "+err.Error())
		return err
	}
	s.recordInjected(connID, fl, s.clock(), b, "gateway")
	return nil
}

// substituteDeviceTail rewrites the device tail (bytes 3–6 of the raw
// frame) to the session's observed tail and recomputes the CRC16. It
// only touches well-formed frames (len == frameHeaderLen + plen +
// frameTailLen); everything else passes through unchanged.
func substituteDeviceTail(raw []byte, dev [4]byte) []byte {
	if len(raw) < frameHeaderLen+frameTailLen || raw[0] != raw[1] {
		return raw
	}
	plen := int(raw[plenHi])<<byteShift | int(raw[plenLo])
	if len(raw) != frameHeaderLen+plen+frameTailLen {
		return raw
	}
	b := make([]byte, len(raw))
	copy(b, raw)
	b[3], b[4], b[5] = dev[1], dev[2], dev[3]
	crc := frame.CRC16(b[2 : frameHeaderLen+plen])

	b[frameHeaderLen+plen] = byte(crc & byteMask)
	b[frameHeaderLen+1+plen] = byte(crc >> byteShift)
	return b
}

// writeS2C writes s2c bytes to the client leg, waiting for the relay to
// be ready (injections can be queued at open, before the dial).
func (s *Service) writeS2C(fl *flow, data []byte) error {
	select {
	case <-fl.hc.Ready():
	case <-fl.hc.Done():
		return errors.New("framesvc: flow closed before write")
	case <-fl.quit:
		return errors.New("framesvc: shutting down")
	}
	if _, err := fl.hc.WriteS2C(data); err != nil {
		return fmt.Errorf("framesvc: write: %w", err)
	}
	return nil
}

// trackLocal records this app's requests in raw as awaiting answers —
// before the write: the inverter answers within tens of ms.
func (s *Service) trackLocal(fl *flow, raw []byte) {
	frs, _ := frame.Split(raw, 0)
	now := s.clock()
	fl.mu.Lock()
	defer fl.mu.Unlock()
	for i := range frs {
		fl.reqs.add(frs[i], askerLocal, now)
	}
}

// recordInjected records one injected s2c byte sequence as frame rows.
func (s *Service) recordInjected(connID string, fl *flow, now time.Time, raw []byte, origin string) {
	frs, _ := frame.Split(raw, 0)
	for i := range frs {
		kind, _ := frame.Decode(frs[i])
		s.st.RecordFrame(s.frameRecord(connID, fl, capture.DirS2C, now, frs[i], kind, store.DecisionInjected, "", origin))
	}
}

// ---------- helpers ----------

// recordResidue records a partial frame left in the reassembler at
// close (audit: the stream ended mid-frame).
func (s *Service) recordResidue(connID string, fl *flow, dir capture.Dir, off int64, residue []byte) {
	if len(residue) == 0 {
		return
	}
	rec := store.FrameRecord{
		ConnID:   connID,
		Seq:      int(fl.seq.Add(1)),
		Dir:      dir,
		Off:      off,
		TS:       s.clock(),
		Class:    frame.KindResidue,
		Decision: store.DecisionResidue,
		Plen:     len(residue),
		Payload:  residue,
	}
	s.st.RecordFrame(rec)
}

// frameRecord builds the store row for one decoded frame.
func (s *Service) frameRecord(
	connID string,
	fl *flow,
	dir capture.Dir,
	now time.Time,
	f frame.Frame,
	kind frame.Kind,
	decision store.Decision,
	reason, origin string,
) store.FrameRecord {
	return store.FrameRecord{
		ConnID:   connID,
		Seq:      int(fl.seq.Add(1)),
		Dir:      dir,
		Off:      f.Off,
		TS:       now,
		Class:    kind,
		Dev:      f.Device,
		Fn:       f.Fn,
		Plen:     len(f.Payload),
		Payload:  f.Payload,
		Decision: decision,
		Reason:   reason,
		Origin:   origin,
		Decoded:  decodedJSON(kind, f.Payload),
	}
}

// decodedJSON returns the Decoded value for audit (telemetry, snap61,
// Modbus); nil when the class carries no structured decode.
func decodedJSON(kind frame.Kind, raw []byte) json.RawMessage {
	var v any
	// frame.Kind is open-ended (SnapKind): the known classes, then the default
	//exhaustive:ignore
	switch kind {
	case frame.KindTele:
		t, err := frame.ParseTele(raw)
		if err != nil {
			return nil
		}
		v = t
	case frame.KindSnap61:
		s61, err := frame.ParseSnap61(raw)
		if err != nil {
			return nil
		}
		var lo, hi *int
		if s61.HasCellTemps {
			lo, hi = &s61.CellMinDeciC, &s61.CellMaxDeciC
		}
		v = struct {
			SN           string   `json:"sn"`
			SocPct       uint16   `json:"soc_pct"`
			EnergyUnits  uint16   `json:"energy_units"`
			BusVoltage   []uint16 `json:"bus_voltage"`
			CellMinDeciC *int     `json:"cell_temp_min_deci_c,omitempty"`
			CellMaxDeciC *int     `json:"cell_temp_max_deci_c,omitempty"`
		}{s61.SN, s61.SocPct, s61.EnergyUnits, s61.BusVoltage, lo, hi}
	case frame.KindWrapper:
		w, err := frame.ParseWrapper(raw)
		if err != nil {
			return nil
		}
		v = w
	case frame.KindMbread, frame.KindMbreadResp, frame.KindMbWrite, frame.KindMbecho:
		pdu, err := frame.ParseModbus(raw)
		if err != nil {
			return nil
		}
		v = pdu
	case frame.KindOther:
		return frame.DescribeOther(raw)
	case frame.KindKeepalive, frame.KindRSSI, frame.KindCfg40, frame.KindRegister, frame.KindCfg91, frame.KindSerialQuery, frame.KindBootstrapResp:
		return nil
	default: // the remaining snapNN classes carry no structured decode
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}
