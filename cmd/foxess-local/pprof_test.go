package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestPprofServer: the profiler answers on its own listener and serves
// nothing else (the API never shares it).
func TestPprofServer(t *testing.T) {
	srv, addr, err := startPprof("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	get := func(path string) (int, string) {
		t.Helper()
		resp, err := http.Get("http://" + addr + path) //nolint:noctx // test against a local listener
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get("/debug/pprof/"); code != http.StatusOK || !strings.Contains(body, "heap") {
		t.Fatalf("/debug/pprof/: %d %.80s", code, body)
	}
	if code, _ := get("/api/v1/state"); code != http.StatusNotFound {
		t.Fatalf("API path on the profiler listener: %d, want 404", code)
	}
}
