package guard_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"foxesslocal/internal/guard"
)

// TestHostCheck (audit S6): a request is served only when its Host names
// this machine — an IP literal, localhost, the machine's own name, a
// local-only suffix, or a configured name. A DNS-rebinding page arrives
// with its own domain in Host and is refused (421) before any handler.
func TestHostCheck(t *testing.T) {
	h := guard.HostCheck("gateway", []string{"solar.example.org"})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	cases := map[string]int{
		"192.168.1.10:18080":      http.StatusOK,
		"192.168.1.10":            http.StatusOK,
		"[fd00::1]:18080":         http.StatusOK,
		"localhost:18080":         http.StatusOK,
		"gateway:18080":           http.StatusOK,
		"GATEWAY":                 http.StatusOK,
		"gateway.local:18080":     http.StatusOK,
		"foxess.home.arpa":        http.StatusOK,
		"gateway.lan":             http.StatusOK,
		"box.internal":            http.StatusOK,
		"solar.example.org:18080": http.StatusOK,
		"evil.example.com:18080":  http.StatusMisdirectedRequest,
		"192.168.1.10.nip.io":     http.StatusMisdirectedRequest,
		"":                        http.StatusMisdirectedRequest,
	}
	for host, want := range cases {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/state", nil)
		r.Host = host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("Host %q: %d, want %d", host, w.Code, want)
		}
	}
}
