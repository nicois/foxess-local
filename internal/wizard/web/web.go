// Package web is the wizard's page and API (design D-031, "Reaching the
// wizard"): on the hotspot (signed in by having joined it, with every
// other host redirected to it: the captive portal), on the home network
// and over USB (signed in with the stick's password).
package web

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"foxesslocal/internal/wizard/loop"
	"foxesslocal/internal/wizard/netmgr"
)

//go:embed static
var static embed.FS

// Port is the wizard's HTTP port.
const Port = 18081

const (
	operatorIdle   = time.Minute // the page polls every 5 s
	sessionTTL     = 12 * time.Hour
	maxFailures    = 5
	firstLockout   = 15 * time.Minute
	maxLockout     = 4 * time.Hour
	maxBody        = 1 << 10
	cookieName     = "fw_session"
	csrfHeader     = "X-Wizard"
	lockoutMode    = 0o600
	hotspotBits    = 24
	foxessHTTPPort = 18080
	tokenBytes     = 32
	errKey         = "error"
)

// Wizard is the loop as the page sees it.
type Wizard interface {
	Snapshot() loop.Snapshot
	StartJoin(ssid, psk string) bool
	Rescan() bool
	RequestHotspot() bool
}

// Server serves the page and API.
type Server struct {
	Wizard      Wizard
	Password    string // the stick's password (on its label)
	Host        string // foxess-1a2b
	LockoutPath string // the sign-in lockout, kept across restarts

	once        sync.Once
	mu          sync.Mutex
	sessions    map[string]time.Time // token → expiry
	failures    int
	lockouts    int
	lockedUntil time.Time
	lastOp      time.Time // the last request from a hotspot client
}

var hotspotNet = netip.PrefixFrom(netmgr.HotspotAddr, hotspotBits)

// Handler is the wizard's HTTP handler.
func (s *Server) Handler() http.Handler {
	s.once.Do(s.load)
	files, err := fs.Sub(static, "static")
	if err != nil {
		panic(err) // the embedded tree is fixed at build time
	}
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(files)))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFileFS(w, r, files, "index.html")
	})
	mux.HandleFunc("GET /api/status", s.authed(s.handleStatus))
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/join", s.authed(s.handleJoin))
	mux.HandleFunc("POST /api/rescan", s.authed(func(w http.ResponseWriter, _ *http.Request) {
		s.accepted(w, s.Wizard.Rescan())
	}))
	mux.HandleFunc("POST /api/hotspot", s.authed(func(w http.ResponseWriter, _ *http.Request) {
		s.accepted(w, s.Wizard.RequestHotspot())
	}))
	return s.front(mux)
}

// front notes hotspot clients, redirects their requests for other hosts
// (captive probes) to the wizard, and guards POSTs against cross-site
// forms (a custom header no form can send).
func (s *Server) front(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if onHotspot(r) {
			s.mu.Lock()
			s.lastOp = time.Now()
			s.mu.Unlock()
			if !s.ownHost(r.Host) {
				http.Redirect(w, r, "http://"+net.JoinHostPort(netmgr.HotspotAddr.String(), strconv.Itoa(Port))+"/", http.StatusFound)
				return
			}
		}
		if r.Method == http.MethodPost && r.Header.Get(csrfHeader) != "1" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) ownHost(host string) bool {
	h := host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		h = hh
	}
	h = strings.ToLower(strings.TrimSuffix(h, "."))
	return h == netmgr.HotspotAddr.String() || h == strings.ToLower(s.Host)+".local"
}

func onHotspot(r *http.Request) bool {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	return err == nil && hotspotNet.Contains(ap.Addr().Unmap())
}

// Operator reports whether a hotspot client has been active lately.
func (s *Server) Operator() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.lastOp.IsZero() && time.Since(s.lastOp) < operatorIdle
}

func (s *Server) signedIn(r *http.Request) bool {
	if onHotspot(r) {
		return true
	}
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.sessions[c.Value]
	if ok && time.Now().After(exp) {
		delete(s.sessions, c.Value)
		return false
	}
	return ok
}

func (s *Server) authed(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.signedIn(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]bool{"signed_in": false})
			return
		}
		h(w, r)
	}
}

func (s *Server) accepted(w http.ResponseWriter, ok bool) {
	if !ok {
		writeJSON(w, http.StatusConflict, map[string]string{errKey: "busy"})
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

type networkView struct {
	SSID    string `json:"ssid"`
	Signal  int    `json:"signal"`
	Secured bool   `json:"secured"`
}

type joinView struct {
	SSID   string `json:"ssid"`
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"` // wrong_password, not_found, failed
	AgeS   int    `json:"age_s"`
}

type statusView struct {
	State     string        `json:"state"`
	Problems  []string      `json:"problems"`
	SignedIn  bool          `json:"signed_in"`
	OnHotspot bool          `json:"on_hotspot"`
	Host      string        `json:"host"`
	Address   string        `json:"address,omitempty"`    // the stick's home address
	FoxessURL string        `json:"foxess_url,omitempty"` // foxess-local's UI
	Networks  []networkView `json:"networks"`
	ScanAgeS  *int          `json:"scan_age_s,omitempty"`
	Join      *joinView     `json:"join,omitempty"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	snap := s.Wizard.Snapshot()
	v := statusView{
		State:     snap.Decision.State.String(),
		Problems:  []string{},
		SignedIn:  true,
		OnHotspot: onHotspot(r),
		Host:      s.Host + ".local",
		Networks:  []networkView{},
	}
	for _, p := range snap.Decision.Problems {
		v.Problems = append(v.Problems, string(p))
	}
	if a := snap.Status.Addr; a.IsValid() && snap.Status.Role == netmgr.RoleHome {
		v.Address = a.Addr().String()
		v.FoxessURL = "http://" + net.JoinHostPort(v.Address, strconv.Itoa(foxessHTTPPort)) + "/"
	}
	for _, n := range snap.Networks {
		v.Networks = append(v.Networks, networkView(n))
	}
	if !snap.ScanAt.IsZero() {
		age := int(time.Since(snap.ScanAt).Seconds())
		v.ScanAgeS = &age
	}
	if j := snap.Join; j != nil {
		v.Join = &joinView{SSID: j.SSID, OK: j.Error() == nil, Reason: reason(j.Error()), AgeS: int(time.Since(j.At).Seconds())}
	}
	writeJSON(w, http.StatusOK, v)
}

// reason names a join failure for the page.
func reason(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, netmgr.ErrWrongPassword):
		return "wrong_password"
	case errors.Is(err, netmgr.ErrNotFound):
		return "not_found"
	default:
		return "failed"
	}
}

func (s *Server) handleJoin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SSID     string `json:"ssid"`
		Password string `json:"password"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if err := netmgr.Validate(req.SSID, req.Password); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{errKey: "invalid"})
		return
	}
	s.accepted(w, s.Wizard.StartJoin(req.SSID, req.Password))
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if wait := time.Until(s.lockedUntil); wait > 0 {
		writeJSON(w, http.StatusTooManyRequests, map[string]int{"retry_after_s": int(wait.Seconds()) + 1})
		return
	}
	if subtle.ConstantTimeCompare([]byte(req.Password), []byte(s.Password)) != 1 || s.Password == "" {
		s.failures++
		if s.failures >= maxFailures {
			s.failures = 0
			s.lockouts++
			d := min(firstLockout<<(s.lockouts-1), maxLockout)
			s.lockedUntil = time.Now().Add(d)
			s.saveLockout(d)
		}
		writeJSON(w, http.StatusUnauthorized, map[string]bool{"signed_in": false})
		return
	}
	s.failures, s.lockouts = 0, 0
	s.saveLockout(0)
	tok := make([]byte, tokenBytes)
	if _, err := rand.Read(tok); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	t := hex.EncodeToString(tok)
	if s.sessions == nil {
		s.sessions = map[string]time.Time{}
	}
	s.sessions[t] = time.Now().Add(sessionTTL)
	// no Secure flag: the wizard is plain HTTP on the hotspot, the home
	// network and USB (no TLS, owner 2026-11-17)
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: plain HTTP by design; HttpOnly and SameSite are set
		Name: cookieName, Value: t, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, MaxAge: int(sessionTTL.Seconds()),
	})
	w.WriteHeader(http.StatusNoContent)
}

// The lockout file holds the lockout count and the remaining seconds
// when it was written: the stick has no clock, so a restart resumes a
// lockout in full (conservative).
func (s *Server) saveLockout(remaining time.Duration) {
	if s.LockoutPath == "" {
		return
	}
	b := strconv.Itoa(s.lockouts) + " " + strconv.Itoa(int(remaining.Seconds())) + "\n"
	_ = os.WriteFile(s.LockoutPath, []byte(b), lockoutMode)
}

func (s *Server) load() {
	if s.LockoutPath == "" {
		return
	}
	b, err := os.ReadFile(s.LockoutPath)
	if err != nil {
		return
	}
	f := strings.Fields(string(b))
	if len(f) != 2 {
		return
	}
	n, err1 := strconv.Atoi(f[0])
	rem, err2 := strconv.Atoi(f[1])
	if err1 != nil || err2 != nil || n < 0 || rem < 0 {
		return
	}
	s.lockouts = n
	if rem > 0 {
		s.lockedUntil = time.Now().Add(time.Duration(rem) * time.Second)
	}
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{errKey: "bad request"})
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		return // the client went away; nothing to tell it
	}
}
