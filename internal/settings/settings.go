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

	mu  sync.RWMutex
	ui  map[string]string // UI overrides
	val Values
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
		},
		src: map[string]Source{},
		ui:  map[string]string{},
	}
	for k := range s.file {
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
	if err := validate(key, value); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.ui[key]; ok && old == value {
		return nil
	}
	if err := s.st.SetSetting(dbPrefix+key, value); err != nil {
		return fmt.Errorf("settings: save %s: %w", key, err)
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
	return nil
}

// Reset drops a UI value: the file's (or the default) applies again.
func (s *Settings) Reset(key, acting string) error {
	if _, ok := s.file[key]; !ok {
		return fmt.Errorf("settings: unknown setting %q", key)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.ui[key]; !ok {
		return nil
	}
	if err := s.st.DeleteSetting(dbPrefix + key); err != nil {
		return fmt.Errorf("settings: reset %s: %w", key, err)
	}
	delete(s.ui, key)
	s.recomputeLocked()
	s.st.RecordAudit(s.clock(), auditKind, fmt.Sprintf("%s reset to the %s value by %s", key, s.src[key], acting))
	return nil
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
		return fmt.Errorf("settings: unknown setting %q", key)
	}
	return nil
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
