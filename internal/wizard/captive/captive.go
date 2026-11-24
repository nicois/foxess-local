// Package captive runs the hotspot's captive portal (design D-031,
// spike S1): the wizard's own dnsmasq, bound to the hotspot address
// only, answers every name with that address and hands out leases; a
// port-80 listener on the same address catches the phones' captive
// probes. Both run only while the hotspot is up.
package captive

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"sync"
	"time"

	"foxesslocal/internal/wizard/netmgr"
)

// DHCP pool on the hotspot.
const (
	poolStart = "10.42.0.10"
	poolEnd   = "10.42.0.200"
	lease     = "10m"
	leaseFile = "/run/foxess-wizard/dnsmasq.leases"
)

const (
	readHeaderTimeout = 10 * time.Second
	stopTimeout       = 5 * time.Second
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
	a := netmgr.HotspotAddr.String()
	stopDNS, err := p.Procs.Start(ctx, "dnsmasq",
		"--keep-in-foreground", "--conf-file=/dev/null", "--no-hosts", "--no-resolv",
		"--bind-interfaces", "--except-interface=lo", "--listen-address="+a,
		"--address=/#/"+a,
		"--dhcp-range="+poolStart+","+poolEnd+","+lease,
		"--dhcp-option=option:router,"+a, "--dhcp-option=option:dns-server,"+a,
		"--dhcp-leasefile="+leaseFile)
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
type Exec struct{}

// Start implements Procs.
func (Exec) Start(ctx context.Context, name string, args ...string) (func(), error) {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: the portal's fixed dnsmasq invocation
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	return func() {
		_ = cmd.Process.Signal(stopSignal)
		select {
		case <-done:
		case <-time.After(stopTimeout):
			_ = cmd.Process.Kill()
			<-done
		}
	}, nil
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
