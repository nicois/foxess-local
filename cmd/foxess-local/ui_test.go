// ui_test.go: the task-11 UI mount gate — web.Handler() is the /
// catch-all added LAST to buildMux, so the specific routes win: the
// API auth is unchanged, the sim prefixes are not shadowed, and the
// SSE stream is byte-identical (": connected" first).
package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nicois/prox/pkg/hooks"
	"github.com/stretchr/testify/require"

	"foxesslocal/internal/api"
	"foxesslocal/internal/config"
	"foxesslocal/internal/forward"
	"foxesslocal/internal/framesvc"
	"foxesslocal/internal/gateway"
	"foxesslocal/internal/policy"
	"foxesslocal/internal/store"
)

// uiBase is the fixed clock anchor (C-004: no wall-clock in tests).
var uiBase = time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)

// uiSimKey is the sim's test API key (test-only; the sim is a LAN
// fixture, and the key is meaningless outside this test binary).
//

const uiSimKey = "ui-sim-key"

// uiStubInjector is the gateway injector for the UI tests (no command
// is ever driven through the stack here).
type uiStubInjector struct{}

func (uiStubInjector) InjectS2C(string, []byte) error { return nil }

// buildUIStack assembles the handle fields buildMux reads, with the
// same seams as production (real store in a temp dir, real gateway with
// a stub injector, real framesvc service on an empty pipeline, real
// forward state, real TokenStore with one installer token, sim enabled
// so the sim prefixes exist).
func buildUIStack(t *testing.T) (mux http.Handler, token string) {
	t.Helper()
	clk := func() time.Time { return uiBase }
	dir := t.TempDir()

	st, err := store.Open(filepath.Join(dir, "ui.db"), version)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })

	cfg := &config.Config{
		Retention: config.Retention{RawHours: 72, BucketDays: 365},
		Sim:       config.Sim{Enabled: true, APIKey: uiSimKey, StationID: "ui-station", CapacityKW: 10},
	}

	h := &handle{
		cfg: cfg,
		clk: clk,
		st:  st,
		gw:  gateway.New(uiStubInjector{}, clk),
		fwd: forward.New(st, clk, policy.ModeFull),
		bus: api.NewBus(),
	}
	h.maint = forward.NewMaintenance(st, clk, func() policy.ForwardMode {
		m, _, _ := h.fwd.Get()
		return m
	}, nil)
	h.svc = framesvc.New(hooks.NewPipeline(), st, h.gw, h.posture, clk)

	sim, err := h.buildSim(cfg)
	require.NoError(t, err)
	h.sim = sim

	tok, err := api.NewTokenStore(filepath.Join(dir, "tokens.json"), clk)
	require.NoError(t, err)
	_, plaintext, err := tok.Create("ui-test", api.RoleInstaller)
	require.NoError(t, err)

	return h.buildMux(cfg, tok), plaintext
}

func uiDo(t *testing.T, mux http.Handler, path string, hdrs map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = "127.0.0.1:18080" // the Host check: an address, as a browser on the LAN sends
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// TestUIIndex: the / catch-all serves the SPA shell.
func TestUIIndex(t *testing.T) {
	mux, _ := buildUIStack(t)
	rec := uiDo(t, mux, "/", nil)
	require.Equal(t, http.StatusOK, rec.Code, "GET /")
	require.True(t, strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html"),
		"Content-Type %q", rec.Header().Get("Content-Type"))
}

// TestUIViewAsset: the vendored htmx core resolves through the catch-all.
func TestUIViewAsset(t *testing.T) {
	mux, _ := buildUIStack(t)
	rec := uiDo(t, mux, "/static/vendor/htmx.min.js", nil)
	require.Equal(t, http.StatusOK, rec.Code, "GET /static/vendor/htmx.min.js")
}

// TestUIAuthUnchanged: the mount did not weaken the API auth (the UI
// HTML carries no data; every datum flows through /api/v1).
func TestUIAuthUnchanged(t *testing.T) {
	mux, token := buildUIStack(t)
	require.Equal(t, http.StatusUnauthorized,
		uiDo(t, mux, "/api/v1/state", nil).Code, "GET /api/v1/state without token")
	require.Equal(t, http.StatusOK,
		uiDo(t, mux, "/api/v1/state", map[string]string{"Authorization": "Bearer " + token}).Code,
		"GET /api/v1/state with token")
}

// TestUIHealthAndSim: /healthz stays open; the sim prefix is not
// shadowed by the / catch-all.
func TestUIHealthAndSim(t *testing.T) {
	mux, _ := buildUIStack(t)
	require.Equal(t, http.StatusOK,
		uiDo(t, mux, "/healthz", nil).Code, "GET /healthz")
	rec := uiDo(t, mux, "/op/v0/device/detail", map[string]string{"token": uiSimKey})
	require.Equal(t, http.StatusOK, rec.Code, "GET /op/v0/device/detail (sim prefix)")
	require.Contains(t, rec.Body.String(), `"errno":0`, "sim envelope body")
}

// TestUIEventsUntouched: the SSE stream is unaffected by the UI mount
// (the ?token= EventSource fallback still streams ": connected"
// first). The read timeout is the escape — no sleeping.
func TestUIEventsUntouched(t *testing.T) {
	mux, token := buildUIStack(t)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/v1/events?token="+token, nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode, "GET /api/v1/events")
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))

	buf := make([]byte, 64)
	n, err := resp.Body.Read(buf)
	require.NoError(t, err)
	require.Positive(t, n)
	require.True(t, strings.HasPrefix(string(buf[:n]), ": connected"), "first bytes %q", string(buf[:n]))
	cancel()
}
