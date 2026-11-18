package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
)

// startPprof serves the Go profiler on addr ([debug] pprof_listen,
// loopback only — config validation) on its own listener and mux: the
// API never shares it and nothing but the profiler is reachable on it.
// It returns the server and the bound address.
//
//	ssh -L 6060:127.0.0.1:6060 <device>
//	go tool pprof http://127.0.0.1:6060/debug/pprof/profile?seconds=60
//	go tool pprof http://127.0.0.1:6060/debug/pprof/heap
func startPprof(addr string) (*http.Server, string, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, "", fmt.Errorf("pprof listen %s: %w", addr, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: httpReadHeaderTimeout}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(os.Stderr, "pprof:", err)
		}
	}()
	slog.Info("pprof: profiler listening (loopback only)", "addr", ln.Addr().String())
	return srv, ln.Addr().String(), nil
}
