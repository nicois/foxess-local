package captive

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeProc struct {
	mu      sync.Mutex
	args    []string
	started int
	stopped int
}

func (f *fakeProc) Start(_ context.Context, name string, args ...string) (func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.args = append([]string{name}, args...)
	f.started++
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.stopped++
	}, nil
}

type fakeListen struct {
	mu     sync.Mutex
	addrs  []string
	closed int
}

func (f *fakeListen) Serve(addr string, _ http.Handler) (func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addrs = append(f.addrs, addr)
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.closed++
	}, nil
}

func TestUpDown(t *testing.T) {
	p, l := &fakeProc{}, &fakeListen{}
	dir := filepath.Join(t.TempDir(), "run", "foxess-wizard")
	c := &Portal{Procs: p, Listen: l, Handler: http.NotFoundHandler(), LeaseDir: dir}
	if err := c.Up(context.Background()); err != nil {
		t.Fatal(err)
	}
	// dnsmasq cannot create its lease file's directory itself
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("lease dir: %v %v", fi, err)
	}
	args := strings.Join(p.args, " ")
	for _, want := range []string{
		"dnsmasq", "--keep-in-foreground", "--conf-file=/dev/null", "--bind-interfaces",
		"--listen-address=10.42.0.1", "--address=/#/10.42.0.1", "--no-resolv",
		"--dhcp-range=10.42.0.10,10.42.0.200,10m",
		"--dhcp-leasefile=" + filepath.Join(dir, "dnsmasq.leases"),
	} {
		if !strings.Contains(args, want) {
			t.Errorf("dnsmasq args lack %q: %s", want, args)
		}
	}
	if !slices.Contains(l.addrs, "10.42.0.1:80") {
		t.Errorf("no port-80 listener: %v", l.addrs)
	}
	// Up again is a no-op
	if err := c.Up(context.Background()); err != nil || p.started != 1 {
		t.Fatalf("second Up: started %d, %v", p.started, err)
	}
	c.Down()
	c.Down()
	if p.stopped != 1 || l.closed != 1 {
		t.Fatalf("Down: stopped %d closed %d", p.stopped, l.closed)
	}
}

// TestExecEarlyExit: a process that fails at start-up (dnsmasq with a
// bad lease path, a port in use) is an error that carries its stderr,
// not a silent success.
func TestExecEarlyExit(t *testing.T) {
	e := Exec{grace: func() <-chan time.Time { return nil }} // wait for the exit
	_, err := e.Start(context.Background(), "sh", "-c", "echo 'dnsmasq: cannot open lease file' >&2; exit 3")
	if err == nil || !strings.Contains(err.Error(), "cannot open lease file") || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("early exit: %v", err)
	}
}

// TestExecRunning: a process still running after the grace period is
// started; stopping it later is not reported as a failure.
func TestExecRunning(t *testing.T) {
	now := make(chan time.Time)
	close(now) // the grace period is over at once
	var mu sync.Mutex
	var logged []string
	e := Exec{grace: func() <-chan time.Time { return now }, Log: func(m string, _ ...any) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, m)
	}}
	stop, err := e.Start(context.Background(), "sh", "-c", "exec sleep 30")
	if err != nil {
		t.Fatal(err)
	}
	stop()
	mu.Lock()
	defer mu.Unlock()
	if len(logged) != 0 {
		t.Fatalf("a requested stop was logged: %v", logged)
	}
}

// TestExecDiesLater: a process that dies on its own after start-up is
// logged with its stderr.
func TestExecDiesLater(t *testing.T) {
	now := make(chan time.Time)
	close(now)
	got := make(chan []any, 1)
	e := Exec{grace: func() <-chan time.Time { return now }, Log: func(_ string, a ...any) { got <- a }}
	release := filepath.Join(t.TempDir(), "go")
	// the process waits for the test, so it is still running when the grace ends
	stop, err := e.Start(context.Background(), "sh", "-c", "while [ ! -e "+release+" ]; do sleep 0.01; done; echo crashed >&2; exit 1")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	a := <-got
	if !strings.Contains(fmtArgs(a), "crashed") {
		t.Fatalf("logged %v", a)
	}
}

func fmtArgs(a []any) string {
	var b strings.Builder
	for _, v := range a {
		if s, ok := v.(string); ok {
			b.WriteString(s + " ")
		}
		if e, ok := v.(error); ok {
			b.WriteString(e.Error() + " ")
		}
	}
	return b.String()
}
