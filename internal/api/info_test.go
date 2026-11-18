package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The ServerInfo variant: /config reflects the injected values (task 10
// wires the real config into the host); the default New behaviour is
// unchanged (the existing config tests pin the dev defaults).
func TestServerInfoInjected(t *testing.T) {
	h := newHarness(t)
	srv := New(h.st, h.gw, h.mnt, h.tok, h.fwd, h.bus, h.clk.get, WithInfo(ServerInfo{
		Version:    "971d648-dev",
		RawHours:   45,
		BucketDays: 30,
	}))

	req := httptest.NewRequest(httpGet, "/api/v1/config", nil)
	req.Header.Set(headerAuth, "Bearer "+h.tokens[RoleReadOnly])
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var v struct {
		Version    string `json:"version"`
		RawHours   int    `json:"raw_hours"`
		BucketDays int    `json:"bucket_days"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &v))
	assert.Equal(t, "971d648-dev", v.Version)
	assert.Equal(t, 45, v.RawHours)
	assert.Equal(t, 30, v.BucketDays)
}
