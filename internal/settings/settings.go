// Package settings holds the operator settings the web UI can change
// (F12, the setup wizard): each value comes from the built-in default,
// then the config file, then the UI — a value set in the UI is stored in
// the database and wins, and survives restarts. Every change is audited.
//
// Forward mode is also persisted here, but it stays live in
// forward.State (which applies it to new connections); the host wires
// forward.State's changes to Set(KeyForwardMode, …).
package settings

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"sync"
	"time"

	"foxesslocal/internal/config"
	"foxesslocal/internal/policy"
)

// The setting keys (the config file's names for the same values).
const (
	KeyCapacityWh     = "battery.capacity_wh"
	KeyPublicReadOnly = "access.public_read_only"
	KeyPublicAllPages = "access.public_all_pages"
	KeyForwardMode    = "forward.mode"
	KeyNotifyWebhook  = "notify.webhook_url"
	KeyNotifyNtfy     = "notify.ntfy_url"
	KeyWriteDelay     = "store.write_delay_s"
	KeySocThreshold   = "alerts.soc_threshold_pct"
	KeySocHorizon     = "alerts.soc_horizon_min"
	KeySocPValue      = "alerts.soc_p_value"
	KeySocWindow      = "alerts.soc_window_min"

	keySetupCompleted = "setup.completed_at"
	dbPrefix          = "ui."     // database keys: ui.<key>
	auditKind         = "setting" // one audit row per change
)

// Source is where a value came from.
type Source string

// The sources, lowest precedence first.
const (
	SourceDefault Source = "default"
	SourceFile    Source = "config"
	SourceUI      Source = "ui"
)

// MaxCapacityWh bounds a plausible residential pack (a typo guard).
const MaxCapacityWh = 200_000

// maxURLLen bounds a notification URL.
const maxURLLen = 2048

// numRange bounds a numeric setting.
type numRange struct {
	min, max float64
	float    bool // a decimal (else a whole number)
	what     string
}

// numeric are the numeric tuning settings and their ranges.
var numeric = map[string]numRange{
	KeyWriteDelay:   {0, 900, false, "seconds from 0 (at once) to 900 (15 minutes)"},
	KeySocThreshold: {0, 95, false, "a percentage from 0 (off) to 95"},
	KeySocHorizon:   {10, 1440, false, "minutes from 10 to 1440 (a day)"},
	KeySocPValue:    {0.001, 0.5, true, "a p-value from 0.001 to 0.5 (0.05 = 95 % confident)"},
	KeySocWindow:    {5, 240, false, "minutes from 5 to 240"},
}

// Expert reports whether a setting is an expert-level tuning knob (an
// expert may change it; the other settings need an installer).
func Expert(key string) bool {
	_, ok := numeric[key]
	return ok
}

// Secret reports whether a key's value is a secret (a notification
// URL carries its topic or webhook id): it is never written to the
// audit log, and the API shows it to installers only.
func Secret(key string) bool { return key == KeyNotifyWebhook || key == KeyNotifyNtfy }

// Values are the effective settings.
type Values struct {
	CapacityWh     int // 0 = unknown
	PublicReadOnly bool
	PublicAllPages bool
	ForwardMode    policy.ForwardMode
	NotifyWebhook  string // "" = off
	NotifyNtfy     string // "" = off

	WriteDelay      time.Duration // records may wait this long in memory
	SocThresholdPct int           // 0 = battery alerts off
	SocHorizon      time.Duration
	SocPValue       float64
	SocWindow       time.Duration
}

// Store is the persistence the settings need (internal/store.Store).
type Store interface {
	Setting(key string) (string, bool, error)
	SetSetting(key, value string) error
	DeleteSetting(key string) error
	RecordAudit(ts time.Time, kind, detail string)
}

// Settings is the live view. Safe for concurrent use.
type Settings struct {
	st    Store
	clock func() time.Time
	file  map[string]string // the file's (or default) value per key
	src   map[string]Source // the file-or-default source per key

	mu       sync.RWMutex
	ui       map[string]string // UI overrides
	val      Values
	onChange func(Values) // OnChange's hook (nil = none)
}

// OnChange registers f, called with the new values after each change or
// reset (not under the lock; at most one hook).
func (s *Settings) OnChange(f func(Values)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onChange = f
}

// changed calls the hook with the current values.
func (s *Settings) changed() {
	s.mu.RLock()
	f, v := s.onChange, s.val
	s.mu.RUnlock()
	if f != nil {
		f(v)
	}
}

// New loads the UI overrides over the config's values.
func New(st Store, cfg *config.Config, clock func() time.Time) (*Settings, error) {
	s := &Settings{
		st: st, clock: clock,
		file: map[string]string{
			KeyCapacityWh:     strconv.Itoa(cfg.Battery.CapacityWh),
			KeyPublicReadOnly: strconv.FormatBool(cfg.Access.PublicReadOnly),
			KeyPublicAllPages: strconv.FormatBool(cfg.Access.PublicAllPages),
			KeyForwardMode:    string(cfg.Forward.Mode),
			KeyNotifyWebhook:  cfg.Notify.WebhookURL,
			KeyNotifyNtfy:     cfg.Notify.NtfyURL,
			KeyWriteDelay:     strconv.Itoa(cfg.Store.WriteDelay()),
			KeySocThreshold:   strconv.Itoa(cfg.Alerts.Threshold()),
			KeySocHorizon:     strconv.Itoa(cfg.Alerts.HorizonMin()),
			KeySocPValue:      strconv.FormatFloat(cfg.Alerts.PValue(), 'g', -1, 64),
			KeySocWindow:      strconv.Itoa(cfg.Alerts.WindowMin()),
		},
		src: map[string]Source{},
		ui:  map[string]string{},
	}
	for k := range s.file {
		if Expert(k) { // the file's tuning values: range-checked like the UI's
			if err := validate(k, s.file[k]); err != nil {
				return nil, fmt.Errorf("config: %w", err)
			}
		}
		s.src[k] = SourceDefault
		if cfg.InFile(k) {
			s.src[k] = SourceFile
		}
		v, ok, err := st.Setting(dbPrefix + k)
		if err != nil {
			return nil, fmt.Errorf("settings: %s: %w", k, err)
		}
		if ok {
			if err := validate(k, v); err != nil {
				return nil, fmt.Errorf("settings: stored %s: %w", k, err)
			}
			s.ui[k] = v
		}
	}
	s.recompute()
	return s, nil
}

// Get returns the effective values.
func (s *Settings) Get() Values {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.val
}

// Sources returns each key's source.
func (s *Settings) Sources() map[string]Source {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]Source, len(s.src))
	for k, src := range s.src {
		out[k] = src
		if _, ok := s.ui[k]; ok {
			out[k] = SourceUI
		}
	}
	return out
}

// Set stores a UI value (validated; audited with the acting token).
func (s *Settings) Set(key, value, acting string) error {
	return s.set(key, value, acting, true)
}

// Persist stores a UI value whose change the caller has already audited
// (forward.State records its own forward-mode row).
func (s *Settings) Persist(key, value string) error {
	return s.set(key, value, "", false)
}

func (s *Settings) set(key, value, acting string, audit bool) error {
	changed, err := s.store(key, value, acting, audit)
	if changed {
		s.changed()
	}
	return err
}

// store saves one UI value and reports whether it changed anything.
func (s *Settings) store(key, value, acting string, audit bool) (bool, error) {
	if err := validate(key, value); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.ui[key]; ok && old == value {
		return false, nil
	}
	if err := s.st.SetSetting(dbPrefix+key, value); err != nil {
		return false, fmt.Errorf("settings: save %s: %w", key, err)
	}
	s.ui[key] = value
	s.recomputeLocked()
	if audit {
		shown := value
		if Secret(key) {
			shown = "(set)"
			if value == "" {
				shown = "(off)"
			}
		}
		s.st.RecordAudit(s.clock(), auditKind, fmt.Sprintf("%s = %s by %s", key, shown, acting))
	}
	return true, nil
}

// Reset drops a UI value: the file's (or the default) applies again.
func (s *Settings) Reset(key, acting string) error {
	changed, err := s.reset(key, acting)
	if changed {
		s.changed()
	}
	return err
}

func (s *Settings) reset(key, acting string) (bool, error) {
	if _, ok := s.file[key]; !ok {
		return false, fmt.Errorf("settings: unknown setting %q", key)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.ui[key]; !ok {
		return false, nil
	}
	if err := s.st.DeleteSetting(dbPrefix + key); err != nil {
		return false, fmt.Errorf("settings: reset %s: %w", key, err)
	}
	delete(s.ui, key)
	s.recomputeLocked()
	s.st.RecordAudit(s.clock(), auditKind, fmt.Sprintf("%s reset to the %s value by %s", key, s.src[key], acting))
	return true, nil
}

// SetupCompleted reports whether the setup wizard was finished.
func (s *Settings) SetupCompleted() bool {
	_, ok, err := s.st.Setting(keySetupCompleted)
	return err == nil && ok
}

// CompleteSetup records the wizard's completion.
func (s *Settings) CompleteSetup(acting string) error {
	now := s.clock()
	if err := s.st.SetSetting(keySetupCompleted, now.UTC().Format(time.RFC3339)); err != nil {
		return fmt.Errorf("settings: setup: %w", err)
	}
	s.st.RecordAudit(now, auditKind, "setup completed by "+acting)
	return nil
}

func (s *Settings) recompute() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recomputeLocked()
}

// recomputeLocked derives Values from the overrides and the file (all
// stored values were validated, so the parses cannot fail).
func (s *Settings) recomputeLocked() {
	get := func(k string) string {
		if v, ok := s.ui[k]; ok {
			return v
		}
		return s.file[k]
	}
	capWh, _ := strconv.Atoi(get(KeyCapacityWh))
	pro, _ := strconv.ParseBool(get(KeyPublicReadOnly))
	pap, _ := strconv.ParseBool(get(KeyPublicAllPages))
	s.val = Values{
		CapacityWh: capWh, PublicReadOnly: pro, PublicAllPages: pap, ForwardMode: policy.ForwardMode(get(KeyForwardMode)),
		NotifyWebhook: get(KeyNotifyWebhook), NotifyNtfy: get(KeyNotifyNtfy),
		WriteDelay:      time.Duration(num(get(KeyWriteDelay)) * float64(time.Second)),
		SocThresholdPct: int(num(get(KeySocThreshold))),
		SocHorizon:      time.Duration(num(get(KeySocHorizon)) * float64(time.Minute)),
		SocPValue:       num(get(KeySocPValue)),
		SocWindow:       time.Duration(num(get(KeySocWindow)) * float64(time.Minute)),
	}
}

// validate checks one UI value.
func validate(key, value string) error {
	switch key {
	case KeyCapacityWh:
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || n > MaxCapacityWh {
			return fmt.Errorf("settings: %s must be a whole number of Wh from 1 to %d, got %q", key, MaxCapacityWh, value)
		}
	case KeyPublicReadOnly, KeyPublicAllPages:
		if _, err := strconv.ParseBool(value); err != nil {
			return fmt.Errorf("settings: %s must be true or false, got %q", key, value)
		}
	case KeyNotifyWebhook, KeyNotifyNtfy:
		if err := ValidateURL(value); err != nil {
			return fmt.Errorf("settings: %s %w", key, err)
		}
	case KeyForwardMode:
		switch policy.ForwardMode(value) {
		case policy.ModeOff, policy.ModeTelemetryOnly, policy.ModeFull:
		default:
			return fmt.Errorf("settings: %s must be off, telemetry_only or full, got %q", key, value)
		}
	default:
		r, ok := numeric[key]
		if !ok {
			return fmt.Errorf("settings: unknown setting %q", key)
		}
		return r.check(key, value)
	}
	return nil
}

// Check validates one value for key (the API's pre-check, before any
// change is stored).
func Check(key, value string) error { return validate(key, value) }

// check validates a numeric value against its range.
func (r numRange) check(key, value string) error {
	f, err := strconv.ParseFloat(value, 64)
	if err == nil && !r.float && f != float64(int64(f)) {
		err = errors.New("not whole")
	}
	if err != nil || f < r.min || f > r.max {
		return fmt.Errorf("settings: %s must be %s, got %q", key, r.what, value)
	}
	return nil
}

// num parses a validated numeric value.
func num(v string) float64 {
	f, _ := strconv.ParseFloat(v, 64)
	return f
}

// ValidateURL checks a notification target: "" (off) or an absolute
// http(s) URL with a host.
func ValidateURL(value string) error {
	if value == "" {
		return nil
	}
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || len(value) > maxURLLen {
		return errors.New("must be an http:// or https:// URL")
	}
	return nil
}
