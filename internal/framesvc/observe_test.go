package framesvc

import (
	"crypto/tls"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nicois/prox/pkg/capture"
	"github.com/stretchr/testify/require"

	"foxesslocal/internal/frame"
)

// pduWriteLim is the s2c fn10 @41021 (0xA03D) x2 write (import limit 5000 W / 0):
// unit fn addr cnt bytecount data — the cloud limit push.
var pduWriteLim = []byte{0x01, 0x10, 0xa0, 0x3d, 0x00, 0x02, 0x04, 0x13, 0x88, 0x00, 0x00}

// evCollector records observed events (the observer fires on the hook
// goroutine; the test reads a copy under the mutex).
type evCollector struct {
	mu  sync.Mutex
	evs []Event
}

func (e *evCollector) add(ev Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.evs = append(e.evs, ev)
}

func (e *evCollector) all() []Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Event, len(e.evs))
	copy(out, e.evs)
	return out
}

// kinds returns the event kinds in order.
func (e *evCollector) kinds() []string {
	out := make([]string, 0, len(e.evs))
	for _, ev := range e.all() {
		out = append(out, ev.Kind)
	}
	return out
}

func (e *evCollector) wait(t *testing.T, kind string) Event {
	t.Helper()
	spinUntil(t, func() bool {
		for _, ev := range e.all() {
			if ev.Kind == kind {
				return true
			}
		}
		return false
	})
	for _, ev := range e.all() {
		if ev.Kind == kind {
			return ev
		}
	}
	t.Fatal("unreachable")
	return Event{}
}

// Scenario A: the observer sees each event exactly once at the right
// point — up, tele, the forwarded write, and down at the close. The
// firmware-sized frame that follows the write is held in full mode (an
// unrecognised kind needs a maintenance window) and observes no event.
func TestObserverEventSet(t *testing.T) {
	r := newRigBase(t, fullPosture())
	col := &evCollector{}
	r.newService(WithObserver(col.add))

	var (
		connID  string
		teleEv  Event
		writeEv Event
		downEv  Event
	)
	synctest.Test(t, func(t *testing.T) {
		upRelayEnd, upCloudEnd := chanpipePair(t)
		fl := r.startFlow(t, cloudPort, upRelayEnd, upCloudEnd)

		// Registration: mirrored upstream; the real cloud owns the
		// bootstrap in full mode (its response trio relays to the
		// client verbatim — no local injection, profile §3).
		_, err := fl.client.Write(regFrames(t))
		require.NoError(t, err)
		r.rec.waitOpens(t, 1)
		connID = r.rec.idAt(t, 0)
		cloud := cloudTrio(t)
		_, err = upCloudEnd.Write(cloud)
		require.NoError(t, err)
		require.Equal(t, cloud, readExact(t, fl.client, len(cloud)))

		// One tele frame (150 B, the 5 s cadence's one sample); the
		// battery power is -500 W (s32 two's complement on the wire;
		// the uint32 literal is the 32-bit encoding of -500).
		_, err = fl.client.Write(teleFrame(t, 100, 200, 300, 400, 0xfffffe0c, 42))
		require.NoError(t, err)
		teleEv = col.wait(t, EventTelemetry)

		// The s2c fn10 write: forwarded to the client leg and observed
		// as a confirmed register write (full mode passes writes).
		wr := mkFrame(t, startS2C, devS2C, envFnS2C, pduWriteLim)
		_, err = upCloudEnd.Write(wr)
		require.NoError(t, err)
		require.Equal(t, wr, readExact(t, fl.client, len(wr)))
		writeEv = col.wait(t, EventWrite)

		// The firmware chunk (1024 B payload): held, not forwarded, and
		// it observes no event.
		_, err = upCloudEnd.Write(fwChunk(t, 0xa5))
		require.NoError(t, err)

		r.endFlow(t, fl, upCloudEnd, int64(len(regFrames(t))+teleFrameBytes))
		downEv = col.all()[len(col.all())-1]
	})

	// Each event exactly once, in order.
	require.Equal(t, []string{
		EventSessionUp, EventTelemetry, EventWrite, EventSessionDown,
	}, col.kinds())

	upEv := col.all()[0]
	require.Equal(t, connID, upEv.ConnID)
	require.Equal(t, upEv.ConnID, downEv.ConnID)

	require.Equal(t, int32(100), teleEv.Tele.Sol)
	require.Equal(t, int32(-500), teleEv.Tele.Batt)
	require.Equal(t, byte(42), teleEv.Tele.Soc)

	require.Equal(t, pduWriteLim, writeEv.PDU)
}

// Scenario B: a client-leg TLS failure is observed (the session-down
// variant) and the flow closes.
func TestObserverTLSFailure(t *testing.T) {
	r := newRigBase(t, fullPosture())
	col := &evCollector{}
	r.newService(WithObserver(col.add))
	r.ladder.LeafSelector = func(string, string) (*tls.Certificate, error) {
		return nil, errors.New("simulated mint failure")
	}
	var tlsEv, downEv Event
	synctest.Test(t, func(t *testing.T) {
		upRelayEnd, upCloudEnd := chanpipePair(t)
		fl := r.startFlow(t, cloudPort, upRelayEnd, upCloudEnd)
		//nolint:gosec // G402: scripted test endpoint — the trust anchor is the test's own CA
		tc := tls.Client(fl.client, &tls.Config{InsecureSkipVerify: true})
		// The ladder peeks the ClientHello, fails the mint, and closes
		// the flow; the client handshake fails with it (ignored here —
		// the observer is the assertion).
		go func() { _ = tc.Handshake() }()
		<-fl.finished
		require.Error(t, <-fl.done, "the flow ends in the mint failure")
		fl.ended = true
		_ = upCloudEnd.Close()
		<-fl.upDrained
		r.st.WaitStreams()
	})
	for _, ev := range col.all() {
		switch ev.Kind {
		case EventTLS:
			tlsEv = ev
		case EventSessionDown:
			downEv = ev
		}
	}
	// Host is empty: the inverter dials the cloud by raw IP (OQ-1) —
	// the session has no DNS name.
	require.Equal(t, "mint_failed", tlsEv.Result)
	require.Empty(t, tlsEv.Host)
	require.NotEmpty(t, tlsEv.ConnID)
	require.Equal(t, "mint_failed", downEv.Reason)
	require.Equal(t, []string{EventSessionUp, EventTLS, EventSessionDown}, col.kinds())
}

// No observer: the same traffic runs unchanged (the nil-safe check).
func TestNoObserverNoChange(t *testing.T) {
	r := newRig(t, fullPosture())
	var wantC2S int64
	synctest.Test(t, func(t *testing.T) {
		upRelayEnd, upCloudEnd := chanpipePair(t)
		fl := r.startFlow(t, cloudPort, upRelayEnd, upCloudEnd)
		reg := regFrames(t)
		wantC2S = int64(len(reg))
		_, err := fl.client.Write(reg)
		require.NoError(t, err)
		r.rec.waitOpens(t, 1)
		cloud := cloudTrio(t)
		_, err = upCloudEnd.Write(cloud)
		require.NoError(t, err)
		require.Equal(t, cloud, readExact(t, fl.client, len(cloud)))
		r.endFlow(t, fl, upCloudEnd, wantC2S)
	})
	r.st.Flush()
	fwd, err := r.st.AuditFrames(decForwarded, capture.DirC2S)
	require.NoError(t, err)
	require.Len(t, fwd, 3)
}

// TestTeleFrameYieldsToPoll: while the register poll supplies telemetry,
// a 150 B tele frame (the cloud's model, no EPS field) publishes no
// telemetry event — interleaved with the poll's, the live values flipped
// between the two (2026-11-17).
func TestTeleFrameYieldsToPoll(t *testing.T) {
	r := newRigBase(t, fullPosture())
	col := &evCollector{}
	r.newService(WithObserver(col.add))

	synctest.Test(t, func(t *testing.T) {
		upRelayEnd, upCloudEnd := chanpipePair(t)
		fl := r.startFlow(t, cloudPort, upRelayEnd, upCloudEnd)
		_, err := fl.client.Write(regFrames(t))
		require.NoError(t, err)
		r.rec.waitOpens(t, 1)
		cloud := cloudTrio(t)
		_, err = upCloudEnd.Write(cloud)
		require.NoError(t, err)
		require.Equal(t, cloud, readExact(t, fl.client, len(cloud)))

		// a poll sample just now, then the cloud-model frame
		r.st.RecordTelemetry("poll", time.Now(), frame.Tele{Sol: 100, Eps: 3800})
		_, err = fl.client.Write(teleFrame(t, 100, 200, 300, 400, 0xfffffe0c, 42))
		require.NoError(t, err)

		r.endFlow(t, fl, upCloudEnd, int64(len(regFrames(t))+teleFrameBytes))
	})
	require.Equal(t, []string{EventSessionUp, EventSessionDown}, col.kinds())
}
