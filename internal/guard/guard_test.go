package guard_test

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"foxesslocal/internal/guard"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serve drives the wrapper over a 200-handler with req.RemoteAddr set
// directly (httptest allows fabricated remotes — no sockets needed).
func serve(t *testing.T, allowPublic bool, extra []netip.Prefix, remote, path string) *httptest.ResponseRecorder {
	t.Helper()
	h := guard.Wrapper(allowPublic, extra)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.RemoteAddr = remote
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestWrapperLocalAllowed(t *testing.T) {
	for _, remote := range []string{
		"127.0.0.1:1", "[::1]:1", "192.168.4.9:1", "10.1.2.3:1", "172.20.1.1:1", "fc00::5:1",
	} {
		t.Run(remote, func(t *testing.T) {
			w := serve(t, false, nil, remote, "/dashboard")
			assert.Equal(t, http.StatusOK, w.Code)
		})
	}
}

func TestWrapperNonLocalAPIRejected(t *testing.T) {
	w := serve(t, false, nil, "203.0.113.7:1", "/api/v1/state")
	require.Equal(t, http.StatusForbidden, w.Code)
	assert.Equal(t, "application/problem+json", w.Header().Get("Content-Type"))
	assert.Contains(t, w.Body.String(), "local_network_only")
}

func TestWrapperNonLocalUIPathHTML(t *testing.T) {
	w := serve(t, false, nil, "203.0.113.7:1", "/static/css/app.css")
	require.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "text/html")
}

func TestWrapperExtraAllowed(t *testing.T) {
	extra := []netip.Prefix{netip.MustParsePrefix("100.64.0.0/10")}
	w := serve(t, false, nil, "100.64.5.6:1", "/")
	assert.Equal(t, http.StatusForbidden, w.Code)
	w = serve(t, false, extra, "100.64.5.6:1", "/")
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestWrapperAllowPublic(t *testing.T) {
	w := serve(t, true, nil, "203.0.113.7:1", "/")
	assert.Equal(t, http.StatusOK, w.Code)
}

// No-port and unparseable remotes fail closed.
func TestWrapperFailClosed(t *testing.T) {
	for _, remote := range []string{"203.0.113.7", "nonsense"} {
		t.Run(remote, func(t *testing.T) {
			w := serve(t, false, nil, remote, "/")
			assert.Equal(t, http.StatusForbidden, w.Code)
		})
	}
}
