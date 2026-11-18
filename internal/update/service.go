package update

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"
)

// Service timing.
const (
	firstCheckAfter = time.Minute            // after start: let the session settle first
	scheduleEvery   = time.Minute            // how often Run looks whether a check is due
	healthyAfter    = 2 * time.Minute        // a pending upgrade is not confirmed before this much uptime
	healthEvery     = 15 * time.Second       // then health is re-checked this often
	healthTimeout   = 15 * time.Minute       // online before, not healthy by now: the upgrade failed
	offlineConfirm  = 10 * time.Minute       // offline before: confirmed after this much uptime
	restartDelay    = 500 * time.Millisecond // lets the apply response reach the client
)

// Service runs the release check, holds its result, confirms a pending
// upgrade once the new binary has run healthily, and applies updates on
// request.
type Service struct {
	U *Updater
	// Interval is how often to check for releases (read live: the
	// installer's setting; 0 or nil = never on its own).
	Interval func() time.Duration
	// AutoApply: apply each new eligible release once, on its own (read
	// live; nil = never). Attempted / SetAttempted persist the last
	// version tried, so a restart or a rollback does not retry it.
	AutoApply    func() bool
	Attempted    func() string
	SetAttempted func(tag string)
	Container    bool   // running in a container: upgrades are image pulls
	Restart      func() // exits the process so systemd starts the new binary
	// Healthy reports the running system is well: the inverter session up
	// with fresh data stored (nil: uptime alone confirms an upgrade).
	Healthy func() bool
	// Fail ends the process unsuccessfully (an upgrade that did not bring
	// the inverter back): the guard counts the start and rolls back.
	Fail func(reason string)

	mu      sync.Mutex
	last    *Status
	lastErr string
	lastAt  time.Time // the last check (scheduled or on request)
}

// interval is the check interval now (0: off).
func (s *Service) interval() time.Duration {
	if s.Interval == nil || s.U.Repo == "" {
		return 0
	}
	return max(0, s.Interval())
}

// View is what the API shows.
type View struct {
	Current   string     `json:"current"`
	Latest    string     `json:"latest,omitempty"`
	URL       string     `json:"url,omitempty"`
	Available bool       `json:"available"`
	CheckedAt *time.Time `json:"checked_at"`
	Error     string     `json:"error,omitempty"`
	Checks    bool       `json:"checks_enabled"`
	CanApply  bool       `json:"can_apply"`
	Reason    string     `json:"reason,omitempty"` // why it cannot be applied
	// ReasonCode is Reason's stable code (Reason* constants).
	ReasonCode string  `json:"reason_code,omitempty"`
	Failed     *Marker `json:"rolled_back,omitempty"`
	Pending    *Marker `json:"pending,omitempty"`
	// LatestPrerelease: Latest is a prerelease (offered by the opt-in).
	LatestPrerelease bool `json:"latest_prerelease"`
	// The installer's choices: the check interval in hours (0 = off),
	// prereleases offered, and updates applied on their own.
	CheckIntervalH int  `json:"check_interval_h"`
	Prerelease     bool `json:"prerelease"`
	AutoApply      bool `json:"auto_apply"`
	// NextCheckAt is when the next scheduled check is due (null: off).
	NextCheckAt *time.Time `json:"next_check_at"`
}

func (s *Service) dir() string { return filepath.Dir(s.U.Exe) }

// Stable codes for why an update cannot be applied here (View.ReasonCode):
// a UI translates these; Reason stays the English text.
const (
	ReasonContainer   = "container"
	ReasonSourceBuild = "source_build"
	ReasonNoKey       = "no_key"
	ReasonNoRepo      = "no_repo"
	ReasonUnavailable = "unavailable" // no updater in this host (devui, tests)
)

// reason is why an update could not be applied here: its code and its
// English text ("", "" = it could).
func (s *Service) reason() (code, text string) {
	switch {
	case s.Container:
		return ReasonContainer, "running in a container: update by pulling the new image"
	case !semver.MatchString(s.U.Current):
		return ReasonSourceBuild, "a source build (" + s.U.Current + "): update by rebuilding"
	case s.U.Key == nil:
		return ReasonNoKey, "this build carries no release signing key"
	case s.U.Repo == "":
		return ReasonNoRepo, "no release repository configured"
	}
	return "", ""
}

// View is the current state.
func (s *Service) View() View {
	s.mu.Lock()
	defer s.mu.Unlock()
	iv := s.interval()
	v := View{
		Current: s.U.Current, Checks: iv > 0, Error: s.lastErr,
		CheckIntervalH: int(iv / time.Hour), Prerelease: s.U.allowPre(),
		AutoApply: s.AutoApply != nil && s.AutoApply(),
	}
	if s.last != nil {
		v.Latest, v.URL, v.Available, v.LatestPrerelease = s.last.Latest, s.last.URL, s.last.Available, s.last.Prerelease
		at := s.last.CheckedAt
		v.CheckedAt = &at
	}
	if iv > 0 && !s.lastAt.IsZero() {
		next := s.lastAt.Add(iv)
		v.NextCheckAt = &next
	}
	v.ReasonCode, v.Reason = s.reason()
	v.CanApply = v.Reason == "" && v.Available
	if s.U.Exe != "" {
		if m, ok, _ := ReadFailed(s.dir()); ok {
			v.Failed = &m
		}
		if m, ok, _ := ReadMarker(s.dir()); ok {
			v.Pending = &m
		}
	}
	return v
}

// CheckNow runs a check and returns the view.
func (s *Service) CheckNow(ctx context.Context) View {
	st, err := s.U.Check(ctx)
	s.mu.Lock()
	s.lastAt = time.Now()
	if err != nil {
		s.lastErr = err.Error()
	} else {
		s.lastErr = ""
		s.last = &st
	}
	s.mu.Unlock()
	return s.View()
}

// Apply installs the latest release and, on success, restarts.
func (s *Service) Apply(ctx context.Context) error {
	if _, r := s.reason(); r != "" {
		return &ReasonError{Reason: r}
	}
	online := s.Healthy != nil && s.Healthy()
	if err := s.U.Apply(ctx); err != nil {
		return err
	}
	if online { // the new binary must bring the inverter back (confirmStep)
		if m, ok, _ := ReadMarker(s.dir()); ok {
			m.Online = true
			if err := WriteMarker(s.dir(), m); err != nil {
				// the guard then confirms on uptime alone (less strict)
				slog.Warn("update: could not record that the inverter was online", "err", err)
			}
		}
	}
	if s.Restart != nil {
		go func() {
			time.Sleep(restartDelay)
			s.Restart()
		}()
	}
	return nil
}

// ClearFailed dismisses the rollback notice.
func (s *Service) ClearFailed() error { return ClearFailed(s.dir()) }

// confirmStep judges a pending upgrade to this version (install review
// G2) and reports whether to look again: confirmed once Healthy (the
// inverter session back with fresh data — not uptime alone); made while
// the inverter was online and still unhealthy at healthTimeout, the
// process fails so the guard rolls back; made while it was offline,
// confirmed after offlineConfirm (nothing to prove).
func (s *Service) confirmStep(up time.Duration) bool {
	m, ok, _ := ReadMarker(s.dir())
	if !ok || m.To != s.U.Current {
		return false
	}
	switch {
	case s.Healthy == nil || s.Healthy():
		return !s.confirm()
	case m.Online && up >= healthTimeout:
		if s.Fail != nil {
			s.Fail(fmt.Sprintf("upgrade to %s: the inverter session did not come back within %s", m.To, healthTimeout))
		}
		return false
	case !m.Online && up >= offlineConfirm:
		return !s.confirm()
	}
	return true
}

// confirm ends the pending upgrade; false (logged) when the marker
// could not be removed — the caller looks again, because a marker left
// behind makes every later restart count as a failed start, and after
// three the guard rolls back a healthy upgrade.
func (s *Service) confirm() bool {
	if err := ConfirmHealthy(s.dir()); err != nil {
		slog.Error("update: could not confirm the upgrade (retrying)", "err", err)
		return false
	}
	return true
}

// ReasonError says why applying is not possible in this installation.
type ReasonError struct{ Reason string }

func (e *ReasonError) Error() string { return "update: " + e.Reason }

// Run confirms a pending upgrade to this version after healthyAfter, and
// checks for releases when due (the first a minute after start, then
// every Interval; a changed interval applies within a minute). With
// AutoApply, a check that finds a new eligible release applies it — once
// per version. It returns when ctx is done.
func (s *Service) Run(ctx context.Context) {
	start := time.Now()
	confirm := time.After(healthyAfter)
	tick := time.NewTicker(scheduleEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-confirm:
			if s.confirmStep(time.Since(start)) {
				confirm = time.After(healthEvery)
			} else {
				confirm = nil
			}
		case <-tick.C:
			s.mu.Lock()
			last := s.lastAt
			s.mu.Unlock()
			if checkDue(time.Now(), start, last, s.interval()) {
				s.autoApply(ctx, s.CheckNow(ctx))
			}
		}
	}
}

// checkDue: a scheduled check is due — never with interval 0; the first
// firstCheckAfter after start; then interval after the last (a last check
// in the future means the clock moved back: check).
func checkDue(now, start, last time.Time, interval time.Duration) bool {
	switch {
	case interval <= 0:
		return false
	case last.IsZero():
		return now.Sub(start) >= firstCheckAfter
	case last.After(now):
		return true
	}
	return now.Sub(last) >= interval
}

// autoApplyTarget is the release auto-apply should try now, if any: an
// available release that can be applied here, not already attempted, and
// not the version that rolled back.
func autoApplyTarget(v View, attempted string) (string, bool) {
	switch {
	case !v.Available || !v.CanApply || v.Latest == "":
		return "", false
	case v.Latest == attempted:
		return "", false
	case v.Failed != nil && v.Failed.To == v.Latest:
		return "", false
	}
	return v.Latest, true
}

// autoApply applies the release a scheduled check found, when the
// installer asked for that. The attempt is recorded before applying, so
// it is made once even when it fails or rolls back.
func (s *Service) autoApply(ctx context.Context, v View) {
	if s.AutoApply == nil || !s.AutoApply() {
		return
	}
	attempted := ""
	if s.Attempted != nil {
		attempted = s.Attempted()
	}
	tag, ok := autoApplyTarget(v, attempted)
	if !ok {
		return
	}
	if s.SetAttempted != nil {
		s.SetAttempted(tag)
	}
	slog.Info("update: applying a new release on its own (auto-apply)", "from", s.U.Current, "to", tag)
	if err := s.Apply(ctx); err != nil {
		slog.Error("update: auto-apply failed", "to", tag, "err", err)
	}
}
