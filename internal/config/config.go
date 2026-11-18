// Package config loads and validates foxess-local configuration.
package config

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"foxesslocal/internal/forward"
	"foxesslocal/internal/policy"

	"github.com/BurntSushi/toml"
)

// Defaults (2026-11-16 overhaul): every setting has one, so an empty
// file is a complete config. The listen ports match the gate's fixed
// tproxy handoff (14431) and the installer's REST port; the state
// layout matches the installer's /var/lib/foxess-local.
const (
	DefaultStateDir       = "/var/lib/foxess-local"
	DefaultInverterListen = "0.0.0.0:14431"
	DefaultHTTPListen     = "0.0.0.0:18080"
)

// UISettable are the keys the web UI can also set (F12): a value set
// there is stored in the database and wins over the file; InFile tells
// whether the file sets one (the source Settings shows).
var UISettable = []string{
	"battery.capacity_wh", "access.public_read_only", "access.public_all_pages", "forward.mode",
	"notify.webhook_url", "notify.ntfy_url",
	"store.write_delay_s",
	"alerts.soc_threshold_pct", "alerts.soc_horizon_min", "alerts.soc_p_value", "alerts.soc_window_min",
}

// retiredKeys were accepted by earlier releases and no longer do
// anything: they load with a warning (the installed config keeps
// them until the operator removes them) instead of failing as unknown.
var retiredKeys = map[string]string{
	"server.lan_listen": "the plan-1 LAN broker was removed",
	"identity":          "prox mints the inverter-facing leaf per connection; the bootstrap SAN is built in",
	"broker.data_dir":   "never read",
	"broker.capture":    "never read",
}

// Config is the fully validated foxess-local configuration.
type Config struct {
	// StateDir roots every persistent path not set explicitly (store,
	// tokens, certs). Default DefaultStateDir.
	StateDir string `toml:"state_dir"`
	Server   Server `toml:"server"`
	Certs    Certs  `toml:"certs"`
	// Broker is the historical name of [certs] (read for compatibility).
	Broker    Broker    `toml:"broker"`
	Retention Retention `toml:"retention"`
	Access    Access    `toml:"access"`
	Notify    Notify    `toml:"notify"`
	Alerts    Alerts    `toml:"alerts"`
	Net       Net       `toml:"net"`
	Store     Store     `toml:"store"`
	Forward   Forward   `toml:"forward"`
	Sim       Sim       `toml:"sim"`
	Tokens    Tokens    `toml:"tokens"`
	Scheduler Scheduler `toml:"scheduler"`
	Battery   Battery   `toml:"battery"`
	Update    Update    `toml:"update"`
	Debug     Debug     `toml:"debug"`

	// Warnings lists retired keys present in the file (not TOML; the
	// caller logs them at startup).
	Warnings []string `toml:"-"`
	// inFile: the UISettable keys the file sets.
	inFile map[string]bool
}

// Debug holds diagnostics, all off by default.
type Debug struct {
	// PprofListen serves the Go profiler (net/http/pprof) on this
	// address; "" = off. Loopback only: profiles and goroutine dumps
	// expose internals, so a remote machine reaches it over an SSH tunnel.
	PprofListen string `toml:"pprof_listen"`
}

// Server holds the network listen addresses (defaults: the gate's
// handoff port and the installer's REST port).
type Server struct {
	// InverterListen is the TPROXY listener the gate hands off to; its
	// port must equal the gate's tproxy target.
	InverterListen string `toml:"inverter_listen"`
	HTTPListen     string `toml:"http_listen"`
}

// Certs holds the certgen manifest location (default
// <state_dir>/certs/cert-manifest.json).
type Certs struct {
	Manifest string `toml:"manifest"`
}

// Broker is the historical section for the manifest location ([broker]
// cert_manifest, from the removed plan-1 MQTT broker): still read, with
// a warning to use [certs] manifest.
type Broker struct {
	CertManifest string `toml:"cert_manifest"`
}

// Battery describes the pack. CapacityWh is the usable energy across
// SoC 0–100 %; 0 = unknown (no built-in default — audit M6: the setup
// wizard asks, suggesting module count × module energy); usable energy is
// reported as SoC × CapacityWh when known.
type Battery struct {
	CapacityWh int `toml:"capacity_wh"`
}

// Update is the self-upgrade channel (design D-023).
type Update struct {
	// Check enables the daily release check (default true): one HTTPS
	// GET to the GitHub releases API carrying only the app version.
	// Updates are never applied without an installer's action.
	Check *bool `toml:"check"`
	// Repo is the release repository "owner/name" (default: the one this
	// binary was built for; empty in a source build).
	Repo string `toml:"repo"`
}

// CheckEnabled resolves Check's default (on).
func (u Update) CheckEnabled() bool { return u.Check == nil || *u.Check }

// Retention holds the retention policy (2026-11-12): the dense raw window
// and the 15-minute rollup horizon; connection-scoped retention is
// measured from connection close. 2026-11-13: the chunk payload byte
// window (ChunkBytesHours, 0 = off) for opt-in byte capture.
type Retention struct {
	RawHours   int `toml:"raw_hours"`
	BucketDays int `toml:"buckets_days"`
	// ChunkBytesHours: hours a finished stream's captured payload bytes
	// are kept, measured from close; 0 = off (development /
	// trace-contribution opt-in, never a default).
	ChunkBytesHours int `toml:"chunk_bytes_hours"`
	// MinFreeMB is the disk guard's critical free-space level (MB,
	// default DefaultMinFreeMB; capped at 10 % of the filesystem): below
	// it, the oldest raw data is discarded hourly, and an upgrade will
	// not download.
	MinFreeMB int `toml:"min_free_mb"`
	// CaptureMaxMB caps one maintenance window's capture (default
	// DefaultCaptureMaxMB): recording stops there.
	CaptureMaxMB int `toml:"capture_max_mb"`
}

// DefaultMinFreeMB is the default critical free-space level.
const DefaultMinFreeMB = 200

// DefaultCaptureMaxMB caps one maintenance window's capture.
const DefaultCaptureMaxMB = 256

// Access holds the access posture (2026-11-12 dashboard iteration).
type Access struct {
	PublicReadOnly bool `toml:"public_read_only"`
	// PublicAllPages: tokenless sessions (PublicReadOnly) may also read
	// the pages beyond the dashboard (schedule, control, activity, device,
	// cloud, settings, frames). Default off: the dashboard only.
	PublicAllPages bool `toml:"public_all_pages"`
}

// Notify holds the outbound notification targets (F2); each is optional
// and the web UI can set them too (a UI value wins).
type Notify struct {
	// WebhookURL receives a JSON POST per notice (e.g. a Home Assistant
	// webhook trigger: http://ha.local:8123/api/webhook/<id>).
	WebhookURL string `toml:"webhook_url"`
	// NtfyURL is an ntfy topic URL (https://ntfy.sh/<topic>, or a
	// self-hosted server).
	NtfyURL string `toml:"ntfy_url"`
}

// Net holds the exposure posture (2026-11-12 dashboard iteration).
type Net struct {
	AllowPublic  bool     `toml:"allow_public"`
	ExtraAllowed []string `toml:"extra_allowed"`
	// HostNames are extra names the web UI may be reached by (the Host
	// check refuses others: DNS rebinding). IP addresses, localhost, this
	// machine's name and .local / .lan / .home.arpa / .internal names
	// always work.
	HostNames []string `toml:"host_names"`
}

// Store holds the SQLite persistence path (the framesvc/forward store).
type Store struct {
	Path string `toml:"path"`
	// WriteDelayS is how long new records may wait in memory before they
	// are written, in seconds (default DefaultWriteDelayS; 0 = at once,
	// every 0.25 s). A power cut loses what waits; commands and setting
	// changes are always written at once.
	WriteDelayS *int `toml:"write_delay_s"`
}

// DefaultWriteDelayS is the default write delay (owner, 2026-11-18).
const DefaultWriteDelayS = 60

// WriteDelay is the write delay in seconds (the default when unset).
func (s Store) WriteDelay() int {
	if s.WriteDelayS == nil {
		return DefaultWriteDelayS
	}
	return *s.WriteDelayS
}

// Threshold is the battery-alert threshold in percent (0 = off; the
// default when unset).
func (a Alerts) Threshold() int {
	if a.SocThresholdPct == nil {
		return DefaultSocThresholdPct
	}
	return *a.SocThresholdPct
}

// HorizonMin is the forecast's horizon in minutes (the default when
// unset).
func (a Alerts) HorizonMin() int { return orDefault(a.SocHorizonMin, DefaultSocHorizonMin) }

// PValue is the forecast's p-value (the default when unset).
func (a Alerts) PValue() float64 { return orDefault(a.SocPValue, DefaultSocPValue) }

// WindowMin is the forecast's sampling window in minutes (the default
// when unset).
func (a Alerts) WindowMin() int { return orDefault(a.SocWindowMin, DefaultSocWindowMin) }

func orDefault[T int | float64](v, d T) T {
	if v == 0 {
		return d
	}
	return v
}

// Alerts tunes the battery alerts (owner, 2026-11-18): a notice when the
// state of charge is at or below the threshold, and one when a line
// fitted to the last window of readings says, with confidence 1 − p,
// that it will be there within the horizon.
type Alerts struct {
	SocThresholdPct *int    `toml:"soc_threshold_pct"` // 0 = off
	SocHorizonMin   int     `toml:"soc_horizon_min"`
	SocPValue       float64 `toml:"soc_p_value"`
	SocWindowMin    int     `toml:"soc_window_min"`
}

// The battery-alert defaults.
const (
	DefaultSocThresholdPct = 20
	DefaultSocHorizonMin   = 120
	DefaultSocPValue       = 0.05
	DefaultSocWindowMin    = 30
)

// Forward holds the cloud-forward ladder (C-001): the mode. The
// maintenance window is process-local state (no config key).
type Forward struct {
	Mode policy.ForwardMode `toml:"mode"`
	// UpstreamCAs overrides the roots trusted for the upstream (real
	// cloud) leg of the MITM. Empty (the default) = the vendor
	// self-signed cloud root embedded in the binary (deploy/cloud-root.pem),
	// materialised under <state_dir>/certs at startup. When set,
	// upstream verification uses exactly these PEM files.
	UpstreamCAs []string `toml:"upstream_cas"`
}

// Sim holds the cloud-stand-in config (task 8). Enabled gates the
// whole surface; when enabled the plant identity must be complete.
type Sim struct {
	Enabled      bool   `toml:"enabled"`
	APIKey       string `toml:"api_key"`
	StationID    string `toml:"station_id"`
	CapacityKW   int    `toml:"capacity_kw"`
	WebToken     string `toml:"web_token"`
	MinSoc       int    `toml:"min_soc"`
	MinSocOnGrid int    `toml:"min_soc_on_grid"`
}

// Tokens holds the REST token store path. Load defaults it to
// <state_dir>/tokens.json; a programmatic "" means in-memory.
type Tokens struct {
	Path string `toml:"path"`
}

// Scheduler holds the force-charge windows (task 9).
type Scheduler struct {
	Windows []WindowConfig `toml:"windows"`
}

// WindowConfig is one [[scheduler.windows]] entry.
type WindowConfig struct {
	Name  string `toml:"name"`
	Slot  int    `toml:"slot"`
	Start int    `toml:"start"`
	End   int    `toml:"end"`
	FdSoc int    `toml:"fd_soc"`
	FdPwr int    `toml:"fd_pwr"`
}

// ForwardWindows maps the config windows onto forward's (the one
// conversion between the TOML and the engine shapes).
func (s Scheduler) ForwardWindows() []forward.Window {
	out := make([]forward.Window, len(s.Windows))
	for i, w := range s.Windows {
		out[i] = forward.Window{Name: w.Name, Slot: w.Slot, Start: w.Start, End: w.End, FdSoc: w.FdSoc, FdPwr: w.FdPwr}
	}
	return out
}

// Load reads and validates the TOML config at path.
func Load(path string) (*Config, error) {
	var c Config
	c.Access.PublicReadOnly = true // default on; an absent key keeps it (spec §5.5)
	md, err := toml.DecodeFile(path, &c)
	if err != nil {
		return nil, fmt.Errorf("load config %s: %w", path, err)
	}
	if err := c.checkKeys(md.Undecoded()); err != nil {
		return nil, fmt.Errorf("load config %s: %w", path, err)
	}
	for _, k := range UISettable {
		if md.IsDefined(strings.Split(k, ".")...) {
			if c.inFile == nil {
				c.inFile = map[string]bool{}
			}
			c.inFile[k] = true
		}
	}
	if err := c.applyDefaults(); err != nil {
		return nil, err
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// InFile reports whether the config file sets a UISettable key (else
// its value is the built-in default).
func (c *Config) InFile(key string) bool { return c.inFile[key] }

// checkKeys turns undecoded keys into warnings (retired) or an error
// (unknown — a typo must not silently fall back to a default).
func (c *Config) checkKeys(undecoded []toml.Key) error {
	var unknown []string
	seen := map[string]bool{}
	for _, k := range undecoded {
		key := k.String()
		retired := ""
		for r := range retiredKeys {
			if key == r || strings.HasPrefix(key, r+".") {
				retired = r
			}
		}
		switch {
		case retired == "":
			unknown = append(unknown, key)
		case !seen[retired]:
			seen[retired] = true
			c.Warnings = append(c.Warnings, fmt.Sprintf("config key %s is retired and ignored (%s) — remove it", retired, retiredKeys[retired]))
		}
	}
	if len(unknown) > 0 {
		return fmt.Errorf("unknown config keys: %s", strings.Join(unknown, ", "))
	}
	return nil
}

// applyDefaults fills every unset setting; paths derive from StateDir.
func (c *Config) applyDefaults() error {
	// [store] write_delay_s and [alerts] default in their accessors
	// (Store.WriteDelay, Alerts.Threshold, …)
	if c.StateDir == "" {
		c.StateDir = DefaultStateDir
	}
	if !filepath.IsAbs(c.StateDir) {
		return fmt.Errorf("state_dir: %q must be an absolute path", c.StateDir)
	}
	if c.Server.InverterListen == "" {
		c.Server.InverterListen = DefaultInverterListen
	}
	if c.Retention.MinFreeMB == 0 {
		c.Retention.MinFreeMB = DefaultMinFreeMB
	}
	if c.Retention.CaptureMaxMB <= 0 {
		c.Retention.CaptureMaxMB = DefaultCaptureMaxMB
	}
	if c.Server.HTTPListen == "" {
		c.Server.HTTPListen = DefaultHTTPListen
	}
	if c.Store.Path == "" {
		c.Store.Path = filepath.Join(c.StateDir, "foxess.db")
	}
	if c.Tokens.Path == "" {
		c.Tokens.Path = filepath.Join(c.StateDir, "tokens.json")
	}
	if c.Battery.CapacityWh < 0 {
		return fmt.Errorf("battery.capacity_wh: %d must be > 0", c.Battery.CapacityWh)
	}
	if old := c.Broker.CertManifest; old != "" {
		if c.Certs.Manifest != "" {
			return fmt.Errorf("certs.manifest and the old broker.cert_manifest are both set: keep certs.manifest only")
		}
		c.Certs.Manifest = old
		c.Warnings = append(c.Warnings, "config key broker.cert_manifest is renamed: use certs.manifest ([certs] manifest = ...)")
	}
	if c.Certs.Manifest == "" {
		c.Certs.Manifest = filepath.Join(c.StateDir, "certs", "cert-manifest.json")
	}
	return nil
}

func (c *Config) validate() error {
	if err := c.validateNet(); err != nil {
		return err
	}
	if c.Retention.RawHours <= 0 {
		c.Retention.RawHours = 72
	}
	if c.Retention.BucketDays <= 0 {
		c.Retention.BucketDays = 365
	}
	if c.Retention.MinFreeMB < 0 {
		return fmt.Errorf("retention.min_free_mb must be >= 0 (0 = the default %d)", DefaultMinFreeMB)
	}
	if c.Retention.ChunkBytesHours < 0 {
		return fmt.Errorf("retention.chunk_bytes_hours must be >= 0 (0 = off)")
	}
	if err := c.validateForward(); err != nil {
		return err
	}
	return c.validateSim()
}

func (c *Config) validateNet() error {
	if a := c.Debug.PprofListen; a != "" && !loopbackAddr(a) {
		return fmt.Errorf("debug.pprof_listen: %q must be a loopback host:port (127.0.0.1, [::1] or localhost)", a)
	}
	for _, n := range c.Net.HostNames {
		if n == "" || strings.ContainsAny(n, ":/ ") {
			return fmt.Errorf("net.host_names: %q is not a plain host name", n)
		}
	}
	for _, cidr := range c.Net.ExtraAllowed {
		if _, err := netip.ParsePrefix(cidr); err != nil {
			return fmt.Errorf("net.extra_allowed: %q is not a CIDR", cidr)
		}
	}
	return nil
}

// loopbackAddr reports whether a host:port names a loopback host.
func loopbackAddr(a string) bool {
	host, _, err := net.SplitHostPort(a)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}

func (c *Config) validateForward() error {
	if c.Forward.Mode == "" {
		// nothing reaches the vendor cloud without an explicit choice
		// (install review I1, 2026-11-17): the installer asks, and an
		// unset mode is off
		c.Forward.Mode = policy.ModeOff
	}
	switch c.Forward.Mode {
	case policy.ModeOff, policy.ModeTelemetryOnly, policy.ModeFull:
	default:
		return fmt.Errorf("forward.mode: %q (want off, telemetry_only or full)", c.Forward.Mode)
	}
	if len(c.Scheduler.Windows) > 0 {
		// Delegate to the engine's construction rules (slot range, HHMM
		// shape, end after start, same-slot overlap) — one validator.
		if _, err := forward.NewScheduler(nil, nil, nil, c.Scheduler.ForwardWindows()); err != nil {
			return fmt.Errorf("scheduler.windows: %w", err)
		}
	}
	return nil
}

func (c *Config) validateSim() error {
	if !c.Sim.Enabled {
		return nil
	}
	if c.Sim.APIKey == "" {
		return fmt.Errorf("sim.api_key is required when sim.enabled")
	}
	if c.Sim.StationID == "" {
		return fmt.Errorf("sim.station_id is required when sim.enabled")
	}
	if c.Sim.CapacityKW < 1 {
		return fmt.Errorf("sim.capacity_kw must be >= 1 (the sim derives fdPwr from it), got %v", c.Sim.CapacityKW)
	}
	if c.Sim.MinSoc == 0 {
		c.Sim.MinSoc = 10
	}
	if c.Sim.MinSocOnGrid == 0 {
		c.Sim.MinSocOnGrid = 11
	}
	if c.Sim.MinSoc < 0 || c.Sim.MinSoc > 100 || c.Sim.MinSocOnGrid < 0 || c.Sim.MinSocOnGrid > 100 {
		return fmt.Errorf("sim.min_soc / sim.min_soc_on_grid must be in 0..100")
	}
	return nil
}

// DefaultPath resolves the config file: $FOXESS_CONFIG, else /etc/foxess-local/config.toml.
func DefaultPath() string {
	if p := os.Getenv("FOXESS_CONFIG"); p != "" {
		return p
	}
	return "/etc/foxess-local/config.toml"
}
