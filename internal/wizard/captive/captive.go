// Package captive runs the hotspot's captive portal (design D-031,
// spike S1): the wizard's own dnsmasq, bound to the hotspot address
// only, answers every name with that address and hands out leases; a
// port-80 listener on the same address catches the phones' captive
// probes. Both run only while the hotspot is up.
package captive

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"foxesslocal/internal/wizard/netmgr"
)

// DHCP pool on the hotspot.
const (
	poolStart = "10.42.0.10"
	poolEnd   = "10.42.0.200"
	lease     = "10m"
)

// LeaseDir holds dnsmasq's lease file. The service unit creates it
// (RuntimeDirectory=); Up creates it too, since dnsmasq will not.
const LeaseDir = "/run/foxess-wizard"

const (
	readHeaderTimeout = 10 * time.Second
	stopTimeout       = 5 * time.Second
	// startGrace is how long a started process must keep running to
	// count as started: dnsmasq fails on its configuration in
	// milliseconds.
	startGrace = 500 * time.Millisecond
	// stderrCap bounds the stderr kept for an error message.
	stderrCap = 4096
	leaseMode = 0o700
)

// Procs starts a long-running process; the returned func stops it.
type Procs interface {
	Start(ctx context.Context, name string, args ...string) (func(), error)
}

// Listener serves h on addr; the returned func closes it.
type Listener interface {
	Serve(addr string, h http.Handler) (func(), error)
}

// Portal is the captive portal's lifecycle.
type Portal struct {
	Procs   Procs
	Listen  Listener
	Handler http.Handler // the wizard's handler, which redirects probes
	// LeaseDir is where dnsmasq keeps its leases; empty means LeaseDir.
	LeaseDir string

	mu   sync.Mutex
	stop []func()
}

// Up starts dnsmasq and the port-80 listener; a second Up is a no-op.
func (p *Portal) Up(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stop != nil {
		return nil
	}
	dir := p.LeaseDir
	if dir == "" {
		dir = LeaseDir
	}
	if err := os.MkdirAll(dir, leaseMode); err != nil {
		return fmt.Errorf("captive: lease dir: %w", err)
	}
	a := netmgr.HotspotAddr.String()
	stopDNS, err := p.Procs.Start(ctx, "dnsmasq",
		"--keep-in-foreground", "--conf-file=/dev/null", "--no-hosts", "--no-resolv",
		"--bind-interfaces", "--except-interface=lo", "--listen-address="+a,
		"--address=/#/"+a,
		"--dhcp-range="+poolStart+","+poolEnd+","+lease,
		"--dhcp-option=option:router,"+a, "--dhcp-option=option:dns-server,"+a,
		"--dhcp-leasefile="+filepath.Join(dir, "dnsmasq.leases"))
	if err != nil {
		return fmt.Errorf("captive: dnsmasq: %w", err)
	}
	stopHTTP, err := p.Listen.Serve(net.JoinHostPort(a, "80"), p.Handler)
	if err != nil {
		stopDNS()
		return fmt.Errorf("captive: port 80: %w", err)
	}
	p.stop = []func(){stopHTTP, stopDNS}
	return nil
}

// Down stops both; a second Down is a no-op.
func (p *Portal) Down() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, f := range p.stop {
		f()
	}
	p.stop = nil
}

// Exec starts real processes, stopped with a signal and waited for.
// A process that exits within its start-up grace is an error carrying
// its stderr; one that dies on its own later is logged.
type Exec struct {
	Log func(msg string, args ...any) // optional

	grace func() <-chan time.Time // tests; nil means startGrace
}

// Start implements Procs.
func (e Exec) Start(ctx context.Context, name string, args ...string) (func(), error) {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: the portal's fixed dnsmasq invocation
	stderr := &capped{max: stderrCap}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	var (
		mu       sync.Mutex
		started  bool // past the grace: a later exit is logged
		stopping bool // Stop asked for the exit: not logged
		exited   bool
		waitErr  error
	)
	done := make(chan struct{})
	go func() {
		err := cmd.Wait()
		mu.Lock()
		exited, waitErr = true, err
		report := started && !stopping
		mu.Unlock()
		close(done)
		if report && e.Log != nil {
			e.Log("process exited", "name", name, "err", err, "stderr", stderr.String())
		}
	}()
	grace := e.grace
	if grace == nil {
		grace = func() <-chan time.Time { return time.After(startGrace) }
	}
	select {
	case <-done:
	case <-grace():
	}
	mu.Lock()
	if exited {
		err := waitErr
		mu.Unlock()
		if err == nil {
			err = errors.New("exited at start-up")
		}
		return nil, fmt.Errorf("%s: %w: %s", name, err, stderr.String())
	}
	started = true
	mu.Unlock()
	return func() {
		mu.Lock()
		stopping = true
		mu.Unlock()
		_ = cmd.Process.Signal(stopSignal)
		select {
		case <-done:
		case <-time.After(stopTimeout):
			_ = cmd.Process.Kill()
			<-done
		}
	}, nil
}

// capped keeps the first max bytes written to it.
type capped struct {
	mu  sync.Mutex
	max int
	b   bytes.Buffer
}

func (c *capped) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if room := c.max - c.b.Len(); room > 0 {
		c.b.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (c *capped) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.TrimSpace(c.b.String())
}

// HTTP serves on a real listener.
type HTTP struct{}

// Serve implements Listener.
func (HTTP) Serve(addr string, h http.Handler) (func(), error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: readHeaderTimeout}
	// Serve ends when Down closes it, or when the hotspot address goes
	// away under it (Down follows)
	go func() { _ = srv.Serve(ln) }()
	return func() { _ = srv.Close() }, nil
}
