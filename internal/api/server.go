package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	apispec "foxesslocal/api"
	"foxesslocal/internal/forward"
	"foxesslocal/internal/gateway"
	"foxesslocal/internal/policy"
	"foxesslocal/internal/scan"
	"foxesslocal/internal/settings"
	"foxesslocal/internal/store"
	"foxesslocal/internal/wincap"
)

// commandTimeout bounds the synchronous read-back (spec: no read-back
// within the timeout → 504 command_failed).
const commandTimeout = 30 * time.Second

// OnlineWindow is the silence threshold behind /state's online flag
// (spec: treat > 15 s of silence as offline).
const OnlineWindow = 15 * time.Second

// problemType is the RFC 7807 type (the spec's type is a uri; a
// per-code URI table is needless churn for v1).
const problemType = "about:blank"

// bodyLimit bounds one request body (64 KiB — the schedule table is
// the largest).
const bodyLimit = 64 << 10

// pageLimitDefault is the spec's default page size.
const pageLimitDefault = 100

// pageLimitMax is the spec's maximum page size.
const pageLimitMax = 1000

// MaintenanceState is the maintenance-window seam (the production
// wiring passes *forward.Maintenance; the tests pass a fake). The
// machine publishes its own transition events through its sink — the
// handlers never publish.
type MaintenanceState interface {
	Open(d time.Duration, acting string) (time.Time, error)
	Close(acting string) error
	View() forward.View
	Active() bool
}

// ForwardModeState is the cloud-forward seam (task 9 replaces the
// config-backed default with the real state machine; the gate effect
// stays out of the API).
type ForwardModeState interface {
	Get() (mode ForwardMode, changedAt time.Time, err error)
	Set(mode ForwardMode, acting string) error
}

// ForwardMode is the cloud-forward mode (the spec's ForwardMode enum;
// the alias keeps the API surface and the policy engine on one type).
type ForwardMode = policy.ForwardMode

// ServerInfo is the read-only config view's server facts (task 10
// wires the real values via a variant constructor).
type ServerInfo struct {
	Version        string
	RawHours       int
	BucketDays     int
	PublicReadOnly bool `json:"public_access"`
	// PublicAllPages admits the public session beyond the dashboard's
	// endpoints ([access] public_all_pages, default off).
	PublicAllPages bool `json:"public_all_pages"`
	// BatteryCapacityWh is the pack's usable energy (0 = unknown).
	BatteryCapacityWh int
	// IssueURL is where testers file a problem report (the release
	// repository's new-issue page); "" when unknown.
	IssueURL string `json:"-"`
	// Settings is the live operator settings (internal/settings: the UI
	// overrides over the config file). nil: the static fields above.
	Settings SettingsView `json:"-"`
	// Online is the host's liveness tracker (internal/liveness: a session
	// up and fresh telemetry) — the same answer its SSE connection
	// events carry. nil: the telemetry age alone decides.
	Online func() bool `json:"-"`
	// Energy is the host's lifetime energy counters (nil: all null).
	Energy func() Energy `json:"-"`
	// WriteBudget is the host's write budget (nil: the server's gateway).
	WriteBudget func() map[string]WriteBudget `json:"-"`
}

// SettingsView is the live settings source (settings.Settings).
type SettingsView interface {
	Get() settings.Values
}

// values are the effective settings: live when a source is wired.
func (i ServerInfo) values() settings.Values {
	if i.Settings != nil {
		return i.Settings.Get()
	}
	return settings.Values{CapacityWh: i.BatteryCapacityWh, PublicReadOnly: i.PublicReadOnly, PublicAllPages: i.PublicAllPages}
}

// capacityWh is the usable pack energy (0 = unknown).
func (i ServerInfo) capacityWh() int { return i.values().CapacityWh }

// Server is the /api/v1 handler set; it is itself the http.Handler.
type Server struct {
	// readyChecks are the host's own readiness checks (WithReadyChecks).
	readyChecks func() []Check
	// routes are the registered patterns ("METHOD /path"), in order: the
	// spec parity test checks api/openapi.yaml against them.
	routes []string
	// notifier sends the test notification (WithNotifier).
	notifier NotifyTester

	st    *store.Store
	gw    *gateway.Gateway
	maint MaintenanceState
	tok   *TokenStore
	fwd   ForwardModeState
	bus   *Bus
	clock func() time.Time
	info  ServerInfo
	upd   UpdateService // nil: no self-upgrade in this host
	// scanner runs read-only device scans (F8); nil: not offered
	scanner *scan.Scanner
	// captures records maintenance windows opened with capture (nil: not
	// offered).
	captures *wincap.Capturer
	mux      *http.ServeMux
}

// ServeHTTP makes *Server an http.Handler (New returns the server; the
// host may wire it into any mux).
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// ServerOption customises the handler set at construction.
type ServerOption func(*Server)

// WithInfo overrides the /config server facts (version, retention)
// with the host's real values; the default New keeps the dev defaults.
func WithInfo(info ServerInfo) ServerOption {
	return func(s *Server) { s.info = info }
}

// New wires the /api/v1 routes (plus /healthz) into the server's mux;
// the returned server is the http.Handler.
func New(st *store.Store, gw *gateway.Gateway, maint MaintenanceState, tok *TokenStore, fwd ForwardModeState, bus *Bus, clock func() time.Time, opts ...ServerOption) *Server {
	s := &Server{
		st: st, gw: gw, maint: maint, tok: tok, fwd: fwd, bus: bus, clock: clock,
		info: ServerInfo{
			Version:        "dev",
			RawHours:       defaultRawHours,
			BucketDays:     defaultBucketDays,
			PublicReadOnly: true, // config default ([access] public_read_only, spec §5.5)
		},
	}
	for _, o := range opts {
		o(s)
	}
	mux := http.NewServeMux()
	s.routeHealth(mux)
	s.handle(mux, "GET /api/v1/state", s.protectDashboard(s.handleState))
	s.handle(mux, "GET /api/v1/compatibility", s.protect(RoleReadOnly, s.handleCompatibility))
	s.handle(mux, "GET /api/v1/write-override", s.protect(RoleReadOnly, s.handleWriteOverrideGet))
	s.handle(mux, "PUT /api/v1/write-override", s.protect(RoleInstaller, s.handleWriteOverridePut))
	s.handle(mux, "DELETE /api/v1/write-override", s.protect(RoleInstaller, s.handleWriteOverrideDelete))
	s.handle(mux, "GET /api/v1/bundle", s.protect(RoleInstaller, s.handleBundle))
	s.reportRoutes(mux)
	s.captureRoutes(mux)
	s.handle(mux, "GET /api/v1/update", s.protect(RoleReadOnly, s.handleUpdateGet))
	s.handle(mux, "POST /api/v1/update/check", s.protect(RoleInstaller, s.handleUpdateCheck))
	s.handle(mux, "POST /api/v1/update/apply", s.protect(RoleInstaller, s.handleUpdateApply))
	s.handle(mux, "DELETE /api/v1/update/rolled-back", s.protect(RoleInstaller, s.handleUpdateDismiss))
	s.handle(mux, "GET /api/v1/history", s.protectDashboard(s.handleHistory))
	s.handle(mux, "GET /api/v1/power/average", s.protectDashboard(s.handlePowerAverage))
	s.handle(mux, "PUT /api/v1/work-mode", s.protect(RoleStandard, s.handleWorkMode))
	s.handle(mux, "PUT /api/v1/schedule", s.protect(RoleStandard, s.handleSchedule))
	s.handle(mux, "PUT /api/v1/meter-compensation", s.protect(RoleExpert, s.handleMeterCompensation))
	s.handle(mux, "PUT /api/v1/limits", s.protect(RoleInstaller, s.handleLimits))
	s.handle(mux, "POST /api/v1/registers/read", s.protect(RoleInstaller, s.handleRegistersRead))
	s.handle(mux, "GET /api/v1/forward-mode", s.protectDashboard(s.handleGetForwardMode))
	s.handle(mux, "PUT /api/v1/forward-mode", s.protect(RoleInstaller, s.handlePutForwardMode))
	s.handle(mux, "GET /api/v1/audit", s.protect(RoleReadOnly, s.handleAudit))
	s.handle(mux, "GET /api/v1/messages", s.protect(RoleExpert, s.handleMessages))
	s.handle(mux, "GET /api/v1/maintenance", s.protect(RoleReadOnly, s.handleMaintenanceGet))
	s.handle(mux, "POST /api/v1/maintenance", s.protect(RoleInstaller, s.handleMaintenanceOpen))
	s.handle(mux, "DELETE /api/v1/maintenance", s.protect(RoleInstaller, s.handleMaintenanceClose))
	s.handle(mux, "GET /api/v1/config", s.protectDashboard(s.handleConfig))
	s.handle(mux, "GET /api/v1/config/tokens", s.protect(RoleInstaller, s.handleTokenList))
	s.handle(mux, "POST /api/v1/config/tokens", s.protect(RoleInstaller, s.handleTokenCreate))
	s.handle(mux, "DELETE /api/v1/config/tokens/{id}", s.protect(RoleInstaller, s.handleTokenRevoke))
	s.handle(mux, "GET /api/v1/events", s.protectDashboard(s.handleEvents))
	s.settingsRoutes(mux)
	s.inverterSettingRoutes(mux)
	s.mux = mux
	return s
}

// defaultRawHours is the dev server's raw-telemetry horizon (mirrors the
// config default, internal/config).
const defaultRawHours = 72

// defaultBucketDays is the dev server's 15-minute rollup horizon (mirrors
// the config default, internal/config).
const defaultBucketDays = 365

// publicToken identifies an unauthenticated public session (role
// read-only — exactly the floor the gate grants it). Spec §5.5.
var publicToken = Token{ID: "public", Name: "public", Role: RoleReadOnly}

// handlerFunc is an authenticated handler (the token that passed).
type handlerFunc func(w http.ResponseWriter, r *http.Request, tk Token)

// protect wraps a handler with the auth + role check (401 / 403 per
// the spec's problem+json). A read-only route admits the public session
// only with [access] public_all_pages on; protectDashboard marks the
// routes the dashboard needs.
func (s *Server) protect(required Role, h handlerFunc) http.HandlerFunc {
	return s.guard(required, false, h)
}

// protectDashboard is a read-only route the dashboard needs: the public
// session reaches it whenever public_read_only is on.
func (s *Server) protectDashboard(h handlerFunc) http.HandlerFunc {
	return s.guard(RoleReadOnly, true, h)
}

// publicAdmits reports whether a tokenless request reaches a route.
func (s *Server) publicAdmits(reqRole Role, dashboard bool) bool {
	v := s.info.values()
	return reqRole == RoleReadOnly && v.PublicReadOnly && (dashboard || v.PublicAllPages)
}

func (s *Server) guard(reqRole Role, dashboard bool, h handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tk, ok := s.authenticate(r)
		if !ok && s.publicAdmits(reqRole, dashboard) {
			// Unauthenticated read-only: admitted while the config
			// flag is on ([access] public_read_only, default on —
			// spec §5.5), beyond the dashboard only with
			// public_all_pages. Writes (required role > read-only)
			// never reach this branch and still 401.
			tk, ok = publicToken, true
		}
		if !ok {
			// failed sign-ins are visible to the owner (audit S11); a
			// missing token on a read the public session lacks is not one
			if r.Header.Get("Authorization") != "" || r.URL.Query().Get("token") != "" {
				slog.Warn("api: rejected a token", "path", r.URL.Path, "remote", r.RemoteAddr)
			}
			writeProblem(w, http.StatusUnauthorized, problem{
				Title:  "Unauthorized",
				Code:   "unauthorized",
				Detail: "missing or invalid token",
			})
			return
		}
		if roleLevel[tk.Role] < roleLevel[reqRole] {
			writeProblem(w, http.StatusForbidden, problem{
				Title:        "Insufficient role",
				Code:         "insufficient_role",
				RequiredRole: string(reqRole),
				TokenRole:    string(tk.Role),
			})
			return
		}
		h(w, r, tk)
	}
}

// authenticate resolves the bearer token: the Authorization header,
// else ?token= on GET /events only (the EventSource fallback). A
// malformed header is a 401, not a fallback.
func (s *Server) authenticate(r *http.Request) (Token, bool) {
	tok := ""
	if h := r.Header.Get("Authorization"); h != "" {
		const prefix = "Bearer "
		if !strings.HasPrefix(h, prefix) {
			return Token{}, false
		}
		tok = strings.TrimSpace(h[len(prefix):])
	} else if q := r.URL.Query().Get("token"); q != "" && r.Method == http.MethodGet && r.URL.Path == pathEvents {
		// the EventSource cannot send a header; nowhere else does a
		// token belong in a URL (history, proxy and shell logs — audit S4)
		tok = q
	}
	return s.tok.Lookup(tok)
}

// pathEvents is the SSE stream, the one endpoint that takes ?token=.
const pathEvents = "/api/v1/events"

// handleHealth is the open liveness probe (the spec's 200
// {"status":"ok"}).
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleSpec is GET /openapi.yaml (F1): the API's contract, as built
// in. Open: it describes the API, it holds no data.
func handleSpec(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/yaml")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(apispec.OpenAPI)
}

// handle registers one route and records its pattern.
func (s *Server) handle(mux *http.ServeMux, pattern string, h http.Handler) {
	s.routes = append(s.routes, pattern)
	mux.Handle(pattern, h)
}

// handleFunc is handle for a plain handler function.
func (s *Server) handleFunc(mux *http.ServeMux, pattern string, h func(http.ResponseWriter, *http.Request)) {
	s.handle(mux, pattern, http.HandlerFunc(h))
}

// routeHealth wires liveness, readiness and metrics, each at the root
// (for monitors) and under /api/v1.
func (s *Server) routeHealth(mux *http.ServeMux) {
	s.handleFunc(mux, "GET /healthz", s.handleHealth)
	s.handleFunc(mux, "GET /api/v1/healthz", s.handleHealth)
	s.handleFunc(mux, "GET /readyz", s.handleReady)
	s.handleFunc(mux, "GET /api/v1/readyz", s.handleReady)
	s.handleFunc(mux, "GET /api/v1/openapi.yaml", handleSpec)
	s.handle(mux, "GET /metrics", s.protectDashboard(s.handleMetrics))
	s.handle(mux, "GET /api/v1/metrics", s.protectDashboard(s.handleMetrics))
}

// problem is the RFC 7807 body (the spec's Problem schema): type,
// title, status are required; code is the stable machine-readable
// discriminator.
type problem struct {
	Type         string       `json:"type"`
	Title        string       `json:"title"`
	Status       int          `json:"status"`
	Detail       string       `json:"detail,omitempty"`
	Code         string       `json:"code,omitempty"`
	RequiredRole string       `json:"required_role,omitempty"`
	TokenRole    string       `json:"token_role,omitempty"`
	Errors       []fieldError `json:"errors,omitempty"`
	Applied      string       `json:"applied,omitempty"`
	// Reason refines write_unsupported: "unmapped" (no known register or
	// encoding) or "not_supported" (the inverter lacks the feature).
	Reason string `json:"reason,omitempty"`
	// write_budget: the setting, none remaining, and when the next write
	// is allowed (Retry-After carries the same).
	Setting    string     `json:"setting,omitempty"`
	Remaining  *int       `json:"remaining,omitempty"`
	NextFreeAt *time.Time `json:"next_free_at,omitempty"`
}

// fieldError is one 400 detail (the spec's errors[] entries).
type fieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// writeProblem sends one RFC 7807 error (the spec's problem+json).
func writeProblem(w http.ResponseWriter, status int, p problem) {
	if p.Type == "" {
		p.Type = problemType
	}
	p.Status = status
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	encodeBody(w, p)
}

// writeJSON sends one 2xx JSON body.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	encodeBody(w, body)
}

// encodeBody writes a response body after the header went out: a failure
// is a client that went away (or an unencodable value — a defect), and
// the status is already sent, so it is logged, not answered.
func encodeBody(w http.ResponseWriter, body any) {
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Debug("api: response body not written", "err", err)
	}
}

// readBody decodes one bounded JSON request body.
//
// It is strict: a field dst does not know, or anything after the one
// JSON value, is an error — a misspelt field must never be dropped while
// the rest of the request applies. An empty body is io.EOF (callers with
// an optional body check for it).
func readBody(r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, bodyLimit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if dec.More() {
		return errTrailingData
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errTrailingData
	}
	return nil
}

// errTrailingData refuses a body with more than one JSON value.
var errTrailingData = errors.New("unexpected data after the JSON value")

// writeNotFound sends the spec's 404 (the stable not_found code).
func writeNotFound(w http.ResponseWriter, detail string) {
	writeProblem(w, http.StatusNotFound, problem{
		Title:  "Not found",
		Code:   "not_found",
		Detail: detail,
	})
}

// badField builds one 400 invalid_value problem for a single field.
func badField(field, msg string) problem {
	return problem{
		Title:  "Invalid value",
		Code:   "invalid_value",
		Errors: []fieldError{{Field: field, Message: msg}},
	}
}

// pageParams is the spec's from/to/before/limit page (from inclusive,
// to and before exclusive, newest first).
type pageParams struct {
	since  time.Time
	until  time.Time
	before time.Time
	limit  int
}

// parsePage resolves the page parameters (a bad value is a 400
// invalid_value).
func parsePage(r *http.Request) (pageParams, problem, bool) {
	q := r.URL.Query()
	p := pageParams{limit: pageLimitDefault}
	parseTS := func(name string, dst *time.Time) bool {
		v := q.Get(name)
		if v == "" {
			return true
		}
		ts, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return false
		}
		*dst = ts
		return true
	}
	if !parseTS("from", &p.since) {
		return p, badField("from", q.Get("from")+" is not RFC 3339"), false
	}
	if !parseTS("to", &p.until) {
		return p, badField("to", q.Get("to")+" is not RFC 3339"), false
	}
	if !parseTS("before", &p.before) {
		return p, badField("before", q.Get("before")+" is not RFC 3339"), false
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > pageLimitMax {
			return p, badField("limit", v+" is not an integer in 1.."+strconv.Itoa(pageLimitMax)), false
		}
		p.limit = n
	}
	return p, problem{}, true
}

// internal sends a 500 for the unexpected (logged upstream; the spec
// has no internal code, so the RFC 7807 default title stands).
func (s *Server) internal(w http.ResponseWriter, err error) {
	// the detail goes to the log, not the client: internal errors carry
	// paths and driver text (audit S11)
	slog.Error("api: internal error", "err", err)
	writeProblem(w, http.StatusInternalServerError, problem{
		Title:  "Internal error",
		Code:   "internal",
		Detail: "an internal error occurred; the server log has the details",
	})
}
