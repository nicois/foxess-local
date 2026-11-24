package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"foxesslocal/internal/wizard/bootcount"
	"foxesslocal/internal/wizard/captive"
	"foxesslocal/internal/wizard/health"
	"foxesslocal/internal/wizard/identity"
	"foxesslocal/internal/wizard/led"
	"foxesslocal/internal/wizard/loop"
	"foxesslocal/internal/wizard/mdns"
	"foxesslocal/internal/wizard/netmgr"
	"foxesslocal/internal/wizard/state"
	"foxesslocal/internal/wizard/web"
)

// Where the wizard keeps things on the stick.
const (
	stateDir = "/var/lib/foxess-wizard"
	iface    = "wlan0"

	readHeaderTimeout = 10 * time.Second
	mdnsRetry         = 30 * time.Second
	shutdownWait      = 5 * time.Second
	stateDirMode      = 0o700
)

// uptime reads the system's uptime: boots are counted by it (there is
// no battery-backed clock).
func uptime() time.Duration {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0
	}
	s, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0
	}
	return time.Duration(s * float64(time.Second))
}

func runCmd(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfg := fs.String("config", identity.Path, "the stick's hotspot and name")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))
	id, err := identity.Read(*cfg)
	if err != nil {
		log.Error("config", "err", err)
		return exitFailed
	}
	if err := os.MkdirAll(stateDir, stateDirMode); err != nil {
		log.Error("state dir", "err", err)
		return exitFailed
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	radio := &netmgr.NM{Run: netmgr.Exec{}, Iface: iface, KeyfileDir: netmgr.KeyfileDir}
	l := &loop.Loop{
		Radio:        radio,
		Lights:       led.New(led.Root),
		Probe:        health.Prober{BaseURL: health.DefaultURL, Run: netmgr.Exec{}},
		Boots:        &bootcount.Store{Path: bootcount.Path},
		Uptime:       uptime,
		InverterSeen: filepath.Join(stateDir, "inverter-seen"),
		HotspotSSID:  id.SSID,
		HotspotPSK:   id.Password,
		Log:          log,
	}
	srv := &web.Server{Wizard: l, Password: id.Password, Host: id.Host, LockoutPath: filepath.Join(stateDir, "lockout")}
	h := srv.Handler()
	l.Operator = srv.Operator
	l.Portal = &captive.Portal{Procs: captive.Exec{Log: func(m string, a ...any) { log.Error(m, a...) }}, Listen: captive.HTTP{}, Handler: h}

	hs := &http.Server{Addr: ":" + strconv.Itoa(web.Port), Handler: h, ReadHeaderTimeout: readHeaderTimeout}
	go func() {
		if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http", "err", err)
			stop()
		}
	}()
	go serveMDNS(ctx, l, id.Host, log)
	log.Info("foxess-wizard running", "version", version, "host", id.Host+".local")
	err = l.Run(ctx)
	sctx, cancel := context.WithTimeout(context.Background(), shutdownWait)
	defer cancel()
	_ = hs.Shutdown(sctx)
	if err != nil {
		log.Error("loop", "err", err)
		return exitFailed
	}
	return 0
}

// serveMDNS answers <host>.local while the stick is on the home
// network; it retries when wlan0 is away (hotspot, logger network).
func serveMDNS(ctx context.Context, l *loop.Loop, host string, log *slog.Logger) {
	addr := func() netip.Addr {
		s := l.Snapshot()
		if s.Status.Role != netmgr.RoleHome || !s.Status.Addr.IsValid() {
			return netip.Addr{}
		}
		return s.Status.Addr.Addr()
	}
	for ctx.Err() == nil {
		r := &mdns.Responder{Iface: iface, Host: host, Addr: addr, Log: func(m string, a ...any) { log.Warn(m, a...) }}
		if err := r.Serve(ctx); err != nil {
			log.Debug("mdns", "err", err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(mdnsRetry):
		}
	}
}

// demoCmd serves the real page over a fake wizard in one state, for
// checking the page in a browser (like tools/devui for foxess-local).
func demoCmd(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", "127.0.0.1:18081", "listen address")
	st := fs.String("state", "unconfigured", "the state to show")
	join := fs.String("join", "", "a last join result: ok, wrong_password, not_found")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	d := &demoWizard{snap: demoSnapshot(*st, *join)}
	srv := &web.Server{Wizard: d, Password: "demo-password", Host: "foxess-1a2b"}
	h := srv.Handler()
	// every request looks like a hotspot client on the wizard's address
	wrap := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = net.JoinHostPort("10.42.0.23", "50000")
		r.Host = net.JoinHostPort(netmgr.HotspotAddr.String(), strconv.Itoa(web.Port))
		h.ServeHTTP(w, r)
	})
	_, _ = fmt.Fprintf(stderr, "demo: http://%s/ (state %s)\n", *addr, *st)
	hs := &http.Server{Addr: *addr, Handler: wrap, ReadHeaderTimeout: readHeaderTimeout}
	if err := hs.ListenAndServe(); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return exitFailed
	}
	return 0
}

//nolint:mnd // illustrative signal strengths for the demo page
var demoNetworks = []netmgr.Network{
	{SSID: "home-network", Signal: 82, Secured: true},
	{SSID: "neighbour", Signal: 40, Secured: true},
	{SSID: "cafe guest", Signal: 22},
}

type demoWizard struct{ snap loop.Snapshot }

func (d *demoWizard) Snapshot() loop.Snapshot    { return d.snap }
func (d *demoWizard) StartJoin(_, _ string) bool { return true }
func (d *demoWizard) Rescan() bool               { return true }
func (d *demoWizard) RequestHotspot() bool       { return true }

func demoSnapshot(name, join string) loop.Snapshot {
	st := state.Unconfigured
	for _, c := range []state.State{state.Unconfigured, state.Joining, state.Normal, state.Degraded, state.HomeLost, state.Hotspot, state.AwaitingInverter} {
		if c.String() == name {
			st = c
		}
	}
	s := loop.Snapshot{Decision: state.Decision{State: st}, Networks: demoNetworks, ScanAt: time.Now()}
	if st == state.Degraded {
		s.Decision.Problems = []state.Problem{state.ProblemNoInverter}
	}
	if st == state.Normal || st == state.Degraded || st == state.AwaitingInverter {
		s.Status = netmgr.Status{Role: netmgr.RoleHome, Addr: netip.MustParsePrefix("192.0.2.40/24")}
	}
	if join != "" {
		var err error
		switch join {
		case "wrong_password":
			err = netmgr.ErrWrongPassword
		case "not_found":
			err = netmgr.ErrNotFound
		}
		s.Join = loop.NewJoinResult("home-network", time.Now(), err)
	}
	return s
}
