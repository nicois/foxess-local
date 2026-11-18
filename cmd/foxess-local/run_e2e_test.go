package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/nicois/prox/pkg/chanpipe"
	pconfig "github.com/nicois/prox/pkg/config"
	prox "github.com/nicois/prox/pkg/prox"
	"github.com/stretchr/testify/require"

	"foxesslocal/internal/api"
	"foxesslocal/internal/config"
	"foxesslocal/internal/store"
)

// runCmdTestConfig writes a runCmd config on free loopback ports with
// the installer token seeded (the token gate fails fast without one)
// and no cert manifest — first start must self-bootstrap the T2 anchor
// (the live first-start path). Returns the config path and HTTP port.
func runCmdTestConfig(t *testing.T) (string, int) {
	t.Helper()
	// Free loopback ports the client can target ahead of time (the
	// inverter leg rejects :0 — TPROXY target semantics).
	freePort := func() int {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		addr, ok := ln.Addr().(*net.TCPAddr)
		require.True(t, ok)
		port := addr.Port
		require.NoError(t, ln.Close())
		return port
	}
	port, invPort := freePort(), freePort()
	dir := t.TempDir()
	seed, err := api.NewTokenStore(filepath.Join(dir, "tokens.json"), time.Now)
	require.NoError(t, err)
	_, _, err = seed.Create("installer", api.RoleInstaller)
	require.NoError(t, err)
	cfgPath := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(fmt.Sprintf(`state_dir = %q

[server]
inverter_listen = "127.0.0.1:%d"
http_listen = "127.0.0.1:%d"

[forward]
mode = "off"
`, dir, invPort, port)), 0o600))
	return cfgPath, port
}

// stubListener substitutes the inverter listener's run function for
// one test (restored on cleanup).
func stubListener(t *testing.T, run func(context.Context) error) {
	t.Helper()
	orig := runListener
	runListener = func(*handle) func(context.Context) error { return run }
	t.Cleanup(func() { runListener = orig })
}

// TestRunCmdExitsWhenListenerFails (DEF-009): an inverter listener that
// cannot start (here: EPERM, as for a missing CAP_NET_ADMIN) ends runCmd
// with a non-zero exit — systemd sees the failure and restarts — instead
// of leaving HTTP up with the inverter leg silently dead.
func TestRunCmdExitsWhenListenerFails(t *testing.T) {
	stubListener(t, func(context.Context) error {
		return fmt.Errorf("prox: listen: IP_TRANSPARENT: %w", syscall.EPERM)
	})
	cfgPath, _ := runCmdTestConfig(t)
	code := make(chan int, 1)
	go func() { code <- runCmd([]string{flagConfig, cfgPath}) }()
	select {
	case c := <-code:
		require.Equal(t, exitRunFailed, c)
	case <-time.After(30 * time.Second):
		t.Fatal("runCmd stayed up with a dead inverter listener")
	}
}

// TestRunCmdServesHTTP guards the production wiring: runCmd must
// actually serve the REST surface, not just bind the listener. The
// handler-level tests (smoke, ui) drive h.srv.Handler directly and
// cannot catch a missing Serve call (the live symptom: the socket
// accepts TCP but never answers — browsers see "connection aborted").
// This one dials the real accept loop over loopback against the full
// embedded-prox stack: temp paths, installer token seeded, forward
// mode off (nothing beyond loopback).
func TestRunCmdServesHTTP(t *testing.T) {
	// The inverter listener needs IP_TRANSPARENT (CAP_NET_ADMIN), which
	// an unprivileged test lacks — and a listener failure is fatal
	// (DEF-009). Stand in a listener that serves until shutdown.
	stubListener(t, func(ctx context.Context) error { <-ctx.Done(); return nil })
	cfgPath, port := runCmdTestConfig(t)

	code := make(chan int, 1)
	go func() { code <- runCmd([]string{flagConfig, cfgPath}) }()

	// The REST surface must answer over real TCP (the regression:
	// listener bound, nothing accepting). 15 s covers the first-start
	// T2 anchor bootstrap on a cold temp dir.
	healthz := fmt.Sprintf("http://127.0.0.1:%d/healthz", port)
	var got int
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(healthz) //nolint:gosec // loopback test URL
		if err == nil {
			got = resp.StatusCode
			require.NoError(t, resp.Body.Close())
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	require.Equal(t, http.StatusOK, got, "runCmd must serve /healthz over TCP")

	// Graceful-shutdown path: SIGTERM ends runCmd with exit 0.
	killed := false
	t.Cleanup(func() {
		if !killed {
			if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
				t.Errorf("shutdown signal: %v", err)
			}
			<-code
		}
	})
	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGTERM))
	killed = true
	require.Equal(t, 0, <-code)
}

// def007Flow is one scripted inverter session of the DEF-007 test:
// the client-side TLS conn plus its serve goroutine's result.
type def007Flow struct {
	tc     *tls.Conn
	srvErr chan error
}

// def007Dial opens one scripted inverter session on in-memory legs
// (the smoke's pattern); withTele also sends the 163 B telemetry
// frame that gives the stream channel bytes.
func def007Dial(t *testing.T, p *prox.Prox, src, dst *net.TCPAddr, withTele bool) def007Flow {
	t.Helper()
	relay, client := chanpipe.New()
	srvErr := make(chan error, 1)
	go func() { srvErr <- p.Ladder.Serve(smokeTestAddr{Conn: relay, remote: src}, dst) }()
	//nolint:gosec // G402: scripted endpoint, the trust anchor is the test's own CA
	tc := tls.Client(client, &tls.Config{ServerName: "203.0.116.72", InsecureSkipVerify: true})
	require.NoError(t, tc.Handshake())
	if withTele {
		_, err := tc.Write(smokeFrame(smokeStartC2S, smokeDevInv, smokeFnData,
			smokeTelePayload(1200, 400, 800, 0, 0xfffffc18, 31)))
		require.NoError(t, err)
	}
	return def007Flow{tc, srvErr}
}

// def007Quiesce ends the given flows and waits for their serve
// goroutines to return (the production-settle equivalent: prox's
// settle phase runs every serve goroutine — including the hook
// open/close that audit the store — to completion before the store
// closes).
func def007Quiesce(t *testing.T, fs []def007Flow) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		for i := range fs {
			_ = fs[i].tc.Close()
			if err := <-fs[i].srvErr; err != nil {
				t.Logf("flow %d ended: %v (expected: the connection was just closed)", i, err)
			}
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("the serve goroutines did not return")
	}
}

// def007Round runs one round of the DEF-007 test: 126 telemetry
// sessions plus 2 holdouts across one handle lifecycle (run → Start →
// quiesce the data flows → Shutdown while the holdouts are still open →
// cancel). It returns the round's run-ctx cancel and store so the test
// can track both for cleanup.
func def007Round(t *testing.T, cfg *config.Config, pcfg *pconfig.Config, src, dst *net.TCPAddr) (context.CancelFunc, *store.Store) {
	t.Helper()
	// A fresh prox per round — the production shape is one prox per
	// process: framesvc registers on the ladder's hook pipeline
	// (append), so reusing one prox across runs would chain the
	// previous rounds' services into every later flow's OnOpen
	// against already-closed stores.
	p, err := prox.New(pcfg)
	require.NoError(t, err)
	h, err := run(cfg, withProx(p))
	require.NoError(t, err)
	rctx, rcancel := context.WithCancel(context.Background())
	h.Start(rctx)
	h.ServeHTTP()

	// 126 inverter sessions carrying telemetry plus 2 holdouts
	// (handshake only): the data frames give each data stream channel
	// bytes (chunk rows at stream end); the holdouts stay open across
	// the shutdown.
	const nData = 126
	flows := make([]def007Flow, 0, nData+2)
	for i := range nData + 2 {
		flows = append(flows, def007Dial(t, p, src, dst, i < nData))
	}
	time.Sleep(100 * time.Millisecond) // the telemetry bytes reach the capture

	// The data flows end before the shutdown (the production-settle
	// equivalent); the holdouts stay open.
	def007Quiesce(t, flows[:nData])

	// The DEF-007 window: h.Shutdown runs to the store's Close
	// (pre-fix) or blocks in WaitStreams (post-fix) while the holdouts
	// are still open. They close during the settle, so their streams
	// end — and their persistStreams record — after the pre-fix Close:
	// pre-fix that panics (the live 10:54:32 signature) and the test
	// binary dies; the post-fix wait covers it deterministically.
	shutdownErr := make(chan error, 1)
	go func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dcancel()
		shutdownErr <- h.Shutdown(dctx)
	}()
	time.Sleep(100 * time.Millisecond) // the pre-fix Close is done; the post-fix wait is live
	for i := nData; i < len(flows); i++ {
		_ = flows[i].tc.Close()
	}
	select {
	case err := <-shutdownErr:
		require.NoError(t, err, "shutdown")
	case <-time.After(10 * time.Second):
		t.Fatalf("shutdown did not complete")
	}
	return rcancel, h.st
}

// def007Setup builds the round configuration: a temp dir with a smoke
// CA (the ladder's signing pair), an empty cert manifest (no
// bootstrap), a seeded installer token, and a forward-off config on a
// free loopback port. It returns the loaded config, the prox config
// (shared, read-only after creation) and the temp dir (the store's
// location).
func def007Setup(t *testing.T) (*config.Config, *pconfig.Config, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr, ok := ln.Addr().(*net.TCPAddr)
	require.True(t, ok)
	port := addr.Port
	require.NoError(t, ln.Close())

	dir := t.TempDir()

	certPEM, keyPEM := smokeCA(t)
	caDir := filepath.Join(dir, "ca")
	require.NoError(t, os.MkdirAll(caDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(caDir, "ca.pem"), certPEM, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(caDir, "ca-key.pem"), keyPEM, 0o600))

	pcfg := pconfig.Default()
	pcfg.Listen = "127.0.0.1:0" // the ladder is driven directly (no TPROXY)
	pcfg.Ports = []int{14431}
	pcfg.DataRoot = "" // embedded: the foxess store owns persistence
	pcfg.CAPaths = []pconfig.CAPath{{
		CertPEM: filepath.Join(caDir, "ca.pem"),
		KeyPEM:  filepath.Join(caDir, "ca-key.pem"),
	}}

	// Empty cert manifest: no bootstrap on first start (same as the
	// smoke — the ladder's CA comes from pcfg.CAPaths).
	manifest := filepath.Join(dir, "cert-manifest.json")
	require.NoError(t, os.WriteFile(manifest, []byte("{}"), 0o600))

	tokensPath := filepath.Join(dir, "tokens.json")
	seed, err := api.NewTokenStore(tokensPath, time.Now)
	require.NoError(t, err)
	_, _, err = seed.Create("installer", api.RoleInstaller)
	require.NoError(t, err)

	cfgPath := filepath.Join(dir, "config.toml")
	cfgTOML := fmt.Sprintf(`
[server]
inverter_listen = "127.0.0.1:0"
http_listen = "127.0.0.1:%d"

[certs]
manifest = %q

[store]
path = %q

[forward]
mode = "off"

[retention]
chunk_bytes_hours = 1

[tokens]
path = %q
`, port, manifest, filepath.Join(dir, "foxess.db"), tokensPath)
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfgTOML), 0o600))
	cfg, err := config.Load(cfgPath)
	require.NoError(t, err)
	return cfg, pcfg, dir
}

// TestShutdownQuiescesCaptureStreams guards the DEF-007 shutdown
// ordering (live panic 2026-11-15 10:54:32, systemctl stop with a live
// inverter session: "panic: store: RecordChunk after Close"): with
// chunk byte capture on and live channel flows ending at the moment of
// shutdown, each stream's persistStream goroutine races the store's
// Close, and Record* after Close panics by the store's contract. The
// host must wait for the finished streams (st.WaitStreams) before
// closing the store.
//
// The no-TPROXY harness (the smoke's pattern: the ladder driven
// directly on chanpipe legs) keeps the test runnable without
// CAP_NET_ADMIN. Each round: 126 handshaken TLS sessions send one
// telemetry frame and idle; 2 holdouts only handshake and stay open
// across the shutdown. The data flows end and their serve goroutines
// return before h.Shutdown (the production-settle equivalent: prox's
// settle runs every serve goroutine — including the hook open/close
// that audits the store — to completion before the store closes). The
// holdouts close during the 100 ms settle after Shutdown starts, so
// their capture streams end — and their persistStream goroutines
// record — after the pre-fix Close: the deterministic red, and
// exactly what the post-fix WaitStreams must cover. Telemetry frames
// (no 2a registration) keep the sessions open without arming the
// delayed bootstrap trio, whose write-error path would touch the
// store after Close. Three rounds: each is deterministic, and the
// repeat exercises the fresh-prox re-registration.
func TestShutdownQuiescesCaptureStreams(t *testing.T) {
	cfg, pcfg, dir := def007Setup(t)

	src := &net.TCPAddr{IP: net.ParseIP("192.168.4.20"), Port: 44723}
	dst := &net.TCPAddr{IP: net.ParseIP("203.0.116.72"), Port: 14431}

	var cancels []context.CancelFunc
	var sts []*store.Store
	t.Cleanup(func() {
		for _, c := range cancels {
			c()
		}
		for _, s := range sts {
			_ = s.Close() // idempotent: rounds that shut down closed it
		}
	})

	for range 3 {
		rcancel, st := def007Round(t, cfg, pcfg, src, dst)
		cancels = append(cancels, rcancel)
		sts = append(sts, st)
	}

	// The captured bytes survived the shutdowns: the live streams'
	// chunk rows are persisted (the data-integrity half of DEF-007 —
	// the streams recorded before Close, not dropped by the race).
	s, err := store.Open(filepath.Join(dir, "foxess.db"), "test")
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	nb, err := s.ChunkPayloadCount()
	require.NoError(t, err)
	require.Positive(t, nb, "the live capture streams' chunks must be persisted")
}
