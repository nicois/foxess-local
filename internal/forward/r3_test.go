package forward

import (
	"errors"
	"io"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nicois/prox/pkg/capture"
	"github.com/nicois/prox/pkg/chanpipe"
	"github.com/nicois/prox/pkg/hooks"
	"github.com/nicois/prox/pkg/proxy"
	"github.com/nicois/prox/pkg/sink"
	"github.com/nicois/prox/pkg/sniff"

	"foxesslocal/internal/framesvc"
	"foxesslocal/internal/gateway"
	"foxesslocal/internal/policy"
	"foxesslocal/internal/store"
)

const (
	r3CloudPort  = 14431
	r3CloudIP    = "203.0.116.72"
	r3ClientIP   = "192.168.4.20"
	r3ClientPort = 44723
)

// r3Addr wraps the relay end with a RemoteAddr (the ladder needs one, as
// on a real TPROXY socket; chanpipe carries no address).
type r3Addr struct {
	net.Conn
	remote net.Addr
}

func (a r3Addr) RemoteAddr() net.Addr { return a.remote }

// r3Injector closes over the *framesvc.Service slot: the gateway is
// constructed before the service (R3 needs no s2c injection at all, but
// the gateway demands an Injector).
type r3Injector struct{ p **framesvc.Service }

func (i *r3Injector) InjectS2C(connID string, raw []byte) error {
	if *i.p == nil {
		return errors.New("r3 test: service not wired")
	}
	return (*i.p).InjectS2C(connID, raw)
}

// r3Recorder counts flow opens and records their ids (the serve audit
// rows are matched against these).
type r3Recorder struct {
	mu    sync.Mutex
	ids   []string
	opens chan struct{}
}

func newR3Recorder() *r3Recorder {
	return &r3Recorder{opens: make(chan struct{}, 16)}
}

func (r *r3Recorder) Name() string { return "r3-recorder" }

func (r *r3Recorder) Filter() hooks.Match { return hooks.Match{} }

func (r *r3Recorder) OnOpen(c *hooks.Conn, _ *hooks.HookConn) hooks.Result {
	r.mu.Lock()
	r.ids = append(r.ids, c.ID)
	r.mu.Unlock()
	r.opens <- struct{}{}
	return hooks.Result{}
}

func (r *r3Recorder) OnData(_ *hooks.Conn, _ hooks.Dir, _ []byte) hooks.Result {
	return hooks.Result{}
}

func (r *r3Recorder) OnClose(_ *hooks.Conn, _ string) {}

func (r *r3Recorder) waitOpens(t *testing.T, n int) {
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

func (r *r3Recorder) idAt(t *testing.T, i int) string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if i >= len(r.ids) {
		t.Fatalf("fewer than %d connections opened", i+1)
	}
	return r.ids[i]
}

// r3Rig is the transparent ladder + pipeline the machine serves through.
type r3Rig struct {
	ladder *proxy.Ladder
	rec    *r3Recorder
	dialMu sync.Mutex
	dialTo *net.Conn
}

func (r *r3Rig) setDialTo(c *net.Conn) {
	r.dialMu.Lock()
	r.dialTo = c
	r.dialMu.Unlock()
}

type r3Flow struct {
	client    net.Conn
	upCloud   net.Conn
	done      chan error
	finished  chan struct{}
	upDrained chan struct{}
	bytes     chan int64 // cumulative c2s bytes after each relay write
	ended     bool
}

type r3Counter struct {
	f *r3Flow
	n int64
}

func (c *r3Counter) Write(b []byte) (int, error) {
	c.n += int64(len(b))
	select {
	case c.f.bytes <- c.n:
	default:
	}
	return len(b), nil
}

// startFlow opens one flow to the cloud port: the relay dials once the
// client writes its first byte, and a drainer counts the relay's c2s
// output on upCloudEnd (chanpipe writes block until read; the relay's
// c2s copy would otherwise stall).
func (r *r3Rig) startFlow(t *testing.T, upRelayEnd, upCloudEnd net.Conn) *r3Flow {
	t.Helper()
	r.setDialTo(&upRelayEnd)
	serverEnd, clientEnd := chanpipe.New()
	fl := &r3Flow{
		client:    clientEnd,
		upCloud:   upCloudEnd,
		done:      make(chan error, 1),
		finished:  make(chan struct{}),
		upDrained: make(chan struct{}),
		bytes:     make(chan int64, 16),
	}
	go func() {
		dst := &net.TCPAddr{IP: net.ParseIP(r3CloudIP), Port: r3CloudPort}
		src := &net.TCPAddr{IP: net.ParseIP(r3ClientIP), Port: r3ClientPort}
		fl.done <- r.ladder.Serve(r3Addr{Conn: serverEnd, remote: src}, dst)
		close(fl.finished)
	}()
	go func() {
		defer close(fl.upDrained)
		_, _ = io.Copy(&r3Counter{f: fl}, upCloudEnd)
	}()
	t.Cleanup(func() {
		_ = clientEnd.Close()
		_ = upCloudEnd.Close()
		if !fl.ended {
			<-fl.finished
			if err := <-fl.done; err != nil {
				t.Errorf("r3 flow serve: %v", err)
			}
		}
	})
	return fl
}

// openFlow starts one flow, writes its first bytes, and waits until
// they are mirrored by the relay (the flow is established and serving).
func (r *r3Rig) openFlow(t *testing.T, label string, opens int) *r3Flow {
	t.Helper()
	upRelay, upCloud := chanpipe.New()
	fl := r.startFlow(t, upRelay, upCloud)
	if _, err := fl.client.Write([]byte(label)); err != nil {
		t.Fatalf("flow %s write: %v", label, err)
	}
	r.rec.waitOpens(t, opens)
	fl.expectBytes(t, int64(len(label)))
	return fl
}

// expectBytes blocks until the relay has forwarded at least want c2s
// bytes (the flow is alive and serving).
func (fl *r3Flow) expectBytes(t *testing.T, want int64) {
	t.Helper()
	for {
		n := <-fl.bytes
		if n >= want {
			return
		}
	}
}

// openLocalFlow starts a flow served locally (mode off): the relay
// never dials, the first bytes are drained by the service, and nothing
// reaches the dial target.
func (r *r3Rig) openLocalFlow(t *testing.T, label string, opens int) *r3Flow {
	t.Helper()
	upRelay, upCloud := chanpipe.New()
	fl := r.startFlow(t, upRelay, upCloud)
	if _, err := fl.client.Write([]byte(label)); err != nil {
		t.Fatalf("flow %s write: %v", label, err)
	}
	r.rec.waitOpens(t, opens)
	time.Sleep(1 * time.Second)
	fl.expectNoBytes(t)
	return fl
}

// expectNoBytes asserts nothing crossed to the cloud yet: a local flow
// has no upstream leg, its c2s bytes are drained by the service.
func (fl *r3Flow) expectNoBytes(t *testing.T) {
	t.Helper()
	select {
	case n := <-fl.bytes:
		t.Fatalf("local flow: %d c2s bytes reached the cloud, want 0", n)
	default:
	}
}

// end tears down the flow deterministically INSIDE the bubble.
func (fl *r3Flow) end(t *testing.T) {
	t.Helper()
	fl.ended = true
	_ = fl.client.Close()
	_ = fl.upCloud.Close()
	<-fl.finished
	if err := <-fl.done; err != nil {
		t.Errorf("r3 flow: %v", err)
	}
	<-fl.upDrained
}

// TestR3EstablishmentTimeDecision (R3): Decide is consulted once per
// connection, at establishment — a Set mid-flow must not change how an
// established flow is served. Flow A, opened in mode off, keeps serving
// locally across a Set(full); flow B, opened after the Set, is mirrored.
func TestR3EstablishmentTimeDecision(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "r3.db"), "dev")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	machine := New(st, time.Now, policy.ModeOff)

	rec := newR3Recorder()
	pipeline := hooks.NewPipeline()
	pipeline.Register(rec)
	var svc *framesvc.Service
	inj := &r3Injector{p: &svc}
	gw := gateway.New(inj, time.Now)
	svc = framesvc.New(pipeline, st, gw,
		func() policy.Posture {
			return policy.Posture{Mode: policy.ModeFull}
		},
		time.Now, framesvc.WithServePolicy(machine))
	_ = svc

	rig := &r3Rig{rec: rec}
	rig.ladder = &proxy.Ladder{
		Sink:             func(string, string, int64) (*sink.Conn, error) { return sink.Noop(), nil },
		DataRoot:         "",
		MaxPeek:          sniff.MaxPeek,
		MaxBytes:         256 << 20,
		HandshakeTimeout: 30 * time.Second,
		Hooks:            pipeline,
		Capture:          capture.NewRegistry(),
		Dial: func(_ net.Addr) (net.Conn, error) {
			rig.dialMu.Lock()
			defer rig.dialMu.Unlock()
			if rig.dialTo == nil {
				return nil, errors.New("r3 test: no dial target set")
			}
			return *rig.dialTo, nil
		},
	}

	synctest.Test(t, func(t *testing.T) {
		// Flow A: opened in mode off (the machine's initial mode) —
		// served ServeLocal: the relay never dials, the c2s bytes are
		// drained by the service, nothing reaches the cloud.
		flA := rig.openLocalFlow(t, "A-first", 1)

		// A mid-flow Set must not re-decide the established flow.
		if err := machine.Set(policy.ModeFull, "r3"); err != nil {
			t.Fatalf("Set(full): %v", err)
		}
		if _, err := flA.client.Write([]byte("A-again")); err != nil {
			t.Fatalf("flow A write 2: %v", err)
		}
		time.Sleep(1 * time.Second)
		flA.expectNoBytes(t)

		// Flow B: opened after the Set — mirrored.
		flB := rig.openFlow(t, "B-first", 2)

		flA.end(t)
		flB.end(t)
	})

	st.Flush()
	idA, idB := rig.rec.idAt(t, 0), rig.rec.idAt(t, 1)
	wantA := idA + " local: forward-mode off: local"
	wantB := idB + " mirror: forward-mode full: mirror"
	rows, err := st.QueryAudit(time.Time{}, "serve")
	if err != nil {
		t.Fatalf("QueryAudit(serve): %v", err)
	}
	var gotA, gotB bool
	for _, r := range rows {
		switch r.Detail {
		case wantA:
			gotA = true
		case wantB:
			gotB = true
		}
	}
	if !gotA || !gotB {
		t.Fatalf("serve audit rows: want %q and %q, got %v", wantA, wantB, rows)
	}
}
