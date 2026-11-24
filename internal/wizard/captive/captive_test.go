package captive

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
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
	c := &Portal{Procs: p, Listen: l, Handler: http.NotFoundHandler()}
	if err := c.Up(context.Background()); err != nil {
		t.Fatal(err)
	}
	args := strings.Join(p.args, " ")
	for _, want := range []string{
		"dnsmasq", "--keep-in-foreground", "--conf-file=/dev/null", "--bind-interfaces",
		"--listen-address=10.42.0.1", "--address=/#/10.42.0.1", "--no-resolv",
		"--dhcp-range=10.42.0.10,10.42.0.200,10m",
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
