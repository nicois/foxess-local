// Package sim is the cloud-compatible surface that serves the owner's
// foxess-control HACS integration unmodified (plan 2, task 8).
//
// It speaks the FoxESS cloud HTTP + WebSocket API the integration is
// written against: the /op/v0 REST endpoints, /basic/v0/user/login, and
// the /dew/v0 wsmaitian real-time feed (~5 s cadence). The read side is
// store-backed (decoded frames); the only inverter write is
// scheduler/enable, which goes through the gateway (C-003). It is
// deliberately not a cloud mimicry (C-005) and must not import
// internal/api; the host wires the bus and the plant record (task 10).
package sim

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"foxesslocal/internal/gateway"
	"foxesslocal/internal/store"
)

// Plant is the static plant/device record (config surface; the inverter's
// rated capacity is not in any decoded frame, so the host supplies it).
type Plant struct {
	StationID  string // stationID returned by plant/list and the WS plantId
	CapacityKW int    // rated kW; the integration derives fdPwr = capacity × 1050 W
}

// Config is the sim's configuration surface (task 10 sets it).
type Config struct {
	APIKey   string // /op/v0 credential (the integration's api_key, sent in the "token" header)
	Plant    Plant  // the plant record
	WebToken string // web-session token; random per start when empty (audit S7)
	// MinSoc / MinSocOnGrid seed the sim-local min-SoC pair (defaults 10 /
	// 11 when zero — the soc/set docs).
	MinSoc       int
	MinSocOnGrid int
}

// Gateway is the narrow seam over *gateway.Gateway: sim submits exactly
// one command kind (KindSchedule) and waits for its verdict.
type Gateway interface {
	Submit(ctx context.Context, c gateway.Command) (string, error)
	Wait(ctx context.Context, id string) (gateway.Result, error)
}

// Alert is the host's alert sink (the sim must not import internal/api
// — C-005): the host publishes the detail as an api.SystemEvent alert,
// so it reaches the web UI and the notifier like any other.
type Alert func(detail string)

// Cloud errno vocabulary (the integration branches only on 40400 and
// 41807-41809; auth failures are HTTP 401 per the brief).
const (
	errnoSuccess      = 0
	errnoInvalidValue = 40257 // invalid value / command failure (cloud errno)
	errnoUnauthorized = 41808 // auth failure (HTTP 401 body)
)

// Sim-local bounds and the cloud contract constants.
const (
	defaultMinSoc       = 10
	defaultMinSocOnGrid = 11
	defaultMaxSoc       = 100
	socBound            = 100
	opEnable            = 1
	opDisable           = 0
	slotBase            = 1 // the cloud groups are 1-based
	hourBound           = 23
	minuteBound         = 59
	groupBound          = 8
)

// wsPushPeriod is the real-time feed cadence (the 150 B tele cadence).
const wsPushPeriod = 5 * time.Second

// waitTimeout bounds a synchronous scheduler/enable Wait (the inverter
// answers a push in seconds when a flow is active).
const waitTimeout = 30 * time.Second

// staleTimeDiff marks the no-data keepalive (the client skips timeDiff > 30).
const staleTimeDiff = 999

// envelope is the cloud API response envelope.
type envelope struct {
	Errno  int    `json:"errno"`
	Msg    string `json:"msg"`
	Result any    `json:"result"`
}

// okEnvelope / errEnvelope build the two envelope shapes.
func okEnvelope(result any) envelope {
	return envelope{Errno: errnoSuccess, Msg: "success", Result: result}
}

func errEnvelope(errno int, msg string) envelope {
	return envelope{Errno: errno, Msg: msg, Result: nil}
}

// Server is the sim surface: one per inverter.
type Server struct {
	cfg Config
	st  *store.Store
	// state reads the decoded state (default st.QueryState). Tests inject
	// a pure closure to keep the SQLite I/O out of the synctest bubble
	// (the framesvc harness pattern: store reads stay outside the group).
	state func() (store.State, error)
	gw    Gateway
	alert Alert
	clock func() time.Time

	socMu        sync.Mutex
	minSoc       int
	minSocOnGrid int

	idMu sync.Mutex
	id   *deviceID

	wsMu    sync.Mutex
	clients []*wsClient
}

// New validates the config and builds the server.
func New(cfg Config, st *store.Store, gw Gateway, alert Alert, clock func() time.Time) (*Server, error) {
	if cfg.APIKey == "" {
		return nil, errors.New("sim: Config.APIKey is required")
	}
	if cfg.Plant.StationID == "" {
		return nil, errors.New("sim: Config.Plant.StationID is required")
	}
	if cfg.Plant.CapacityKW < 1 {
		return nil, errors.New("sim: Config.Plant.CapacityKW must be >= 1")
	}
	if clock == nil {
		clock = time.Now
	}
	minSoc := cfg.MinSoc
	if minSoc == 0 {
		minSoc = defaultMinSoc
	}
	minSocOnGrid := cfg.MinSocOnGrid
	if minSocOnGrid == 0 {
		minSocOnGrid = defaultMinSocOnGrid
	}
	if cfg.WebToken == "" {
		// a random per-start token, never the predictable
		// "sim-token-<station>" (audit S7); clients get it from login
		b := make([]byte, webTokenBytes)
		if _, err := rand.Read(b); err != nil {
			return nil, fmt.Errorf("sim: web token: %w", err)
		}
		cfg.WebToken = hex.EncodeToString(b)
	}
	slog.Warn("sim: the cloud-API simulator is enabled — a second write surface on the LAN (static API key); keep it off unless an integration needs it")
	return &Server{
		cfg:          cfg,
		st:           st,
		state:        st.QueryState,
		gw:           gw,
		alert:        alert,
		clock:        clock,
		minSoc:       minSoc,
		minSocOnGrid: minSocOnGrid,
	}, nil
}

// Handler returns the HTTP surface. The host mounts it on the :8080 mux or
// a dedicated listener (task 10).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /op/v0/plant/list", s.opAuth(s.handlePlantList))
	mux.HandleFunc("POST /op/v0/device/list", s.opAuth(s.handleDeviceList))
	mux.HandleFunc("GET /op/v0/device/detail", s.opAuth(s.handleDeviceDetail))
	mux.HandleFunc("POST /op/v0/device/real/query", s.opAuth(s.handleRealQuery))
	mux.HandleFunc("GET /op/v0/device/battery/soc/get", s.opAuth(s.handleSocGet))
	mux.HandleFunc("POST /op/v0/device/battery/soc/set", s.opAuth(s.handleSocSet))
	mux.HandleFunc("POST /op/v0/device/scheduler/get", s.opAuth(s.handleSchedulerGet))
	mux.HandleFunc("POST /op/v0/device/scheduler/enable", s.opAuth(s.handleSchedulerEnable))
	mux.HandleFunc("POST /basic/v0/user/login", s.handleLogin)
	mux.HandleFunc("GET /dew/v0/device/detail", s.dewAuth(s.handleBatteryDetail))
	mux.HandleFunc("GET /dew/v0/wsmaitian", s.handleWS)
	return mux
}

// opAuth guards the /op/v0 endpoints: the api key in the "token" header
// (the integration's client) or the "api_key" header (alias). Missing or
// wrong → HTTP 401 (the integration treats non-2xx as fatal and keeps
// its last-known data).
func (s *Server) opAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !secretEqual(r.Header.Get("Token"), s.cfg.APIKey) && !secretEqual(r.Header.Get("Api_key"), s.cfg.APIKey) {
			writeEnvelope(w, http.StatusUnauthorized, errEnvelope(errnoUnauthorized, "unauthorized"))
			return
		}
		defer drainBody(r)
		next(w, r)
	}
}

// dewAuth guards the /dew/v0 endpoints: the web-session token in the
// "token" header or query. (Stricter than the reference simulator, which
// does no WS auth; the surface is LAN-bound.)
func (s *Server) dewAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.dewAuthed(r) {
			writeEnvelope(w, http.StatusUnauthorized, errEnvelope(errnoUnauthorized, "unauthorized"))
			return
		}
		defer drainBody(r)
		next(w, r)
	}
}

// dewAuthed checks the web-session token (header or query).
func (s *Server) dewAuthed(r *http.Request) bool {
	tok := r.Header.Get("Token")
	if tok == "" {
		tok = r.URL.Query().Get("token")
	}
	return secretEqual(tok, s.webToken())
}

// webToken is the web-session token: configured, or random per start.
func (s *Server) webToken() string { return s.cfg.WebToken }

// webTokenBytes is the random web token's entropy (256 bits).
const webTokenBytes = 32

// secretEqual compares a presented secret in constant time (audit S7).
func secretEqual(got, want string) bool {
	return want != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// drainBody discards an unread request body (connection reuse hygiene).
func drainBody(r *http.Request) {
	if r.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, r.Body)
	_ = r.Body.Close()
}

// handlePlantList returns the configured plant record (the integration's
// discovery step 1).
func (s *Server) handlePlantList(w http.ResponseWriter, _ *http.Request) {
	writeEnvelope(w, http.StatusOK, okEnvelope(plantList{Data: []plantRecord{
		{StationID: s.cfg.Plant.StationID},
	}}))
}

// handleDeviceList returns the observed inverter SN (empty before the
// first identity snapshot).
func (s *Server) handleDeviceList(w http.ResponseWriter, _ *http.Request) {
	data := []deviceRecord{}
	if id := s.deviceID(); id.SN != "" {
		data = []deviceRecord{{DeviceSN: id.SN}}
	}
	writeEnvelope(w, http.StatusOK, okEnvelope(deviceList{Data: data}))
}

// handleDeviceDetail returns the detail record the integration requires
// (capacity > 0 is load-bearing: it derives fdPwr from it).
func (s *Server) handleDeviceDetail(w http.ResponseWriter, _ *http.Request) {
	id := s.deviceID()
	writeEnvelope(w, http.StatusOK, okEnvelope(deviceDetail{
		Capacity:   s.cfg.Plant.CapacityKW,
		DeviceType: id.Model,
		DeviceSN:   id.SN,
		Firmware:   id.Firmware,
	}))
}

// handleRealQuery maps the requested variables onto the decoded state:
// backed variables carry the tele/snap61 value, the rest are absent
// (reference-simulator parity — the integration tolerates absence).
func (s *Server) handleRealQuery(w http.ResponseWriter, r *http.Request) {
	var req realQueryReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeEnvelope(w, http.StatusOK, errEnvelope(errnoInvalidValue, "invalid request"))
		return
	}
	st, err := s.state()
	if err != nil {
		writeEnvelope(w, http.StatusOK, errEnvelope(errnoInvalidValue, "store: "+err.Error()))
		return
	}
	datas := []variableValue{}
	for _, name := range req.Variables {
		src, ok := variables[name]
		if !ok {
			continue
		}
		if v, ok := src(st); ok {
			datas = append(datas, variableValue{Variable: name, Value: v})
		}
	}
	writeEnvelope(w, http.StatusOK, okEnvelope([]deviceRealTime{{Datas: datas, DeviceSN: req.SN}}))
}

// handleSocGet returns the sim-local min-SoC pair (see handleSocSet for
// the recon-gap note).
func (s *Server) handleSocGet(w http.ResponseWriter, _ *http.Request) {
	s.socMu.Lock()
	pair := minSocSettings{MinSoc: s.minSoc, MinSocOnGrid: s.minSocOnGrid}
	s.socMu.Unlock()
	writeEnvelope(w, http.StatusOK, okEnvelope(pair))
}

// handleSocSet updates the sim-local min-SoC pair. Recon gap (profile §8):
// no decoded register carries the global min-SoC, so the setting is
// accepted locally with one log line and one bus alert — it is NOT
// forwarded to the inverter (the gateway is never touched).
func (s *Server) handleSocSet(w http.ResponseWriter, r *http.Request) {
	var req minSocSettings
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeEnvelope(w, http.StatusOK, errEnvelope(errnoInvalidValue, "invalid request"))
		return
	}
	if req.MinSoc < opDisable || req.MinSoc > socBound || req.MinSocOnGrid < opDisable || req.MinSocOnGrid > socBound {
		writeEnvelope(w, http.StatusOK, errEnvelope(errnoInvalidValue, "minSoc/minSocOnGrid must be 0..100"))
		return
	}
	s.socMu.Lock()
	s.minSoc = req.MinSoc
	s.minSocOnGrid = req.MinSocOnGrid
	s.socMu.Unlock()
	slog.Warn("sim: min-SoC setting accepted locally, not forwarded to the inverter (recon gap: no mapped register)",
		"minSoc", req.MinSoc, "minSocOnGrid", req.MinSocOnGrid)
	s.alert(fmt.Sprintf("sim: min-SoC %d (on-grid) / %d (total) accepted locally (not forwarded; recon gap: no mapped register)",
		req.MinSocOnGrid, req.MinSoc))
	writeEnvelope(w, http.StatusOK, okEnvelope(nil))
}

// handleLogin issues the static web token (LAN-bound; the credentials in
// the body are ignored — reference-simulator shape).
func (s *Server) handleLogin(w http.ResponseWriter, _ *http.Request) {
	writeEnvelope(w, http.StatusOK, okEnvelope(loginResult{Token: s.webToken()}))
}

// handleBatteryDetail returns the empty battery object: the battery
// temperature frame field is not yet decoded (profile §8 recon gap), so
// the integration's .get() chain degrades to "no BMS temperature".
func (s *Server) handleBatteryDetail(w http.ResponseWriter, _ *http.Request) {
	writeEnvelope(w, http.StatusOK, okEnvelope(batteryDetail{}))
}

// Response payload shapes (the integration's parsers).
type plantRecord struct {
	StationID string `json:"stationID"`
}

type plantList struct {
	Data []plantRecord `json:"data"`
}

type deviceRecord struct {
	DeviceSN string `json:"deviceSN"`
}

type deviceList struct {
	Data []deviceRecord `json:"data"`
}

type deviceDetail struct {
	Capacity   int      `json:"capacity"`
	DeviceType string   `json:"deviceType"`
	DeviceSN   string   `json:"deviceSN"`
	Firmware   []string `json:"firmware"`
}

type realQueryReq struct {
	SN        string   `json:"sn"`
	Variables []string `json:"variables"`
}

type variableValue struct {
	Variable string  `json:"variable"`
	Value    float64 `json:"value"`
}

type deviceRealTime struct {
	Datas    []variableValue `json:"datas"`
	DeviceSN string          `json:"deviceSN"`
}

type minSocSettings struct {
	MinSoc       int `json:"minSoc"`
	MinSocOnGrid int `json:"minSocOnGrid"`
}

type loginResult struct {
	Token string `json:"token"`
}

// batteryDetail is intentionally empty (recon gap: no decoded BMS fields).
type batteryDetail struct {
	Battery struct{} `json:"battery"`
}

// writeEnvelope writes one cloud envelope with the given HTTP status.
func writeEnvelope(w http.ResponseWriter, status int, env envelope) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(env); err != nil {
		slog.Debug("sim: response body not written", "err", err)
	}
}
