package health

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"foxesslocal/internal/api"
)

type fakeRunner struct {
	out string
	err error
}

func (f fakeRunner) Run(context.Context, string, ...string) ([]byte, error) {
	return []byte(f.out), f.err
}

// errNotFailed is systemctl is-failed's non-zero exit for a unit that
// is not failed.
var errNotFailed = errors.New("exit status 1")

func server(t *testing.T, readyCode int, readyBody string) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(readyCode)
		_, _ = w.Write([]byte(readyBody))
	})
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s.URL
}

func TestProbeReady(t *testing.T) {
	url := server(t, http.StatusOK, `{"status":"ready","checks":[{"id":"inverter_connected","title":"Inverter connected","status":"ok","detail":""},{"id":"store","title":"Database writes","status":"ok","detail":""}]}`)
	p := Prober{BaseURL: url, Run: fakeRunner{out: "active\n", err: errNotFailed}}
	r := p.Probe(context.Background())
	if !r.Live || r.UnitFailed || r.Ready != "ready" || !r.InverterSeen() {
		t.Fatalf("report %+v", r)
	}
}

func TestProbeNotReady(t *testing.T) {
	url := server(t, http.StatusServiceUnavailable, `{"status":"not_ready","checks":[{"id":"inverter_connected","title":"Inverter connected","status":"fail","detail":""}]}`)
	p := Prober{BaseURL: url, Run: fakeRunner{out: "active\n", err: errNotFailed}}
	r := p.Probe(context.Background())
	if !r.Live || r.Ready != "not_ready" || r.InverterSeen() {
		t.Fatalf("report %+v", r)
	}
	if r.Checks[checkInverter] != api.CheckFail {
		t.Fatalf("checks %v", r.Checks)
	}
}

func TestProbeDown(t *testing.T) {
	p := Prober{BaseURL: "http://127.0.0.1:1", Run: fakeRunner{out: "failed\n"}}
	r := p.Probe(context.Background())
	if r.Live || !r.UnitFailed || r.Ready != "" || r.InverterSeen() {
		t.Fatalf("report %+v", r)
	}
}

// TestPinnedToAPI: the ids and statuses the wizard reads are
// foxess-local's own, so a rename there fails here.
func TestPinnedToAPI(t *testing.T) {
	if checkInverter != api.ReadyInverterConnected {
		t.Errorf("inverter check id %q, the API's is %q", checkInverter, api.ReadyInverterConnected)
	}
	if statusOK != api.CheckOK || statusPending != api.CheckPending {
		t.Errorf("statuses %q/%q, the API's are %q/%q", statusOK, statusPending, api.CheckOK, api.CheckPending)
	}
}
