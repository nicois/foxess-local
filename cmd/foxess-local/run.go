// run.go: the plan-2 host — embedded prox (the inverter's cloud channel
// on 14431) wired into the frame service, the command gateway, the REST
// + SSE API, the sim surface, the forward-mode machine, and the
// force-charge scheduler. One process, one stack (C-006).
package main

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	pconfig "github.com/nicois/prox/pkg/config"
	hooks "github.com/nicois/prox/pkg/hooks"
	prox "github.com/nicois/prox/pkg/prox"

	apispec "foxesslocal/api"
	"foxesslocal/deploy"
	"foxesslocal/internal/api"
	"foxesslocal/internal/certgen"
	"foxesslocal/internal/config"
	"foxesslocal/internal/device"
	"foxesslocal/internal/diskguard"
	"foxesslocal/internal/diskspace"
	"foxesslocal/internal/emudhra"
	"foxesslocal/internal/forward"
	"foxesslocal/internal/frame"
	"foxesslocal/internal/framesvc"
	"foxesslocal/internal/gateway"
	"foxesslocal/internal/guard"
	"foxesslocal/internal/liveness"
	"foxesslocal/internal/mcp"
	"foxesslocal/internal/notify"
	"foxesslocal/internal/policy"
	"foxesslocal/internal/proxpin"
	"foxesslocal/internal/scan"
	"foxesslocal/internal/settings"
	"foxesslocal/internal/sim"
	"foxesslocal/internal/store"
	"foxesslocal/internal/update"
	"foxesslocal/internal/web"
	"foxesslocal/internal/wincap"
)

// version is the build tag (-ldflags "-X main.version=…"); "dev" for a
// plain build.
var version = "dev"

// runOption customises run (the smoke injects a pre-built prox and a
// virtual clock; production passes none).
type runOption func(*runOpts)

// maxStreams caps concurrent live-update (SSE) streams: a household's
// browsers and Home Assistant need a handful (audit S6).
const maxStreams = 32

// livenessTick is how often silence is re-checked (offline within a tick
// of api.OnlineWindow).
const livenessTick = time.Second

// httpReadHeaderTimeout bounds the HTTP header read (slowloris guard).
const httpReadHeaderTimeout = 10 * time.Second

// httpShutdownTimeout bounds the graceful HTTP shutdown.
const httpShutdownTimeout = 10 * time.Second

// maintenanceInterval is the store's retention maintenance cadence.
const maintenanceInterval = 60 * time.Second

// pemFileMode is the mode for the derived PEM material (caMaterial).
const pemFileMode = 0o600

// certDirMode is the mode for the cert manifest directory (bootstrap).
const certDirMode = 0o700

// pemCertType is the PEM block type of an X.509 certificate.
const pemCertType = "CERTIFICATE"

type runOpts struct {
	prox *prox.Prox
	// storeOpen overrides store.Open (the smoke needs pool headroom:
	// a bubble reader must never park on the pool while the
	// root-group flusher holds a conn).
	storeOpen func(string, string) (*store.Store, error)
}

func withProx(p *prox.Prox) runOption {
	return func(o *runOpts) { o.prox = p }
}

func withStoreOpen(f func(string, string) (*store.Store, error)) runOption {
	return func(o *runOpts) { o.storeOpen = f }
}

// handle is the built stack (the smoke drives its ladder and REST
// surface; runCmd serves it for the process lifetime).
type handle struct {
	capt     *wincap.Capturer   // maintenance-window captures
	notifier *notify.Notifier   // outbound notifications (F2)
	grid     gridWatch          // grid lost / restored, from the inverter state
	soc      socWatch           // the battery alerts, from the state of charge
	energy   energyWatch        // the lifetime energy counters
	pprof    *http.Server       // [debug] pprof_listen; nil when off
	live     *liveness.Tracker  // online = a session up and fresh telemetry
	tok      *api.TokenStore    // flushed on shutdown (token last_used)
	set      *settings.Settings // the UI-settable settings over the config (F12)
	// scanner runs read-only device scans (F8)
	scanner *scan.Scanner
	cfg     *config.Config
	clk     func() time.Time
	st      *store.Store
	fwd     *forward.State
	maint   *forward.Maintenance
	gw      *gateway.Gateway
	svc     *framesvc.Service
	bus     *api.Bus
	sim     *sim.Server
	sched   *forward.Scheduler
	prox    *prox.Prox

	proxOwned bool // run built it (Start runs its listener)
	// fatal carries a supervised component's unrecoverable error
	// (DEF-009: the inverter listener) to runCmd, which exits non-zero.
	fatal chan error
	// pollStop/pollDone: the live-value poller's own lifecycle (it must
	// be stopped and drained before the store closes — see Shutdown).
	pollStop context.CancelFunc
	pollDone chan struct{}
	ln       net.Listener
	srv      *http.Server
	upd      *update.Service // self-upgrade (D-023)
}

// run builds the full stack from cfg (config load is the caller's).
// Order per the wiring: store → forward state → maintenance window → gateway (with
// the lazy injector that breaks the gw → svc cycle) → frame service
// (registered on the prox pipeline, observer wired to the bus) → API
// → sim → scheduler → HTTP listener. Nothing starts here: Start runs
// the background loops, Shutdown drains them (prox first, then the
// HTTP server, then the store).
// attachCapture wires the prox capture registry to the store (per-
// connection capture persistence, the plan's data path). Chunk byte
// capture (trace contribution) is opt-in and must be set before any
// stream opens — the registry decides per stream at Open (G2).
func attachCapture(p *prox.Prox, st *store.Store, chunkBytesHours int) {
	if chunkBytesHours > 0 {
		p.Capture.EnableBytes()
	}
	st.AttachRegistry(p.Capture)
}

func run(cfg *config.Config, opts ...runOption) (*handle, error) {
	o := &runOpts{}
	for _, f := range opts {
		f(o)
	}
	clk := time.Now

	st, err := openStore(cfg.Store.Path, o)
	if err != nil {
		return nil, err
	}

	// The installer gate, before the heavy setup (see tokenStoreOpen).
	tok, err := tokenStoreOpen(cfg.Tokens.Path, clk)
	if err != nil {
		_ = st.Close()
		return nil, err
	}

	p, proxOwned, err := proxFor(cfg, o)
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	attachCapture(p, st, cfg.Retention.ChunkBytesHours)

	set, fwd, err := buildSettings(st, cfg, clk)
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	h := &handle{cfg: cfg, clk: clk, st: st, fwd: fwd, set: set, prox: p, proxOwned: proxOwned, bus: api.NewBus()}
	h.bus.SetMaxSubscribers(maxStreams)
	h.maint = forward.NewMaintenance(st, clk, func() policy.ForwardMode {
		m, _, _ := h.fwd.Get()
		return m
	}, h.maintEmit)
	h.startStoreUpkeep()
	if err := h.buildService(p); err != nil {
		return nil, err
	}
	h.reconnectOnModeChange()

	h.sim, err = h.buildSim(cfg)
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	h.sched, err = h.buildSched(cfg)
	if err != nil {
		_ = st.Close()
		return nil, err
	}

	h.tok = tok
	if err := h.buildHTTP(cfg, tok); err != nil {
		_ = st.Close()
		return nil, err
	}
	return h, nil
}

// buildHTTP binds the REST/UI listener and builds its server: the
// LAN-only source guard, then the Host check (DNS rebinding — audit S6),
// then the routes. Timeouts: the header read (slowloris) and idle
// keep-alives (audit R7); no ReadTimeout or WriteTimeout — either would
// cut the event stream, and request bodies are size-limited by the
// handlers.
func (h *handle) buildHTTP(cfg *config.Config, tok *api.TokenStore) error {
	ln, err := net.Listen("tcp", cfg.Server.HTTPListen)
	if err != nil {
		return err
	}
	h.ln = ln
	hostname, _ := os.Hostname()
	h.srv = &http.Server{
		Handler: guard.Wrapper(cfg.Net.AllowPublic, netExtra(cfg.Net.ExtraAllowed))(
			guard.HostCheck(hostname, cfg.Net.HostNames)(h.buildMux(cfg, tok))),
		ReadHeaderTimeout: httpReadHeaderTimeout,
		IdleTimeout:       httpIdleTimeout,
	}
	return nil
}

// httpIdleTimeout closes idle keep-alive connections (audit R7).
const httpIdleTimeout = 2 * time.Minute

// releaseRepo is the GitHub "owner/name" this binary's releases come
// from — stamped by `make release` (-X main.releaseRepo=…); empty in a
// source build. [update] repo overrides it.
var releaseRepo = ""

// updateHTTPTimeout bounds each release request (the binary is ~15 MB).
const updateHTTPTimeout = 2 * time.Minute

// buildUpdate assembles the self-upgrade service (D-023): the release
// key embedded in this binary, the repository, and this binary's path.
func buildUpdate(cfg *config.Config) *update.Service {
	exe, err := os.Executable()
	if err == nil {
		if r, err := filepath.EvalSymlinks(exe); err == nil {
			exe = r
		}
	}
	var key *update.PublicKey
	if k := strings.TrimSpace(string(deploy.ReleaseKey)); k != "" {
		if pk, err := update.ParsePublicKey(k); err == nil {
			key = &pk
		} else {
			slog.Warn("update: the embedded release key does not parse; self-upgrade disabled", "err", err)
		}
	}
	repo := cfg.Update.Repo
	if repo == "" {
		repo = releaseRepo
	}
	return &update.Service{
		U: &update.Updater{
			// FOXESS_UPDATE_API points the release check at a mirror (or a
			// test server); the signature check is unaffected
			Repo: repo, API: os.Getenv("FOXESS_UPDATE_API"), Client: &http.Client{Timeout: updateHTTPTimeout}, Key: key,
			Current: version, Arch: runtime.GOARCH, Exe: exe,
			FreeSpace: diskspace.Free, MinFree: minFreeBytes(cfg),
		},
		CheckEnabled: cfg.Update.CheckEnabled(),
		Container:    os.Getenv("FOXESS_CONTAINER") != "",
	}
}

// minFreeBytes is the configured critical free-space level in bytes.
func minFreeBytes(cfg *config.Config) uint64 {
	return uint64(cfg.Retention.MinFreeMB) * diskspace.MiB //nolint:gosec // G115: validated >= 0
}

// storeDir is the directory holding the database (its filesystem is the
// one the disk guard watches).
func (h *handle) storeDir() string { return filepath.Dir(h.cfg.Store.Path) }

// diskGuard discards the oldest raw data when the store's filesystem is
// critically low (hourly), alerting the UI.
func (h *handle) diskGuard() *diskguard.Guard {
	dir := h.storeDir()
	return &diskguard.Guard{
		St:      h.st,
		Space:   func() (uint64, uint64, error) { return diskspace.Free(dir) },
		MinFree: minFreeBytes(h.cfg),
		Now:     h.clk,
		Alert: func(msg string) {
			h.bus.PublishSystem(api.SystemEvent{Type: api.SystemAlert, Detail: msg})
		},
	}
}

// startStoreUpkeep starts the retention pass, converts an old database
// to incremental vacuum (see enableVacuum), and installs the liveness
// tracker and the settings-change hook (watchLiveness).
func (h *handle) startStoreUpkeep() {
	h.watchLiveness()
	h.st.StartMaintenance(maintenanceInterval, store.MaintainConfig{
		RawHours:        h.cfg.Retention.RawHours,
		BucketDays:      h.cfg.Retention.BucketDays,
		ChunkBytesHours: h.cfg.Retention.ChunkBytesHours,
	})
	h.enableVacuum()
}

// enableVacuum converts a database created before incremental
// auto-vacuum, once, when the disk has room for the rewrite (so the disk
// guard's discards give space back to the filesystem).
func (h *handle) enableVacuum() {
	free, _, err := diskspace.Free(h.storeDir())
	if err != nil {
		return
	}
	t0 := time.Now()
	if ok, err := h.st.EnableIncrementalVacuum(free); err != nil {
		slog.Warn("store: enabling incremental vacuum failed", "err", err)
	} else if ok {
		slog.Info("store: enabled incremental vacuum (one-time rewrite)", "took", time.Since(t0).Round(time.Millisecond))
	}
}

// netExtra parses the configured extra CIDRs (validate() guarantees
// the entries parse).
func netExtra(cidrs []string) []netip.Prefix {
	var out []netip.Prefix
	for _, c := range cidrs {
		out = append(out, netip.MustParsePrefix(c))
	}
	return out
}

// openStore opens the store, or the smoke's pool-headroom override
// (a bubble reader must never park on the pool while the
// root-group flusher holds a conn).
// appVersion is the composed version stamp (build tag + conformance
// pin) — the single value the store rows, the API info, and the
// startup log agree on (release traceability, D-018).
func appVersion() string {
	return fmt.Sprintf("%s-prox-%s", version, proxpin.Short())
}

func openStore(path string, o *runOpts) (*store.Store, error) {
	open := o.storeOpen
	if open == nil {
		open = store.Open
	}
	return open(path, appVersion())
}

// tokenStoreOpen opens the token store and enforces the installer gate:
// a persisted store without an installer-role token is a lockout (token
// minting is installer-only), so run fails fast with the recovery
// command. The in-memory mode (path "") skips the gate — its tokens
// are a dev convenience, not a production surface.
func tokenStoreOpen(path string, clk func() time.Time) (*api.TokenStore, error) {
	ts, err := api.NewTokenStore(path, clk)
	if err != nil {
		return nil, err
	}
	if path != "" && !ts.HasInstaller() {
		return nil, errors.New("no installer token: the service will not start until one exists — " +
			"run `sudo -u foxess <binary> tokens create -name <name> -role installer` with the service stopped " +
			"(e.g. sudo -u foxess /var/lib/foxess-local/bin/foxess-local), then start")
	}
	return ts, nil
}

// buildService builds the gateway (the sole upstream→inverter write
// path, C-003) and the frame service; the lazy injector breaks the
// gw → svc → gw cycle (the method dereferences the service slot once
// it is built).
func (h *handle) buildService(p *prox.Prox) error {
	budget, err := budgetOptions(h.cfg.Writes)
	if err != nil {
		return err
	}
	var svc *framesvc.Service
	// C-008: every write (API, simulator, scheduler) is put to the
	// device view first; reads (the poller, the probe) are never gated.
	// inverter wear: read before every write and skip what it already
	// holds; cap real writes per setting ([writes])
	opts := append([]gateway.Option{
		gateway.WithObserver(h.gwObs),
		gateway.WithAuthorizer(device.GatewayAuthorizer(h.st)),
		gateway.WithSkipUnchanged(),
	}, budget...)
	h.gw = gateway.New(&svcInjector{p: &svc}, h.clk, opts...)
	h.capt = wincap.New(filepath.Join(h.storeDir(), "captures"),
		int64(h.cfg.Retention.CaptureMaxMB)*diskspace.MiB, h.clk, appVersion())
	svc = framesvc.New(p.Ladder.Hooks, h.st, h.gw, h.posture, h.clk,
		framesvc.WithServePolicy(servePolicy{h: h}),
		framesvc.WithCapture(h.capt.Chunk),
		framesvc.WithObserver(h.svcObs)) // publishes to the injector's slot via svc
	h.svc = svc
	return nil
}

// buildMux builds the HTTP mux: the self-owned API under /api/v1 (plus
// /healthz), the sim's own prefixes when the sim is enabled (C-005:
// the sim is a separate surface, not a subpath of /api/v1), and the
// web UI as the / catch-all LAST (task 11) — the Go 1.22+ pattern-
// matched mux keeps the specific routes winning over it.
func (h *handle) buildMux(cfg *config.Config, tok *api.TokenStore) http.Handler {
	h.upd = buildUpdate(cfg)
	// an upgrade is confirmed once the inverter session is back with data
	// stored, and fails the process if it never comes (install review G2)
	h.upd.Healthy = func() bool { return h.live != nil && h.live.Online() && h.st.Err() == nil }
	h.upd.Fail = func(reason string) {
		select {
		case h.fatal <- errors.New(reason):
		default:
		}
	}
	info := api.ServerInfo{
		Version:    appVersion(),
		RawHours:   cfg.Retention.RawHours,
		BucketDays: cfg.Retention.BucketDays,
		Online:     h.onlineFunc(),
		Energy:     h.energy.energy,
		IssueURL:   issueURL(cfg),
		// a handle built without settings (tests): the config's values
		PublicReadOnly:    cfg.Access.PublicReadOnly,
		PublicAllPages:    cfg.Access.PublicAllPages,
		BatteryCapacityWh: cfg.Battery.CapacityWh,
	}
	if h.set != nil { // never a typed nil in the interface
		info.Settings = h.set
	}
	// read-only device scans (F8): operator-origin reads through the gateway
	h.scanner = scan.New(scan.GatewayReader(h.gw, scanOrigin), h.clk)
	h.notifier = notify.New(h.notifyTargets, h.clk, notify.WithOutbox(storeOutbox{h.st}))
	apiH := api.New(h.st, h.gw, h.maint, tok, h.fwd, h.bus, h.clk, api.WithInfo(info), api.WithUpdate(h.upd), api.WithScanner(h.scanner), api.WithCaptures(h.capt),
		api.WithReadyChecks(h.readyChecks), api.WithNotifier(h.notifier))
	mux := http.NewServeMux()
	mux.Handle("/api/v1/{rest...}", apiH)
	mux.Handle("/healthz", apiH)
	mux.Handle("/readyz", apiH)
	mux.Handle("/metrics", apiH)
	// the MCP service: tools over this API, with the caller's token
	if m, err := mcp.New(apiH, apispec.OpenAPI, appVersion()); err != nil {
		slog.Error("mcp: not served", "err", err) // the embedded spec is checked by tests; a defect
	} else {
		mux.Handle("/mcp", m) // POST; other methods get its 405
	}
	if h.sim != nil {
		for _, prefix := range []string{"/op/v0", "/basic/v0", "/dew/v0"} {
			mux.Handle(prefix+"/{rest...}", h.sim.Handler())
		}
	}
	mux.Handle("/", web.Handler())
	return mux
}

// buildSim builds the sim server when enabled (nil when disabled); it
// publishes to the bus through the emit callback.
func (h *handle) buildSim(cfg *config.Config) (*sim.Server, error) {
	if !cfg.Sim.Enabled {
		return nil, nil //nolint:nilnil // nil = the sim is off (the caller checks)
	}
	return sim.New(sim.Config{
		APIKey:       cfg.Sim.APIKey,
		Plant:        sim.Plant{StationID: cfg.Sim.StationID, CapacityKW: cfg.Sim.CapacityKW},
		WebToken:     cfg.Sim.WebToken,
		MinSoc:       cfg.Sim.MinSoc,
		MinSocOnGrid: cfg.Sim.MinSocOnGrid,
	}, h.st, h.gw, simAlert(h.bus), h.clk)
}

// simAlert publishes the sim's alerts as system alerts (the web UI and
// the notifier take them like any other).
func simAlert(bus *api.Bus) sim.Alert {
	return func(detail string) {
		bus.PublishSystem(api.SystemEvent{Type: api.SystemAlert, Detail: detail})
	}
}

// buildSched builds the forward-window scheduler when windows are
// configured (nil otherwise).
func (h *handle) buildSched(cfg *config.Config) (*forward.Scheduler, error) {
	if len(cfg.Scheduler.Windows) == 0 {
		return nil, nil //nolint:nilnil // nil = no windows configured (the caller checks)
	}
	s, err := forward.NewScheduler(h.st, h.gw, h.clk, cfg.Scheduler.ForwardWindows())
	if err != nil {
		return nil, err
	}
	s.OnAlert(func(msg string) {
		h.bus.PublishSystem(api.SystemEvent{Type: api.SystemAlert, Detail: msg})
	})
	return s, nil
}

// buildProx builds the run-owned embedded prox (see proxConfig).
func buildProx(cfg *config.Config) (*prox.Prox, error) {
	pcfg, err := proxConfig(cfg)
	if err != nil {
		return nil, err
	}
	return prox.New(pcfg)
}

// proxConfig builds the embedded prox config: the CA material from the
// certgen output, the tproxy port list from the inverter listen
// address, and store-owned persistence (DataRoot "").
func proxConfig(cfg *config.Config) (*pconfig.Config, error) {
	ca, err := caMaterial(cfg)
	if err != nil {
		return nil, err
	}
	ports, err := inverterPorts(cfg.Server.InverterListen)
	if err != nil {
		return nil, err
	}
	pcfg := pconfig.Default()
	pcfg.Listen = cfg.Server.InverterListen
	pcfg.Ports = ports
	pcfg.DataRoot = "" // embedded: the foxess store owns persistence
	pcfg.CAPaths = ca
	pcfg.Isolation = "off"
	pcfg.Rules.Mode = "consumer" // the gate owns all TPROXY rules (C-007)
	upstream, err := upstreamCAs(cfg)
	if err != nil {
		return nil, err
	}
	pcfg.MITM.UpstreamCAs = upstream
	return pcfg, nil
}

// upstreamCAs returns the roots trusted on the upstream (real cloud)
// leg: forward.upstream_cas when set, else the embedded vendor cloud
// root materialised next to the cert manifest (prox takes PEM paths).
func upstreamCAs(cfg *config.Config) ([]string, error) {
	if len(cfg.Forward.UpstreamCAs) > 0 {
		return cfg.Forward.UpstreamCAs, nil
	}
	dir := filepath.Dir(cfg.Certs.Manifest)
	if err := os.MkdirAll(dir, certDirMode); err != nil {
		return nil, fmt.Errorf("run: cert dir %s: %w", dir, err)
	}
	p := filepath.Join(dir, "cloud-root.pem")
	if err := os.WriteFile(p, deploy.CloudRootPEM, pemFileMode); err != nil {
		return nil, fmt.Errorf("run: %s: %w", p, err)
	}
	return []string{p}, nil
}

// posture maps the forward-mode state to the policy posture (the gate
// is live: an operator mode change re-decides the next frame; an
// active maintenance window overrides everything — both directions).
func (h *handle) posture() policy.Posture {
	mode, _, err := h.fwd.Get()
	if err != nil {
		return policy.Posture{Mode: policy.ModeOff}
	}
	return policy.Posture{Mode: mode, Window: h.maint.Active()}
}

// servePolicy decides the per-connection cloud leg: an active
// maintenance window mirrors everything (the unfettered leg),
// otherwise the forward-mode state decides.
type servePolicy struct{ h *handle }

func (sp servePolicy) Decide(c *hooks.Conn) (framesvc.ServeMode, framesvc.ServeLeg, string, error) {
	if sp.h.maint.Active() {
		return framesvc.ServeMirror, nil, "maintenance window: mirror", nil
	}
	return sp.h.fwd.Decide(c)
}

// maintEmit is the maintenance machine's event sink: the transition
// events (opened/extended/expired/closed) go to the bus as system
// events, and a transition that changes the serve decision ends the
// live session so it applies at once (windowReconnect). It runs under
// the machine's lock: Reconnect only marks the session (it closes at
// its next chunk), so the next Decide's Active() does not deadlock.
func (h *handle) maintEmit(event, detail string) {
	h.bus.PublishSystem(api.SystemEvent{Type: event, Detail: detail})
	if h.svc == nil || h.fwd == nil {
		return
	}
	mode, _, err := h.fwd.Get()
	if err != nil {
		return
	}
	if reason, ok := windowReconnect(event, mode); ok {
		h.svc.Reconnect(reason)
	}
}

// windowReconnect: whether a maintenance-window transition must end the
// live session, and why. The serve decision is per connection and only
// off mode serves locally (no cloud leg) — so in off, opening a window
// must reconnect for the cloud to get its leg, and closing or expiry
// must reconnect to take it away. telemetry_only / full sessions are
// mirrored either way (the window acts per frame); a re-arm changes
// nothing.
func windowReconnect(event string, mode policy.ForwardMode) (string, bool) {
	if mode != policy.ModeOff {
		return "", false
	}
	switch event {
	case forward.EventMaintenanceOpened:
		return "maintenance window opened in off mode: reconnecting with a cloud leg", true
	case forward.EventMaintenanceClosed, forward.EventMaintenanceExpired:
		return "maintenance window " + strings.TrimPrefix(event, "maintenance-") + " in off mode: reconnecting without the cloud", true
	}
	return "", false
}

// ServeHTTP starts the REST server. It is split from Start so the
// socket server can come up (or be driven in-process by tests) without
// starting the clocked background loops, which stay in Start.
func (h *handle) ServeHTTP() {
	if a := h.cfg.Debug.PprofListen; a != "" && h.pprof == nil {
		// a diagnostic: a bind failure is logged, never fatal
		if srv, _, err := startPprof(a); err != nil {
			slog.Warn("pprof: not started", "err", err)
		} else {
			h.pprof = srv
		}
	}
	go func() {
		if err := h.srv.Serve(h.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(os.Stderr, "http:", err)
		}
	}()
}

// Start runs the clocked background loops under ctx (the prox listener
// — production only; the smoke drives the ladder directly — the
// maintenance window's expiry loop, the scheduler, the sim websocket
// push). Callers that need the HTTP server
// call ServeHTTP separately.
func (h *handle) Start(ctx context.Context) {
	if h.fatal == nil {
		h.fatal = make(chan error, 1)
	}
	if h.proxOwned {
		run := runListener(h) // resolved now, not in the goroutine (the race detector: tests swap it)
		go func() {
			if err := superviseListener(ctx, run); err != nil {
				select {
				case h.fatal <- err:
				default:
				}
			}
		}()
	}
	if h.proxOwned && h.svc != nil {
		h.startPoller(ctx, h.svc.RunPoller)
	}
	if h.sched != nil {
		go h.sched.Run(ctx)
	}
	go h.maint.Run(ctx)
	go h.live.Run(ctx, livenessTick)
	go h.diskGuard().Run(ctx)
	h.startNotify(ctx)
	h.startCaptures(ctx)
	h.seedEnergy()
	if h.upd != nil {
		go h.upd.Run(ctx)
	}
	if h.sim != nil {
		go h.sim.Start(ctx)
	}
}

// startPoller runs the live-value poller on its own cancellable context
// (Shutdown stops it explicitly: on the fatal path the root ctx is
// still live when Shutdown runs).
func (h *handle) startPoller(ctx context.Context, run func(context.Context)) {
	pctx, cancel := context.WithCancel(ctx)
	h.pollStop = cancel
	h.pollDone = make(chan struct{})
	go func() {
		defer close(h.pollDone)
		run(pctx)
	}()
}

// stopPoller cancels the poller and waits for it to exit (no-op when
// it never started; idempotent).
func (h *handle) stopPoller() {
	if h.pollStop == nil {
		return
	}
	h.pollStop()
	<-h.pollDone
}

// Shutdown drains in the order that protects the data path: the prox
// first (no more cloud-channel flows once its listener is down), then
// the HTTP server (in-flight requests), then the store (the last
// writer). The store's host contract is quiesce-before-Close: every
// finished stream's persistStream goroutine must have recorded its
// rows before Close — Record* after Close panics (DEF-007, live 2026-
// 11-15 10:54:32, systemctl stop with a live inverter session). The
// prox's settle phase guarantees that once its Shutdown returns nil;
// its error names the connections still in flight, on which the wait
// would hang on live streams, so it is skipped then.
// The scheduler and the sim exit on their own ctx.
func (h *handle) Shutdown(ctx context.Context) error {
	h.stopPoller() // a store writer: drained before anything closes
	quiesced := !h.proxOwned
	if h.proxOwned {
		if err := h.prox.Shutdown(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "prox shutdown:", err)
			quiesced = false
		}
	}
	sctx, cancel := context.WithTimeout(ctx, httpShutdownTimeout)
	defer cancel()
	if err := h.srv.Shutdown(sctx); err != nil {
		fmt.Fprintln(os.Stderr, "http shutdown:", err)
	}
	if h.pprof != nil {
		_ = h.pprof.Shutdown(sctx)
	}
	if quiesced {
		// All streams are finished (or there is no prox-owned drain),
		// so the wait terminates.
		h.st.WaitStreams()
	}
	if h.scanner != nil {
		h.scanner.Close()
	}
	if h.svc != nil {
		h.svc.Quiesce() // the bootstrap emitters: no store write after Close (DEF-008)
	}
	if h.tok != nil {
		if err := h.tok.FlushUsage(); err != nil {
			fmt.Fprintln(os.Stderr, "tokens: saving last-used times:", err)
		}
	}
	// the clean-stop marker: the next start knows this was no power cut
	h.st.Flush()
	if err := h.st.SetSetting(cleanStopKey, h.clk().UTC().Format(time.RFC3339)); err != nil {
		fmt.Fprintln(os.Stderr, "clean-stop marker:", err)
	}
	return h.st.Close()
}

// svcInjector breaks the gateway → service cycle: the service holds the
// gateway (the sole upstream path), the gateway holds this injector,
// and the method dereferences the service slot once it is built. It
// never calls back into the gateway (the gateway calls it under its
// lock — a callback would deadlock).
type svcInjector struct {
	p **framesvc.Service
}

func (i *svcInjector) InjectS2C(connID string, raw []byte) error {
	if i.p == nil || *i.p == nil {
		return errors.New("gateway: service not ready")
	}
	return (*i.p).InjectS2C(connID, raw)
}

// ---------- the observer → bus adapters (the spec's SSE map) ----------

// svcObs maps framesvc events onto the bus. It runs on the hook
// goroutine: bus.Publish is the whole body — no gateway, no service, no
// store queries that can stall the relay.
func (h *handle) svcObs(ev framesvc.Event) {
	switch ev.Kind {
	// online/offline: the liveness tracker publishes the transitions (a
	// session up and fresh telemetry — the same answer /state gives); a
	// failed TLS needs nothing here: its session-down follows.
	case framesvc.EventSessionUp:
		h.live.SessionUp(ev.ConnID)
	case framesvc.EventSessionDown:
		h.live.SessionDown(ev.ConnID)
	case framesvc.EventTelemetry:
		h.live.Telemetry() // online (if it was not) before the sample
		if gev, ok := h.grid.observe(ev.Tele.InvState, h.clk()); ok {
			h.bus.PublishSystem(gev)
		}
		h.observeSoc(ev.Tele.Soc, h.clk())
		h.bus.PublishSnapshot(api.SnapshotEvent{
			TS: h.clk(),
			Power: api.Power{
				PVW: int(ev.Tele.Sol), GridW: int(ev.Tele.Grid),
				LoadW: int(ev.Tele.Haus), AcPortW: int(ev.Tele.Acport),
				BatteryW: int(ev.Tele.Batt), EpsW: int(ev.Tele.Eps),
			},
			SocPct:       int(ev.Tele.Soc),
			BatteryState: teleBatteryState(ev.Tele),
			Energy:       h.snapshotEnergy(),
		})
	case framesvc.EventEnergy:
		for _, f := range h.energy.observe(*ev.Energy, h.clk()) {
			h.bus.PublishSystem(api.SystemEvent{Type: api.SystemCounterReset, Detail: f})
		}
	case framesvc.EventWrite:
		h.publishConfig(api.SectionsOf(writtenAddrs(ev.PDU)))
	}
}

// gwObs maps a command's terminal state onto the bus (acked → the
// config event "confirmed register change — ours"; failed/expired → the
// alert).
func (h *handle) gwObs(res gateway.Result) {
	// Reads change no configuration (no config event); a failed poll is
	// retried next tick (no alert — an outage must not storm the feed).
	if res.Kind == gateway.KindRead &&
		(res.Status == gateway.StatusAcked || res.Origin == gateway.OriginPoll) {
		return
	}
	switch res.Status {
	case gateway.StatusAcked:
		if sec := commandSection(res.Kind); sec != "" {
			h.publishConfig([]string{sec})
		}
	case gateway.StatusFailed, gateway.StatusExpired:
		if res.Kind != gateway.KindRead {
			// the write may have been sent (it counts): push the budget
			h.publishConfig([]string{api.SectionWriteBudget})
		}
		detail := res.Detail
		if detail == "" {
			detail = string(res.Status)
		}
		h.bus.PublishSystem(api.SystemEvent{
			Type:   api.SystemAlert,
			Detail: fmt.Sprintf("command %s %s: %s", res.ID, res.Status, detail),
		})
	case gateway.StatusQueued, gateway.StatusInFlight:
		// non-terminal: the observer fires exactly once, at terminal
	}
}

// state builds the config event's full State snapshot ("" when the
// build fails — the Changed list still names the register).
func (h *handle) state() api.State {
	st, err := api.StateFor(h.st, h.fwd, h.clk, api.ServerInfo{
		BatteryCapacityWh: h.capacityWh(), Online: h.onlineFunc(), Energy: h.energy.energy, WriteBudget: h.writeBudget,
	})
	if err != nil {
		return api.State{TS: h.clk()}
	}
	return st
}

// watchLiveness builds the liveness tracker (its transitions are the
// SSE connection events) and pushes settings the poll found changed
// outside this app (the inverter's panel) as config events.
func (h *handle) watchLiveness() {
	h.live = liveness.New(api.OnlineWindow, h.clk, func(online bool) {
		state := "offline"
		if online {
			state = "online"
		}
		h.bus.PublishConnection(api.ConnectionEvent{State: state})
	})
	h.st.OnRegisterReadChange(func(addrs []int) { h.publishConfig(api.SectionsOf(addrs)) })
}

// onlineFunc is the tracker's answer for /state (nil without a tracker:
// a handle built without run, in tests).
func (h *handle) onlineFunc() func() bool {
	if h.live == nil {
		return nil
	}
	return h.live.Online
}

// publishConfig sends the config event for the changed sections (none:
// nothing the UI shows changed) with the full new state.
func (h *handle) publishConfig(sections []string) {
	if len(sections) == 0 {
		return
	}
	h.bus.PublishConfig(api.ConfigEvent{Changed: sections, State: h.state()})
}

// writtenAddrs lists the registers a Modbus write PDU sets.
func writtenAddrs(pdu []byte) []int {
	m, err := frame.ParseModbus(pdu)
	if err != nil {
		return nil
	}
	n := max(m.Count, 1) // fn06: one register
	out := make([]int, n)
	for i := range out {
		out[i] = m.Addr + i
	}
	return out
}

// commandSection names the state section a command kind confirms (the
// spec's ConfigEvent.changed vocabulary).
func commandSection(k gateway.CommandKind) string {
	switch k {
	case gateway.KindWorkMode:
		return api.SectionWorkMode
	case gateway.KindLimits, gateway.KindCurrentLimits, gateway.KindBatteryCurrents,
		gateway.KindPowerLimit, gateway.KindPortLimits, gateway.KindEPSMode:
		return api.SectionLimits
	case gateway.KindMeterCompensation:
		return api.SectionMeterCompensation
	case gateway.KindSchedule:
		return api.SectionSchedule
	case gateway.KindRead:
		return "" // reads change nothing
	default:
		return ""
	}
}

// teleBatteryState maps the tele battery fields to the spec's state
// vocabulary (the charge-state byte wins; the power sign is the
// fallback, mirroring the store-side batteryState).
func teleBatteryState(t *frame.Tele) string {
	switch t.ChargeState {
	case 0x04:
		return "charging"
	case 0x02:
		return "idle"
	}
	switch {
	case t.Batt > 0:
		return "charging"
	case t.Batt < 0:
		return "discharging"
	default:
		return "idle"
	}
}

// inverterPorts derives the tproxy port list from the inverter listen
// address (the cloud channel is one port; the gate scopes to it).
func inverterPorts(listen string) ([]int, error) {
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return nil, fmt.Errorf("run: server.inverter_listen %q: %w", listen, err)
	}
	var n int
	_, err = fmt.Sscanf(port, "%d", &n)
	if err != nil || n <= 0 || n > 65535 {
		return nil, fmt.Errorf("run: server.inverter_listen %q: bad port", listen)
	}
	return []int{n}, nil
}

// defaultLeafIP is the bootstrap leaf's iPAddress SAN (certgen requires
// one). Only the CA half of the bootstrap output is used at runtime —
// prox mints the inverter-facing leaf per connection — so this is the
// cloud's primary endpoint for completeness, not a configured identity.
var defaultLeafIP = netip.MustParseAddr("8.209.116.72")

// caMaterial derives the prox leaf-mint CA from the certgen output:
// T1 self-signed (cert.pem + key.pem as the one CA — the inverter
// trusts any CA) or T2 small-key (the issuer is the second PEM block of
// cert.pem, the key is the eMudhra small key). T3/T4 do not persist the
// issuer key, so they refuse at startup (the operator re-mints as T1/T2).
func caMaterial(cfg *config.Config) ([]pconfig.CAPath, error) {
	m, err := certgen.LoadManifest(cfg.Certs.Manifest)
	if errors.Is(err, os.ErrNotExist) {
		// First start without a certgen run: self-mint the T2 anchor
		// (embedded G3C public cert + small scalar k) and cache it in
		// the standard location.
		if berr := bootstrapDefaultCA(cfg); berr != nil {
			return nil, berr
		}
		m, err = certgen.LoadManifest(cfg.Certs.Manifest)
	}
	if err != nil {
		return nil, fmt.Errorf("run: certgen manifest: %w (delete the manifest to re-bootstrap, or run certgen)", err)
	}
	dir := filepath.Dir(cfg.Certs.Manifest)
	certPath := filepath.Join(dir, "cert.pem")
	certData, err := os.ReadFile(certPath) //nolint:gosec // G304: operator-configured path
	if err != nil {
		return nil, fmt.Errorf("run: %s: %w", certPath, err)
	}
	switch m.AnchorKind {
	case certgen.AnchorSelfSigned:
		return []pconfig.CAPath{{CertPEM: certPath, KeyPEM: filepath.Join(dir, "key.pem")}}, nil
	case certgen.AnchorSmallKey:
		return smallKeyMaterial(dir, certPath, certData, m.SmallKey)
	case certgen.AnchorExtracted, certgen.AnchorOwnCA:
		return nil, fmt.Errorf("run: anchor %s: the issuer key is not persisted by certgen; re-mint as selfsigned or small-key", m.AnchorKind)
	default:
		return nil, fmt.Errorf("run: unknown anchor kind %s", m.AnchorKind)
	}
}

// smallKeyMaterial derives the T2 small-key CA pair from the certgen
// output. The build writes both files non-atomically; a crash between
// the writes leaves the issuer without its key — rebuild when either
// file is missing.
func smallKeyMaterial(dir, certPath string, certData []byte, smallKey int) ([]pconfig.CAPath, error) {
	issuerPath := filepath.Join(dir, "issuer.pem")
	keyPath := filepath.Join(dir, "issuer-key.pem")
	needBuild := false
	for _, p := range []string{issuerPath, keyPath} {
		if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
			needBuild = true
		}
	}
	if needBuild {
		if err := buildSmallKeyMaterial(certPath, issuerPath, keyPath, certData, smallKey); err != nil {
			return nil, err
		}
	}
	return []pconfig.CAPath{{CertPEM: issuerPath, KeyPEM: keyPath}}, nil
}

// bootstrapDefaultCA self-mints the T2 small-key anchor when no
// certgen output exists yet: the issuer is the embedded public G3C
// certificate (its key is the small scalar k — derived, never stored),
// the SANs come from [identity]. Output lands in the standard location
// (the [certs] manifest directory) and is reused on later starts
// — the operator never handles cert files. The certgen CLI remains for
// explicit re-mints and the other anchors (T1 self-signed, T3/T4).
func bootstrapDefaultCA(cfg *config.Config) error {
	issuer, err := emudhra.G3CCert()
	if err != nil {
		return fmt.Errorf("run: bootstrap anchor: %w", err)
	}
	k, ok := emudhra.SmallKeyFor(issuer, emudhra.SmallKeys)
	if !ok {
		return fmt.Errorf("run: bootstrap anchor: embedded G3C cert is not a small-key intermediate")
	}
	certPEM, keyPEM, m, err := certgen.Generate(certgen.Options{
		Kind:       certgen.AnchorSmallKey,
		IssuerCert: issuer,
		IssuerKey:  emudhra.KeyFromSmallK(k),
		SANIPs:     []netip.Addr{defaultLeafIP},
	}, time.Now)
	if err != nil {
		return fmt.Errorf("run: bootstrap anchor: %w", err)
	}
	dir := filepath.Dir(cfg.Certs.Manifest)
	if err := os.MkdirAll(dir, certDirMode); err != nil {
		return fmt.Errorf("run: bootstrap dir %s: %w", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cert.pem"), certPEM, pemFileMode); err != nil {
		return fmt.Errorf("run: bootstrap cert.pem: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "key.pem"), keyPEM, pemFileMode); err != nil {
		return fmt.Errorf("run: bootstrap key.pem: %w", err)
	}
	if err := certgen.WriteManifest(cfg.Certs.Manifest, m); err != nil {
		return fmt.Errorf("run: bootstrap manifest: %w", err)
	}
	slog.Info("run: no certgen output — bootstrapped the T2 anchor (embedded G3C public cert, small key k) and cached it",
		"cert_manifest_dir", dir, "small_key", k)
	return nil
}

// buildSmallKeyMaterial derives the T2 small-key CA material: the
// issuer is the second CERTIFICATE block of cert.pem (parsed and
// persisted), the key is the eMudhra small key re-marshaled.
func buildSmallKeyMaterial(certPath, issuerPath, keyPath string, certData []byte, smallKey int) error {
	issuer, ok := secondCertPEM(certData)
	if !ok {
		return fmt.Errorf("run: %s: no issuer %s block (small-key anchor)", certPath, pemCertType)
	}
	block, _ := pem.Decode(issuer)
	if block == nil {
		return fmt.Errorf("run: %s: no issuer %s block (small-key anchor)", certPath, pemCertType)
	}
	if _, err := x509.ParseCertificate(block.Bytes); err != nil {
		return fmt.Errorf("run: %s: bad issuer block: %w", certPath, err)
	}
	//nolint:gosec // G304: the paths are derived from the operator's cert dir
	if err := os.WriteFile(issuerPath, issuer, pemFileMode); err != nil {
		return fmt.Errorf("run: %s: %w", issuerPath, err)
	}
	keyDER, err := x509.MarshalECPrivateKey(emudhra.KeyFromSmallK(smallKey))
	if err != nil {
		return fmt.Errorf("run: small-key %d: %w", smallKey, err)
	}

	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), pemFileMode); err != nil {
		return fmt.Errorf("run: %s: %w", keyPath, err)
	}
	return nil
}

// secondCertPEM returns the second CERTIFICATE block of a concatenated
// PEM file (the certgen T2 output: leaf + issuer).
func secondCertPEM(data []byte) ([]byte, bool) {
	rest := data
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return nil, false
		}
		if block.Type == pemCertType {
			second, _ := pem.Decode(rest)
			if second == nil || second.Type != pemCertType {
				return nil, false
			}
			return pem.EncodeToMemory(second), true
		}
	}
}

// capacityWh is the configured usable pack energy (0 when the handle was
// built without a config — the API falls back to the default).
// proxFor is the injected prox (tests), or one built from the config and
// owned by this handle.
func proxFor(cfg *config.Config, o *runOpts) (*prox.Prox, bool, error) {
	if o.prox != nil {
		return o.prox, false, nil
	}
	p, err := buildProx(cfg)
	return p, err == nil, err
}

// reconnectOnModeChange makes a forward-mode change apply at once: the
// change is persisted, then the live session is ended so the inverter
// reconnects under the new mode (the serve decision is per connection).
func (h *handle) reconnectOnModeChange() {
	if h.svc == nil || h.set == nil {
		return
	}
	h.fwd.OnChange(func(m policy.ForwardMode, _ string) error {
		err := h.set.Persist(settings.KeyForwardMode, string(m))
		h.svc.Reconnect("forward mode changed to " + string(m) + ": reconnecting under it")
		return err
	})
}

// scanOrigin marks a device scan's reads in the gateway.
const scanOrigin = "device-scan"

// issueURL is where testers file a problem report: the release
// repository's new-issue page ("" when no repository is known — a source
// build without [update] repo).
func issueURL(cfg *config.Config) string {
	repo := cfg.Update.Repo
	if repo == "" {
		repo = releaseRepo
	}
	if repo == "" {
		return ""
	}
	return "https://github.com/" + repo + "/issues/new"
}

// buildSettings loads the UI-settable settings over the config and the
// forward-mode machine on the effective mode; a mode set in the UI is
// persisted, so it survives restarts (F12).
func buildSettings(st *store.Store, cfg *config.Config, clk func() time.Time) (*settings.Settings, *forward.State, error) {
	set, err := settings.New(st, cfg, clk)
	if err != nil {
		return nil, nil, err
	}
	fwd := forward.New(st, clk, set.Get().ForwardMode)
	fwd.OnChange(func(m policy.ForwardMode, _ string) error { return set.Persist(settings.KeyForwardMode, string(m)) })
	// the write delay applies at once (owner, 2026-11-18: fragile storage)
	st.SetWriteDelay(set.Get().WriteDelay)
	set.OnChange(func(v settings.Values) { st.SetWriteDelay(v.WriteDelay) })
	return set, fwd, nil
}

func (h *handle) capacityWh() int {
	switch {
	case h.set != nil:
		return h.set.Get().CapacityWh
	case h.cfg != nil:
		return h.cfg.Battery.CapacityWh
	}
	return 0
}

// writeBudget is the gateway's budget for /state and config events.
func (h *handle) writeBudget() map[string]api.WriteBudget {
	if h.gw == nil {
		return map[string]api.WriteBudget{}
	}
	return api.BudgetView(h.gw.Budget())
}
