package framesvc

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nicois/prox/pkg/capture"
	"github.com/nicois/prox/pkg/chanpipe"
	"github.com/nicois/prox/pkg/hooks"
	"github.com/nicois/prox/pkg/proxy"
	"github.com/nicois/prox/pkg/sink"
	"github.com/nicois/prox/pkg/sniff"

	"github.com/stretchr/testify/require"

	"foxesslocal/internal/frame"
	"foxesslocal/internal/gateway"
	"foxesslocal/internal/policy"
	"foxesslocal/internal/store"
)

// The decision / origin / class / audit-kind vocabularies the scenarios
// assert on (shared so goconst sees one copy).
const (
	decForwarded = "forwarded"
	decDropped   = "dropped"
	decInjected  = "injected"
	decResidue   = "residue"

	originBootstrap = "bootstrap"
	originTrigread  = "trigread"
	originGateway   = "gateway"

	// testOrigin is the command origin the scenario tests submit with.
	testOrigin = "test"

	classBootstrapResp = frame.KindBootstrapResp
	classMbread        = frame.KindMbread
	classMbwrite       = frame.KindMbWrite
	classTele          = frame.KindTele
	classRegister      = frame.KindRegister

	auditFlowOpen = "flow-open"

	// reasonFull / reasonTelemetryDeny are the verdict reasons the
	// scenarios assert on (the audit-trust-stable strings).
	reasonFull          = "mode:full"
	reasonTelemetryDeny = "mode:telemetry_only deny:"
	reasonTelemetryRead = "mode:telemetry_only read:"
	reasonWindowPass    = "maintenance-window"
)

// The test four-tuple: an inverter-style client dialing a cloud endpoint
// on 14431 by raw IP (the deployment shape); altPort stands in for a
// non-cloud flow on the same rig.
const (
	clientIP   = "192.168.4.20"
	clientPort = 44723
	dstIP      = "203.0.116.72"
	altPort    = 443
)

// Frame-layout constants (the test frame builder mirrors the codec).
const (
	testFrameOverhead = 13 // start(2)+dev(4)+fn(1)+plen(2)+crc(2)+term(2)
	startC2S          = 0x7e
	startS2C          = 0x7f
	fnData            = 0x10
	envFnS2C          = 0x2f // the s2c Modbus envelope fn the scenarios pin

	telePayloadLen = 150
	teleSolOff     = 35
	teleGridOff    = 39
	teleHausOff    = 43
	teleAcportOff  = 59
	teleBattOff    = 67
	teleChgOff     = 72
	teleSocOff     = 79
	teleIdle       = 2

	keepalivePayloadLen = 135
	fwPayloadLen        = 1024
)

// Whole-frame sizes the scenarios read byte-exact.
const (
	teleFrameBytes   = testFrameOverhead + telePayloadLen      // 163
	keepaliveBytes   = testFrameOverhead + keepalivePayloadLen // 148
	fwFrameBytes     = testFrameOverhead + fwPayloadLen        // 1037
	modbusFrameBytes = testFrameOverhead + 6                   // 19
	trioBytes        = 45                                      // 18 + 13 + 14
)

var (
	devInv  = [4]byte{0x11, 0, 0, 1}
	devKA   = [4]byte{0x06, 0x6a, 0xeb, 0x66}
	devRegA = [4]byte{0x2a, 0, 0x7b, 0xc5}
	devRegB = [4]byte{0x2c, 0, 0x7b, 0xce}
	devRegC = [4]byte{0x2b, 0, 0x7b, 0xd3}
	devS2C  = [4]byte{0x11, 0xaa, 0xbb, 0xcc}
	// a write leads 0x12 (echoed 0x92; profile §5).
	devS2CWrite = [4]byte{0x12, 0xaa, 0xbb, 0xcc}
	// the inverter echoes the request's device tail in its answer (11-05
	// write echoes and 11-15 read answers, every frame).
	devEcho = [4]byte{0x92, 0xaa, 0xbb, 0xcc}
	devAns  = [4]byte{0x91, 0xaa, 0xbb, 0xcc} // a read answer to a devS2C request
	dev91   = [4]byte{0x91, 0, 0, 0}          // the inverter's Modbus response device (request dev0 0x11 | 0x80; classTable: cfg91/mbecho)

	pduReadWM  = []byte{0x01, 0x03, 0xa0, 0x28, 0, 1} // fn03 @41000 x1
	pduReadLim = []byte{0x01, 0x03, 0xa0, 0x45, 0, 2} // fn03 @41021 x2
	pduWriteWM = []byte{0x01, 0x06, 0xa0, 0x28, 0, 1} // fn06 @41000 = 1
	regPayload = []byte{0x01, 0x00, 0x01, 0x11, 0x0f, '6', '0', '9', 'W', '6', 'F', 'G', 'F', '5', 'A', 'Y', 'B', '1', '1', '7', 0x00, 0x00, 0x4c}
)

// testAddr presents the declared client address on the in-memory client
// leg (the ladder derives hooks.Conn.Src/SrcPort from RemoteAddr, exactly
// as on a real TPROXY socket; chanpipe carries no address).
type testAddr struct {
	net.Conn
	remote net.Addr
}

func (a testAddr) RemoteAddr() net.Addr { return a.remote }

// svcInjector closes over the *Service slot: the gateway is constructed
// before the service, and the slot is filled right after New returns —
// the one-liner task 10's wiring reuses.
type svcInjector struct{ p **Service }

func (i *svcInjector) InjectS2C(connID string, raw []byte) error {
	if *i.p == nil {
		return errors.New("framesvc test: service not wired")
	}
	return (*i.p).InjectS2C(connID, raw)
}

// recorder watches the pipeline (matches everything): it records the
// opened connection IDs on a buffered channel — deterministic
// synchronization, no timers.
type recorder struct {
	mu    sync.Mutex
	ids   []string
	opens chan struct{}
}

func newRecorder() *recorder {
	return &recorder{opens: make(chan struct{}, 16)}
}

func (r *recorder) Name() string { return "recorder" }

func (r *recorder) Filter() hooks.Match { return hooks.Match{} }

func (r *recorder) OnOpen(c *hooks.Conn, _ *hooks.HookConn) hooks.Result {
	r.mu.Lock()
	r.ids = append(r.ids, c.ID)
	r.mu.Unlock()
	r.opens <- struct{}{}
	return hooks.Result{}
}

func (r *recorder) OnData(_ *hooks.Conn, _ hooks.Dir, _ []byte) hooks.Result { return hooks.Result{} }

func (r *recorder) OnClose(_ *hooks.Conn, _ string) {}

// waitOpens blocks until at least n connections have opened (the opens
// channel is only the wake signal; the count is cumulative).
func (r *recorder) waitOpens(t *testing.T, n int) {
	t.Helper()
	for {
		r.mu.Lock()
		got := len(r.ids)
		r.mu.Unlock()
		if got >= n {
			return
		}
		<-r.opens
	}
}

// idAt returns the i-th opened connection ID.
func (r *recorder) idAt(t *testing.T, i int) string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	require.Less(t, i, len(r.ids), "fewer than %d connections opened", i+1)
	return r.ids[i]
}

// rig is the in-memory relay rig: an embedded-mode ladder (DataRoot "":
// noop sink, no file capture) with the frame service on a pipeline ahead
// of nothing, a capture registry (the store mirrors it), and a scripted
// upstream through Ladder.Dial.
type rig struct {
	t        *testing.T
	st       *store.Store
	gw       *gateway.Gateway
	svc      *Service
	reg      *capture.Registry
	ladder   *proxy.Ladder
	rec      *recorder
	posture  *policy.Posture
	pipeline *hooks.Pipeline

	// clock is the service's clock: virtual wall time by default, pinned
	// to a corpus epoch by the golden-transaction suite.
	clock func() time.Time

	// dialMu/dialTo: the Ladder's Dial function is installed once and
	// resolves the target by destination port under the lock. The port
	// key (not a shared "current target") keeps flows correct when an
	// earlier flow's dial runs only after a later flow has started —
	// the ladder dials after the OnOpen hook, so the test's waitOpens
	// does not order the dials.
	dialMu sync.Mutex
	dialTo map[int]net.Conn
}

// newRig builds the rig. The store lives outside the synctest bubble (its
// flusher goroutine must not join the group): it is constructed here by
// the test, before the bubble, and torn down in t.Cleanup (LIFO: after
// every flow ended).
func newRig(t *testing.T, posture policy.Posture) *rig {
	t.Helper()
	r := newRigBase(t, posture)
	r.newService()
	return r
}

// newRigBase builds the store/registry/gateway/ladder half of the rig
// (no service yet): tests that need a service option (e.g. an observer)
// call newService on the result.
func newRigBase(t *testing.T, posture policy.Posture) *rig {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"), "dev")
	require.NoError(t, err)
	reg := capture.NewRegistry()
	st.AttachRegistry(reg)
	r := &rig{t: t, st: st, reg: reg, posture: &posture, dialTo: map[int]net.Conn{}}
	r.clock = time.Now
	t.Cleanup(func() {
		require.NoError(t, st.Close())
	})
	r.rec = newRecorder()
	pipeline := hooks.NewPipeline()
	pipeline.Register(r.rec)
	// The injector closes over the rig's service slot (nil until
	// newService fills it) — the one-liner task 10's wiring reuses.
	r.gw = gateway.New(&svcInjector{p: &r.svc}, time.Now)
	r.pipeline = pipeline
	r.ladder = &proxy.Ladder{
		Sink:             func(string, string, int64) (*sink.Conn, error) { return sink.Noop(), nil },
		DataRoot:         "",
		MaxPeek:          sniff.MaxPeek,
		MaxBytes:         256 << 20,
		HandshakeTimeout: 30 * time.Second,
		Hooks:            pipeline,
		Capture:          reg,
		Dial: func(addr net.Addr) (net.Conn, error) {
			r.dialMu.Lock()
			defer r.dialMu.Unlock()
			c, ok := r.dialTo[addr.(*net.TCPAddr).Port]
			if !ok {
				return nil, errors.New("framesvc test: no dial target for " + addr.String())
			}
			return c, nil
		},
	}
	return r
}

// newService registers the frame service on the rig's pipeline (the
// injector slot is filled by the assignment below).
func (r *rig) newService(opts ...Option) {
	r.svc = New(r.pipeline, r.st, r.gw, func() policy.Posture { return *r.posture }, r.clock, opts...)
}

// chanpipePair is one in-memory pipe pair (the relay takes end a, the
// test the cloud end b) — created by the test inside the bubble, per the
// chanpipe synctest rules.
func chanpipePair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	a, b := chanpipe.New()
	return a, b
}

// c2sSink is the drainer's writer: it keeps the consumed c2s bytes for
// byte-exactness assertions and counts them per Write. The append
// precedes the counter add, so the count reaching want implies every
// byte is in upBytes (the spin's happens-before).
type c2sSink struct{ fl *flowLegs }

func (w *c2sSink) Write(b []byte) (int, error) {
	w.fl.upBytes = append(w.fl.upBytes, b...)
	atomic.AddInt64(&w.fl.upN, int64(len(b)))
	return len(b), nil
}

// flowLegs is one in-memory flow: the client (inverter side) leg, the
// relay teardown handles, and the c2s drainer state.
type flowLegs struct {
	client    net.Conn
	done      chan error
	finished  chan struct{}
	ended     bool
	upDrained chan struct{}
	upN       int64
	upBytes   []byte // the c2s bytes the drainer consumed, in order
}

// startFlow starts one flow to dstPort. The relay dials upRelayEnd once
// the client writes its first byte (the ladder peeks before dialing), and
// a drainer consumes the relay's c2s output on upCloudEnd (chanpipe
// writes block until read; the relay's c2s copy would otherwise stall).
func (r *rig) startFlow(t *testing.T, dstPort int, upRelayEnd, upCloudEnd net.Conn) *flowLegs {
	t.Helper()
	r.dialMu.Lock()
	r.dialTo[dstPort] = upRelayEnd
	r.dialMu.Unlock()
	serverEnd, clientEnd := chanpipe.New()
	fl := &flowLegs{
		client:    clientEnd,
		done:      make(chan error, 1),
		finished:  make(chan struct{}),
		upDrained: make(chan struct{}),
	}
	go func() {
		dst := &net.TCPAddr{IP: net.ParseIP(dstIP), Port: dstPort}
		src := &net.TCPAddr{IP: net.ParseIP(clientIP), Port: clientPort}
		fl.done <- r.ladder.Serve(testAddr{Conn: serverEnd, remote: src}, dst)
		close(fl.finished)
	}()
	go func() {
		defer close(fl.upDrained)
		// Count (and capture) per chunk: endFlow settles on the count
		// before closing the cloud end, so it must grow while the flow
		// is live.
		_, _ = io.Copy(&c2sSink{fl}, upCloudEnd)
	}()
	t.Cleanup(func() {
		_ = clientEnd.Close()
		_ = upCloudEnd.Close()
		if !fl.ended {
			<-fl.finished
			require.NoError(t, <-fl.done)
		}
	})
	return fl
}

// endFlow ends the flow deterministically (the relay torn down, the
// c2s drainer finished) and settles the capture-stream persistence:
// WaitStreams must run INSIDE the bubble — the store's stream WaitGroup
// is associated with the bubble through the persistStream group
// goroutines, and waiting on it after the bubble panics. Store frame
// rows stay enqueued after it; call st.Flush() from the test's outer
// scope (the Flush handshake is atomic — it is bubble-safe).
func (r *rig) endFlow(t *testing.T, fl *flowLegs, upCloudEnd net.Conn, wantC2S int64) {
	t.Helper()
	// Settle: the relay's c2s copy must have delivered every byte before
	// the cloud end is closed (a write racing the close fails with
	// "chanPipe: closed" and poisons the Serve error).
	spinUntil(t, func() bool { return atomic.LoadInt64(&fl.upN) == wantC2S })
	_ = fl.client.Close()
	_ = upCloudEnd.Close()
	<-fl.finished
	require.NoError(t, <-fl.done)
	fl.ended = true
	<-fl.upDrained
	r.st.WaitStreams()
}

// readExact reads exactly n bytes from the client leg.
func readExact(t *testing.T, c net.Conn, n int) []byte {
	t.Helper()
	buf := make([]byte, n)
	_, err := io.ReadFull(c, buf)
	require.NoError(t, err, "reading %d bytes from the client leg", n)
	return buf
}

// readWithin reads exactly n bytes from the client leg, or fails after
// d of virtual time — a read that never completes (e.g. the pre-fix
// no-re-emit deadlock) must fail the bubble fast, not hang it.
func readWithin(t *testing.T, c net.Conn, n int, d time.Duration) []byte {
	t.Helper()
	ch := make(chan []byte, 1)
	go func() {
		buf := make([]byte, n)
		if _, err := io.ReadFull(c, buf); err != nil {
			return
		}
		ch <- buf
	}()
	select {
	case b := <-ch:
		return b
	case <-time.After(d):
		t.Fatalf("readWithin: %d bytes did not arrive within %v (virtual)", n, d)
	}
	return nil
}

// spinUntil polls cond (Gosched between polls) until it holds.
func spinUntil(t *testing.T, cond func() bool) {
	t.Helper()
	for range spinYields {
		if cond() {
			return
		}
		runtime.Gosched()
	}
	t.Fatal("spinUntil: condition not reached")
}

const spinYields = 20000000

// mkFrame builds one raw frame: the start marker twice, dev(4), fn,
// plen(2) BE, payload, crc16(dev..payload) LE, and the 2-byte
// terminator (the nibble-swap of start: 0x7e→0xe7, 0x7f→0xf7) — the
// same layout the codec validates.
func mkFrame(t *testing.T, start byte, dev [4]byte, fn byte, payload []byte) []byte {
	t.Helper()
	body := make([]byte, 0, testFrameOverhead+len(payload))
	body = append(body, start, start)
	body = append(body, dev[:]...)
	//nolint:gosec // G115: test payloads are far under 64 KiB.
	body = append(body, fn, byte(len(payload)>>byteShift), byte(len(payload)&byteMask))
	body = append(body, payload...)
	crc := frame.CRC16(body[2:])

	body = append(body, byte(crc&byteMask), byte(crc>>byteShift))
	term := (start&0x0f)<<4 | start>>4
	body = append(body, term, term)
	return body
}

// telePayload builds a 150 B power-telemetry payload with the given
// powers and SoC (ParseTele field map, profile §4).
func telePayload(sol, grid, haus, acport, batt uint32, soc byte) []byte {
	p := make([]byte, telePayloadLen)
	binary.BigEndian.PutUint32(p[teleSolOff:], sol)
	binary.BigEndian.PutUint32(p[teleGridOff:], grid)
	binary.BigEndian.PutUint32(p[teleHausOff:], haus)
	binary.BigEndian.PutUint32(p[teleAcportOff:], acport)
	binary.BigEndian.PutUint32(p[teleBattOff:], batt)
	p[teleChgOff] = teleIdle
	p[teleSocOff] = soc
	return p
}

// teleFrame is one c2s 150 B power-telemetry frame.
func teleFrame(t *testing.T, sol, grid, haus, acport, batt uint32, soc byte) []byte {
	t.Helper()
	return mkFrame(t, startC2S, devInv, fnData, telePayload(sol, grid, haus, acport, batt, soc))
}

// keepaliveFrame is one c2s K12104 keepalive (135 B, dev0 0x06).
func keepaliveFrame(t *testing.T) []byte {
	t.Helper()
	return mkFrame(t, startC2S, devKA, fnData, make([]byte, keepalivePayloadLen))
}

// regFrames is the c2s registration trio request in order (dev0
// 0x2a, 0x2c, 0x2b — the trio pattern the service tracks).
func regFrames(t *testing.T) []byte {
	t.Helper()
	var all []byte
	all = append(all, mkFrame(t, startC2S, devRegA, 0x5f, regPayload)...)
	all = append(all, mkFrame(t, startC2S, devRegB, 0x76, []byte{0xe1, 0x01, 0x04, 0x00})...)
	all = append(all, mkFrame(t, startC2S, devRegC, 0x0c, []byte{0x00})...)
	return all
}

// mbreadS2C is the 19 B s2c fn03 @41000 x1 frame the scenarios forward
// (envelope fn envFnS2C, device devS2C — the session identity).
func mbreadS2C(t *testing.T) []byte {
	t.Helper()
	return mkFrame(t, startS2C, devS2C, envFnS2C, pduReadWM)
}

// fwChunk is a 1024 B firmware-style s2c payload of one repeated byte.
func fwChunk(t *testing.T, fill byte) []byte {
	t.Helper()
	return mkFrame(t, startS2C, devS2C, fnData, bytes.Repeat([]byte{fill}, fwPayloadLen))
}

// fullPosture is full mode: the kinds the cloud is known to send pass;
// anything else (a firmware-sized frame, say) is held without a window.
func fullPosture() policy.Posture {
	return policy.Posture{Mode: policy.ModeFull}
}

// offPosture is the no-forwarding posture (D-011): the channel is served
// locally, no cloud leg.
func offPosture() policy.Posture {
	return policy.Posture{Mode: policy.ModeOff}
}

// offServePolicy is the off-mode ServePolicy double (the real one is
// forward.State, which framesvc cannot import — import cycle): every
// connection is served locally on a fresh black leg.
type offServePolicy struct{}

func (offServePolicy) Decide(*hooks.Conn) (ServeMode, ServeLeg, string, error) {
	return ServeLocal, newBlackLeg(), "off: local", nil
}

// blackLeg is the off-mode local leg double (the real one is
// forward.State's): two io.Pipe pairs in prox's ServeLocal contract —
// RelayLeg: Write = c2s (the relay writes, framesvc drains), Read =
// s2c (idle; hook injection bypasses the leg); Drain: the c2s reader.
type blackLeg struct {
	c2sR *io.PipeReader
	c2sW *io.PipeWriter
	s2cR *io.PipeReader
	s2cW *io.PipeWriter
}

func newBlackLeg() *blackLeg {
	c2sR, c2sW := io.Pipe()
	s2cR, s2cW := io.Pipe()
	return &blackLeg{c2sR: c2sR, c2sW: c2sW, s2cR: s2cR, s2cW: s2cW}
}

func (b *blackLeg) RelayLeg() io.ReadWriteCloser { return &blackRelayLeg{b: b} }
func (b *blackLeg) Drain() io.ReadCloser         { return &blackDrainLeg{b: b} }

type blackRelayLeg struct{ b *blackLeg }

func (r *blackRelayLeg) Read(p []byte) (int, error)  { return r.b.s2cR.Read(p) }
func (r *blackRelayLeg) Write(p []byte) (int, error) { return r.b.c2sW.Write(p) }
func (r *blackRelayLeg) Close() error {
	_ = r.b.c2sW.Close()
	_ = r.b.s2cW.Close()
	_ = r.b.c2sR.Close()
	_ = r.b.s2cR.Close()
	return nil
}

type blackDrainLeg struct{ b *blackLeg }

func (d *blackDrainLeg) Read(p []byte) (int, error) { return d.b.c2sR.Read(p) }
func (d *blackDrainLeg) Close() error               { return (&blackRelayLeg{b: d.b}).Close() }
