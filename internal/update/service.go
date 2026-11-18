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
	checkEvery      = 24 * time.Hour         // then daily
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
	U            *Updater
	CheckEnabled bool
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
	Failed    *Marker    `json:"rolled_back,omitempty"`
	Pending   *Marker    `json:"pending,omitempty"`
}

func (s *Service) dir() string { return filepath.Dir(s.U.Exe) }

// reason is why an update could not be applied here ("" = it could).
func (s *Service) reason() string {
	switch {
	case s.Container:
		return "running in a container: update by pulling the new image"
	case !semver.MatchString(s.U.Current):
		return "a source build (" + s.U.Current + "): update by rebuilding"
	case s.U.Key == nil:
		return "this build carries no release signing key"
	case s.U.Repo == "":
		return "no release repository configured"
	}
	return ""
}

// View is the current state.
func (s *Service) View() View {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := View{Current: s.U.Current, Checks: s.CheckEnabled, Error: s.lastErr, Reason: s.reason()}
	if s.last != nil {
		v.Latest, v.URL, v.Available = s.last.Latest, s.last.URL, s.last.Available
		at := s.last.CheckedAt
		v.CheckedAt = &at
	}
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
	if r := s.reason(); r != "" {
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
// checks for releases (first after firstCheckAfter, then daily) when
// enabled. It returns when ctx is done.
func (s *Service) Run(ctx context.Context) {
	start := time.Now()
	confirm := time.After(healthyAfter)
	var check <-chan time.Time
	if s.CheckEnabled && s.U.Repo != "" {
		check = time.After(firstCheckAfter)
	}
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
		case <-check:
			s.CheckNow(ctx)
			check = time.After(checkEvery)
		}
	}
}
