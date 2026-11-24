package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	fl "foxesslocal/internal/web"
	"foxesslocal/internal/wizard/loop"
	"foxesslocal/internal/wizard/netmgr"
	"foxesslocal/internal/wizard/state"
)

const (
	hotspotClient = "10.42.0.23:51000"
	homeClient    = "192.0.2.77:51000"
	password      = "stick-password"
	testSSID      = "home"
)

type fakeWizard struct {
	mu    sync.Mutex
	snap  loop.Snapshot
	joins []string
	asks  int
}

func (f *fakeWizard) Snapshot() loop.Snapshot { f.mu.Lock(); defer f.mu.Unlock(); return f.snap }
func (f *fakeWizard) StartJoin(ssid, _ string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.joins = append(f.joins, ssid)
	return true
}
func (f *fakeWizard) Rescan() bool { return true }
func (f *fakeWizard) RequestHotspot() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asks++
	return true
}

func newServer(t *testing.T, w *fakeWizard, dir string) *Server {
	t.Helper()
	return &Server{Wizard: w, Password: password, Host: "foxess-1a2b", LockoutPath: filepath.Join(dir, "lockout")}
}

func do(t *testing.T, h http.Handler, method, url, remote, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	if strings.HasPrefix(url, "/") {
		url = "http://10.42.0.1:18081" + url // the page's own origin
	}
	req := httptest.NewRequest(method, url, strings.NewReader(body))
	req.RemoteAddr = remote
	if method == http.MethodPost {
		req.Header.Set("X-Wizard", "1")
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHotspotClientIsSignedIn(t *testing.T) {
	w := &fakeWizard{snap: loop.Snapshot{
		Decision: state.Decision{State: state.Unconfigured},
		Networks: []netmgr.Network{{SSID: testSSID, Signal: 70, Secured: true}},
	}}
	h := newServer(t, w, t.TempDir()).Handler()
	rec := do(t, h, http.MethodGet, "http://10.42.0.1:18081/api/status", hotspotClient, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var v struct {
		State    string `json:"state"`
		SignedIn bool   `json:"signed_in"`
		Networks []struct {
			SSID string `json:"ssid"`
		} `json:"networks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.State != "unconfigured" || !v.SignedIn || len(v.Networks) != 1 || v.Networks[0].SSID != testSSID {
		t.Fatalf("status %+v", v)
	}
}

func TestHomeClientSignsIn(t *testing.T) {
	h := newServer(t, &fakeWizard{}, t.TempDir()).Handler()
	if rec := do(t, h, http.MethodGet, "/api/status", homeClient, "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned status: %d", rec.Code)
	}
	if rec := do(t, h, http.MethodPost, "/api/login", homeClient, `{"password":"wrong"}`, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d", rec.Code)
	}
	rec := do(t, h, http.MethodPost, "/api/login", homeClient, `{"password":"`+password+`"}`, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	c := rec.Result().Cookies()
	if len(c) != 1 || !c[0].HttpOnly || c[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie %+v", c)
	}
	if rec := do(t, h, http.MethodGet, "/api/status", homeClient, "", map[string]string{"Cookie": c[0].Name + "=" + c[0].Value}); rec.Code != http.StatusOK {
		t.Fatalf("signed-in status: %d", rec.Code)
	}
}

// TestLockout: five wrong passwords lock sign-in for 15 minutes, even
// for the right one, and across a restart.
func TestLockout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		h := newServer(t, &fakeWizard{}, dir).Handler()
		for range 5 {
			do(t, h, http.MethodPost, "/api/login", homeClient, `{"password":"wrong"}`, nil)
		}
		if rec := do(t, h, http.MethodPost, "/api/login", homeClient, `{"password":"`+password+`"}`, nil); rec.Code != http.StatusTooManyRequests {
			t.Fatalf("locked out: %d", rec.Code)
		}
		time.Sleep(10 * time.Minute)
		h2 := newServer(t, &fakeWizard{}, dir).Handler() // a restart
		if rec := do(t, h2, http.MethodPost, "/api/login", homeClient, `{"password":"`+password+`"}`, nil); rec.Code != http.StatusTooManyRequests {
			t.Fatalf("after a restart: %d", rec.Code)
		}
		time.Sleep(16 * time.Minute)
		if rec := do(t, h2, http.MethodPost, "/api/login", homeClient, `{"password":"`+password+`"}`, nil); rec.Code != http.StatusNoContent {
			t.Fatalf("after the lockout: %d", rec.Code)
		}
	})
}

func TestPostNeedsHeader(t *testing.T) {
	h := newServer(t, &fakeWizard{}, t.TempDir()).Handler()
	req := httptest.NewRequest(http.MethodPost, "http://10.42.0.1:18081/api/join", strings.NewReader(`{"ssid":"home","password":"a-password"}`))
	req.RemoteAddr = hotspotClient
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST without X-Wizard: %d", rec.Code)
	}
}

func TestJoin(t *testing.T) {
	w := &fakeWizard{}
	h := newServer(t, w, t.TempDir()).Handler()
	if rec := do(t, h, http.MethodPost, "/api/join", hotspotClient, `{"ssid":"home","password":"a-password"}`, nil); rec.Code != http.StatusAccepted {
		t.Fatalf("join: %d %s", rec.Code, rec.Body)
	}
	if len(w.joins) != 1 || w.joins[0] != testSSID {
		t.Fatalf("joins %v", w.joins)
	}
	if rec := do(t, h, http.MethodPost, "/api/join", hotspotClient, `{"ssid":"home","password":"short"}`, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("short password: %d", rec.Code)
	}
}

func TestJoinResultReasons(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{nil, ""},
		{netmgr.ErrWrongPassword, "wrong_password"},
		{netmgr.ErrNotFound, "not_found"},
		{netmgr.ErrJoinFailed, "failed"},
	} {
		if got := reason(c.err); got != c.want {
			t.Errorf("reason(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

// TestCaptiveRedirect: a hotspot client asking for any other host (the
// phones' captive probes) is sent to the wizard.
func TestCaptiveRedirect(t *testing.T) {
	h := newServer(t, &fakeWizard{}, t.TempDir()).Handler()
	for _, url := range []string{
		"http://connectivitycheck.gstatic.com/generate_204",
		"http://captive.apple.com/hotspot-detect.html",
		"http://www.msftconnecttest.com/connecttest.txt",
	} {
		rec := do(t, h, http.MethodGet, url, hotspotClient, "", nil)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != "http://10.42.0.1:18081/" {
			t.Errorf("%s: %d %q", url, rec.Code, rec.Header().Get("Location"))
		}
	}
	// the wizard's own address is served, not redirected
	if rec := do(t, h, http.MethodGet, "http://10.42.0.1:18081/", hotspotClient, "", nil); rec.Code != http.StatusOK {
		t.Errorf("own page: %d", rec.Code)
	}
	// a home-network client is never redirected
	if rec := do(t, h, http.MethodGet, "http://foxess-1a2b.local:18081/", homeClient, "", nil); rec.Code != http.StatusOK {
		t.Errorf("home client page: %d", rec.Code)
	}
}

func TestOperator(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newServer(t, &fakeWizard{}, t.TempDir())
		h := s.Handler()
		if s.Operator() {
			t.Fatal("operator before any request")
		}
		do(t, h, http.MethodGet, "/api/status", hotspotClient, "", nil)
		if !s.Operator() {
			t.Fatal("no operator after a hotspot request")
		}
		time.Sleep(operatorIdle + time.Second)
		if s.Operator() {
			t.Fatal("operator still present after they went quiet")
		}
		do(t, h, http.MethodGet, "/api/status", homeClient, "", nil)
		if s.Operator() {
			t.Fatal("a home-network request counted as an operator on the hotspot")
		}
	})
}

func TestRequestHotspot(t *testing.T) {
	w := &fakeWizard{}
	h := newServer(t, w, t.TempDir()).Handler()
	if rec := do(t, h, http.MethodPost, "/api/hotspot", homeClient, `{}`, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned: %d", rec.Code)
	}
	if rec := do(t, h, http.MethodPost, "/api/hotspot", hotspotClient, `{}`, nil); rec.Code != http.StatusAccepted || w.asks != 1 {
		t.Fatalf("hotspot request: %d asks %d", rec.Code, w.asks)
	}
}

// TestCatalogues: every wizard string is in all five catalogues, with
// the same placeholders (foxess-local's scanner and rules).
func TestCatalogues(t *testing.T) {
	keys, errs := fl.UIKeys("static")
	for _, err := range errs {
		t.Error(err)
	}
	if len(keys) < 20 {
		t.Fatalf("only %d UI strings found: the scan is broken", len(keys))
	}
	for _, lang := range fl.UILangs {
		b, err := os.ReadFile(filepath.Join("static", "i18n", lang+".json")) //nolint:gosec // G304: the package's own catalogues
		if err != nil {
			t.Fatal(err)
		}
		var cat map[string]string
		if err := json.Unmarshal(b, &cat); err != nil {
			t.Fatalf("%s: %v", lang, err)
		}
		missing, mismatched, stale := fl.CatalogueProblems(keys, cat)
		for _, m := range missing {
			t.Errorf("%s: missing %s", lang, m)
		}
		for _, m := range mismatched {
			t.Errorf("%s: %s", lang, m)
		}
		for _, s := range stale {
			t.Errorf("%s: stale entry %s", lang, s)
		}
	}
}
