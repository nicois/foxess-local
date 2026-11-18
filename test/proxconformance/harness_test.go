// Package proxconformance is the conformance suite that pins
// foxesslocal's adoption of prox (the embedded interception substrate) to a
// specific prox commit. It drives the real prox relay — the pkg/proxy
// Ladder with the pkg/chanpipe in-memory transport — and asserts each
// adoption requirement R1–R9 of docs/positions/prox-interface.md against
// the pinned version, so "prox will comply" is a version pin with evidence,
// not an assumption.
//
// Pinned prox: internal/proxpin (the v0.1.0 publish tree, squashed from
// 8d68f0e, whose range since the previous pin implements the gap-clarification items:
// consumer-owned TPROXY rules mode (R13), opt-in byte payload on capture
// records (R6a), chunk length on hook drop, s2c cap losses recorded on
// the capture stream). R3 (per-flow serve mode) and R7b
// (certificate-selection callback) remain tracked skips — their check
// bodies are owed now that Phase 2 is in the pin. See README.md for the
// results table and the upgrade procedure.
//
// Drive level: every check runs the relay at its ladder seam
// (Ladder.Serve on the client leg + Ladder.Dial for the scripted upstream),
// which is the same seam the TPROXY facade (pkg/prox) uses per connection.
// The facade's Run/Shutdown itself needs CAP_NET_ADMIN (nftables + a
// transparent listener) and is not drivable in an unprivileged test, so the
// R8 drain check covers the ladder seam. The client half of the four-tuple
// is presented through addrShim: the ladder derives hooks.Conn.Src/SrcPort
// from conn.RemoteAddr() exactly as on a real TPROXY socket, and chanpipe's
// net.Conn carries no address of its own.
package proxconformance

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nicois/prox/pkg/capture"
	"github.com/nicois/prox/pkg/chanpipe"
	"github.com/nicois/prox/pkg/config"
	"github.com/nicois/prox/pkg/hooks"
	"github.com/nicois/prox/pkg/proxy"
	"github.com/nicois/prox/pkg/sink"
	"github.com/nicois/prox/pkg/sniff"
	"github.com/nicois/prox/pkg/tlsutil"

	"foxesslocal/internal/proxpin"
)

// Pinned prox: the version the build compiles (internal/proxpin, read
// from the build info = go.mod's requirement). The tracked skips name the
// short form in their skip reason.
var pinnedShort = proxpin.Short()

// The four-tuple the checks assert on: an inverter-style client dialing a
// cloud endpoint on 14431 by raw IP (the foxesslocal deployment shape).
// failoverIP stands in for one of the Aliyun failover-set IPs the
// inverter dials as well (TEST-NET-2 range).
const (
	clientIP   = "192.168.4.20"
	clientPort = 44723
	dstIP      = "203.0.116.72"
	dstPort    = 14431
	failoverIP = "198.51.100.7"
)

// newLadder builds a ladder wired exactly like the prox facade wires it in
// embedded mode (DataRoot == ""): no-op sink (no file capture), no store.
// The test provides the scripted upstream through l.Dial.
func newLadder(t *testing.T, reg *capture.Registry, hs ...hooks.Hook) *proxy.Ladder {
	t.Helper()
	p := hooks.NewPipeline()
	for _, h := range hs {
		p.Register(h)
	}
	l := &proxy.Ladder{
		Sink:             func(string, string, int64) (*sink.Conn, error) { return sink.Noop(), nil },
		DataRoot:         "",
		MaxPeek:          sniff.MaxPeek,
		MaxBytes:         256 << 20,
		HandshakeTimeout: 30 * time.Second,
		Hooks:            p,
	}
	if reg != nil {
		l.Capture = reg
	}
	return l
}

// addrShim presents a declared client address on the in-memory client leg.
// The ladder derives hooks.Conn.Src/SrcPort from conn.RemoteAddr() the way
// it does on a real TPROXY socket; chanpipe's net.Conn returns an empty
// address, so the shim supplies the tuple the checks assert on. It changes
// nothing else: all bytes flow through the untouched chanpipe.
type addrShim struct {
	net.Conn
	remote net.Addr
}

func (a *addrShim) RemoteAddr() net.Addr { return a.remote }

// clientAddr is the client half of the test four-tuple.
func clientAddr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP(clientIP), Port: clientPort}
}

// dstAddr is the original-destination half of the test four-tuple.
func dstAddr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP(dstIP), Port: dstPort}
}

// recorder observes every chunk of every matched connection: per
// connection ID and direction it stores the exact chunks in arrival order,
// plus per-hook open/close signals on buffered channels the test reads with
// exact counts (deterministic synchronization, no timers).
type recorder struct {
	mu         sync.Mutex
	conns      map[string]*hooks.Conn
	chunks     map[string]map[hooks.Dir][][]byte
	opened     chan struct{}
	closed     chan string
	opensSeen  int // waitOpens is cumulative: n = total opens expected
	closesSeen int // waitCloses is cumulative: n = total closes expected
}

func newRecorder() *recorder {
	return &recorder{
		conns:  map[string]*hooks.Conn{},
		chunks: map[string]map[hooks.Dir][][]byte{},
		opened: make(chan struct{}, 64),
		closed: make(chan string, 64),
	}
}

func (r *recorder) Name() string { return "recorder" }

func (r *recorder) Filter() hooks.Match { return hooks.Match{} }

func (r *recorder) OnOpen(c *hooks.Conn, _ *hooks.HookConn) hooks.Result {
	r.mu.Lock()
	r.conns[c.ID] = c
	r.chunks[c.ID] = map[hooks.Dir][][]byte{}
	r.mu.Unlock()
	r.opened <- struct{}{}
	return hooks.Result{}
}

func (r *recorder) OnData(c *hooks.Conn, dir hooks.Dir, data []byte) hooks.Result {
	r.mu.Lock()
	r.chunks[c.ID][dir] = append(r.chunks[c.ID][dir], append([]byte(nil), data...))
	r.mu.Unlock()
	return hooks.Result{}
}

func (r *recorder) OnClose(_ *hooks.Conn, reason string) { r.closed <- reason }

// waitOpens blocks until a total of n connections opened.
func (r *recorder) waitOpens(t *testing.T, n int) {
	t.Helper()
	for r.opensSeen < n {
		<-r.opened
		r.opensSeen++
	}
}

// waitCloses blocks until a total of n connections closed and returns the
// reasons.
func (r *recorder) waitCloses(t *testing.T, n int) []string {
	t.Helper()
	out := make([]string, 0, n)
	for r.closesSeen < n {
		out = append(out, <-r.closed)
		r.closesSeen++
	}
	return out
}

// soleID returns the ID of the single recorded connection.
func (r *recorder) soleID(t *testing.T) string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.conns) != 1 {
		t.Fatalf("expected 1 connection, got %d", len(r.conns))
	}
	for id := range r.conns {
		return id
	}
	t.Fatal("unreachable")
	return ""
}

// conn returns the hooks.Conn recorded for id.
func (r *recorder) conn(t *testing.T, id string) *hooks.Conn {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.conns[id]
	if !ok {
		t.Fatalf("no connection %q recorded", id)
	}
	return c
}

// concat returns the per-direction bytes the hook saw, in chunk order.
func (r *recorder) concat(t *testing.T, id string, dir hooks.Dir) []byte {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []byte
	for _, c := range r.chunks[id][dir] {
		out = append(out, c...)
	}
	return out
}

// chunkCount returns how many chunks the hook saw in one direction.
func (r *recorder) chunkCount(t *testing.T, id string, dir hooks.Dir) int {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.chunks[id][dir])
}

// count tracks how many chunks and bytes one hook saw per connection and
// direction.
type count struct{ chunks, bytes int }

// policy is a scriptable decision hook: the test supplies a per-chunk
// decider (transform / drop / pass). It counts what it saw per connection
// ID and direction, so chain-order and short-circuit checks can assert
// exactly what each link of the chain observed.
type policy struct {
	name    string
	decider func(c *hooks.Conn, dir hooks.Dir, data []byte) hooks.Result
	// onOpen, if set, is called with the open time (synctest flows may
	// anchor a virtual window to it).
	onOpen func(now time.Time)

	mu     sync.Mutex
	seen   map[string]map[hooks.Dir]*count
	closed chan string
}

func newPolicy(name string, decider func(c *hooks.Conn, dir hooks.Dir, data []byte) hooks.Result) *policy {
	return &policy{
		name:    name,
		decider: decider,
		seen:    map[string]map[hooks.Dir]*count{},
		closed:  make(chan string, 64),
	}
}

func (p *policy) Name() string { return p.name }

func (p *policy) Filter() hooks.Match { return hooks.Match{} }

func (p *policy) OnOpen(c *hooks.Conn, _ *hooks.HookConn) hooks.Result {
	p.mu.Lock()
	p.seen[c.ID] = map[hooks.Dir]*count{}
	p.mu.Unlock()
	if p.onOpen != nil {
		p.onOpen(time.Now())
	}
	return hooks.Result{}
}

func (p *policy) OnData(c *hooks.Conn, dir hooks.Dir, data []byte) hooks.Result {
	p.mu.Lock()
	per := p.seen[c.ID]
	if per[dir] == nil {
		per[dir] = &count{}
	}
	per[dir].chunks++
	per[dir].bytes += len(data)
	p.mu.Unlock()
	if p.decider == nil {
		return hooks.Result{}
	}
	return p.decider(c, dir, data)
}

func (p *policy) OnClose(_ *hooks.Conn, reason string) { p.closed <- reason }

// saw reports how many chunks/bytes this hook saw for id in dir.
func (p *policy) saw(id string, dir hooks.Dir) (chunks, bytes int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if per, ok := p.seen[id][dir]; ok {
		return per.chunks, per.bytes
	}
	return 0, 0
}

// injector writes the scripted payload on one leg at an arbitrary moment —
// the test closes the trigger channel, which happens after the relay is
// ready and is not tied to any received byte (R2c).
type injector struct {
	name    string
	dir     hooks.Dir
	payload []byte
	trigger chan struct{}

	exited chan struct{}
}

func newInjector(name string, dir hooks.Dir, payload []byte) *injector {
	return &injector{
		name:    name,
		dir:     dir,
		payload: payload,
		trigger: make(chan struct{}),
		exited:  make(chan struct{}),
	}
}

func (j *injector) Name() string { return j.name }

func (j *injector) Filter() hooks.Match { return hooks.Match{} }

func (j *injector) OnOpen(_ *hooks.Conn, hc *hooks.HookConn) hooks.Result {
	go func() {
		<-hc.Ready()
		<-j.trigger
		var err error
		if j.dir == hooks.DirS2C {
			_, err = hc.WriteS2C(j.payload)
		} else {
			_, err = hc.WriteC2S(j.payload)
		}
		if err == nil {
			// Wait for the relay to seal the connection before exiting,
			// so the injected bytes are in flight when the flow ends.
			<-hc.Done()
		}
		close(j.exited)
	}()
	return hooks.Result{}
}

func (j *injector) OnData(_ *hooks.Conn, _ hooks.Dir, _ []byte) hooks.Result {
	return hooks.Result{}
}

func (j *injector) OnClose(_ *hooks.Conn, _ string) {}

// fire releases the injector at this (arbitrary) moment. The injected
// bytes land on the leg independently of any received byte; the test
// reads j.exited after the flow ends to confirm the goroutine exited.
func (j *injector) fire() { close(j.trigger) }

// tlsObserver watches the client-leg TLS outcome (hooks.TLSObserver).
type tlsObserver struct {
	results chan capture.TLSInfo
}

func newTLSObserver() *tlsObserver {
	return &tlsObserver{results: make(chan capture.TLSInfo, 2)}
}

func (o *tlsObserver) Name() string { return "tlsObserver" }

func (o *tlsObserver) Filter() hooks.Match { return hooks.Match{} }

func (o *tlsObserver) OnOpen(_ *hooks.Conn, _ *hooks.HookConn) hooks.Result {
	return hooks.Result{}
}

func (o *tlsObserver) OnData(_ *hooks.Conn, _ hooks.Dir, _ []byte) hooks.Result {
	return hooks.Result{}
}

func (o *tlsObserver) OnClose(_ *hooks.Conn, _ string) {}

func (o *tlsObserver) OnTLSResult(_ *hooks.Conn, r capture.TLSInfo) {
	o.results <- r
}

// testCA is a throwaway self-signed certificate authority.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newTestCA(t *testing.T, cn string) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(48 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CA cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("CA parse: %v", err)
	}
	return &testCA{cert: cert, key: key}
}

// pool returns an x509 pool trusting only this CA.
func (c *testCA) pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(c.cert)
	return p
}

// leafForHost returns a server leaf signed by this CA; host is an IP
// literal (iPAddress SAN) or a name (dNSName SAN).
func (c *testCA) leafForHost(t *testing.T, cn, host string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("leaf key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(48 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		t.Fatalf("leaf cert: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// caFiles writes the CA as the PEM file pair config.CAPath expects and
// returns the paths.
func (c *testCA) caFiles(t *testing.T, dir string) (certPEM, keyPEM string) {
	t.Helper()
	certPEM = filepath.Join(dir, "ca-cert.pem")
	keyPEM = filepath.Join(dir, "ca-key.pem")
	certOut := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.cert.Raw})
	keyDER, err := x509.MarshalECPrivateKey(c.key)
	if err != nil {
		t.Fatalf("CA key marshal: %v", err)
	}
	keyOut := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(certPEM, certOut, 0o600); err != nil {
		t.Fatalf("write CA cert: %v", err)
	}
	if err := os.WriteFile(keyPEM, keyOut, 0o600); err != nil {
		t.Fatalf("write CA key: %v", err)
	}
	return certPEM, keyPEM
}

// newLeafProvider builds the ladder's leaf minter from a CA (the
// certgen-style file form the facade uses).
func newLeafProvider(t *testing.T, ca *testCA) *tlsutil.Provider {
	t.Helper()
	dir := t.TempDir()
	certPEM, keyPEM := ca.caFiles(t, dir)
	p, err := tlsutil.NewProvider([]config.CAPath{{CertPEM: certPEM, KeyPEM: keyPEM}}, config.MITM{
		ValidityDays: 365,
	})
	if err != nil {
		t.Fatalf("leaf provider: %v", err)
	}
	return p
}

// testSNI is the SNI the SNI-carrying flows use. It is a hostname, because
// Go's stdlib client suppresses the SNI extension for IP-literal
// ServerNames (crypto/tls hostnameInSNI returns "" for parseable IPs);
// sniff's SNI parse is byte-generic, so a hostname exercises exactly the
// SNI plumbing an IP-literal SNI would. The IP-literal shape (what the
// real inverter, an OpenSSL client, sends) is covered at the minting seam
// in TestR7a.
const testSNI = "inverter-cloud.example"

// tlsHarness is a full MITM test rig: a ladder with a leaf minter and
// upstream trust pool, the client-side TLS config that trusts the ladder's
// CA, and the scripted upstream's server leaf (SAN matching sni).
type tlsHarness struct {
	ladder      *proxy.Ladder
	client      *tls.Config     // for the client leg (trusts the ladder CA)
	upLeaf      tls.Certificate // the scripted upstream's server leaf
	clientEnd   net.Conn        // the end the test drives as the inverter
	serverEnd   net.Conn        // the end handed to Ladder.Serve
	upServerEnd net.Conn        // the end the test drives as the cloud
}

// newTLSHarness builds the rig for the given SNI (a hostname or an IP
// literal).
func newTLSHarness(t *testing.T, reg *capture.Registry, sni string, hs ...hooks.Hook) *tlsHarness {
	t.Helper()
	ladderCA := newTestCA(t, "conformance-ladder-ca")
	upstreamCA := newTestCA(t, "conformance-upstream-ca")

	l := newLadder(t, reg, hs...)
	l.Leaves = newLeafProvider(t, ladderCA)
	l.UpstreamRoots = upstreamCA.pool()

	serverEnd, clientEnd := chanpipe.New()
	upClientEnd, upServerEnd := chanpipe.New()
	l.Dial = func(net.Addr) (net.Conn, error) { return upClientEnd, nil }

	return &tlsHarness{
		ladder:      l,
		client:      &tls.Config{ServerName: sni, RootCAs: ladderCA.pool()},
		upLeaf:      upstreamCA.leafForHost(t, "conformance-upstream", sni),
		clientEnd:   clientEnd,
		serverEnd:   serverEnd,
		upServerEnd: upServerEnd,
	}
}

// rawFlow is one in-memory raw-TCP flow through the ladder.
type rawFlow struct {
	client   net.Conn // the end the test drives as the inverter
	upstream net.Conn // the end the test drives as the cloud
	done     chan error
	finished chan struct{} // closed once Serve has returned
}

// startFlowOn starts one raw flow on l. The test owns the upstream pipe
// pair: l.Dial hands the relay the client end, startFlowOn gets the server
// end (the leg the relay dials). The legs are torn down in t.Cleanup.
func startFlowOn(t *testing.T, l *proxy.Ladder, upServerEnd net.Conn) *rawFlow {
	t.Helper()
	serverEnd, clientEnd := chanpipe.New()
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		err := l.Serve(&addrShim{Conn: serverEnd, remote: clientAddr()}, dstAddr())
		done <- err
		close(finished)
	}()
	f := &rawFlow{client: clientEnd, upstream: upServerEnd, done: done, finished: finished}
	t.Cleanup(func() {
		_ = clientEnd.Close()
		_ = upServerEnd.Close()
		<-f.finished
	})
	return f
}

// startRawFlow starts a fresh embedded-mode ladder plus one flow on it.
func startRawFlow(t *testing.T, reg *capture.Registry, hs ...hooks.Hook) *rawFlow {
	t.Helper()
	l := newLadder(t, reg, hs...)
	upClientEnd, upServerEnd := chanpipe.New()
	l.Dial = func(net.Addr) (net.Conn, error) { return upClientEnd, nil }
	return startFlowOn(t, l, upServerEnd)
}

// runTLSFlow drives one full MITM flow over h: the client (inverter side)
// sends c2s and reads s2c; the scripted upstream reads c2s and sends s2c.
// It returns the client's TLS connection state (the forged leaf is in
// state.PeerCertificates) and the ladder's Serve error after the flow has
// ended.
func runTLSFlow(t *testing.T, h *tlsHarness, c2s, s2c string) (tls.ConnectionState, error) {
	t.Helper()
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- h.ladder.Serve(&addrShim{Conn: h.serverEnd, remote: clientAddr()}, dstAddr())
	}()
	upErr := make(chan error, 1)
	go func() {
		if e := driveUpstreamTLS(t, h, c2s, s2c); e != nil {
			upErr <- e
		} else {
			upErr <- nil
		}
	}()
	state, e := driveClientTLS(t, h, c2s, s2c)
	if e != nil {
		return state, e
	}
	if e := <-upErr; e != nil {
		return state, e
	}
	return state, <-serveErr
}

// driveUpstreamTLS is the scripted cloud side of a runTLSFlow exchange.
func driveUpstreamTLS(t *testing.T, h *tlsHarness, c2s, s2c string) error {
	t.Helper()
	up := tls.Server(h.upServerEnd, &tls.Config{Certificates: []tls.Certificate{h.upLeaf}})
	defer func() { _ = up.Close() }()
	if err := up.Handshake(); err != nil {
		return fmt.Errorf("upstream handshake: %w", err)
	}
	buf := make([]byte, len(c2s))
	if _, err := io.ReadFull(up, buf); err != nil {
		return err
	}
	if string(buf) != c2s {
		return fmt.Errorf("upstream got %q, want %q", buf, c2s)
	}
	if _, err := up.Write([]byte(s2c)); err != nil {
		return err
	}
	return nil
}

// driveClientTLS is the inverter side of a runTLSFlow exchange.
func driveClientTLS(t *testing.T, h *tlsHarness, c2s, s2c string) (tls.ConnectionState, error) {
	t.Helper()
	cli := tls.Client(h.clientEnd, h.client)
	defer func() { _ = cli.Close() }()
	if err := cli.Handshake(); err != nil {
		return tls.ConnectionState{}, fmt.Errorf("client handshake: %w", err)
	}
	state := cli.ConnectionState()
	if _, err := cli.Write([]byte(c2s)); err != nil {
		return state, err
	}
	buf := make([]byte, len(s2c))
	if _, err := io.ReadFull(cli, buf); err != nil {
		return state, err
	}
	if string(buf) != s2c {
		return state, fmt.Errorf("client got %q, want %q", buf, s2c)
	}
	return state, nil
}
