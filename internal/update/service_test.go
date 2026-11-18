package update

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// TestServiceReasons: the view says why an update cannot be applied —
// a container, a source build, a build without the release key.
func TestServiceReasons(t *testing.T) {
	cases := []struct {
		name string
		svc  *Service
		want string
	}{
		{"container", &Service{U: &Updater{Current: vOld, Key: &PublicKey{}, Repo: testRepo}, Container: true}, "container"},
		{"source build", &Service{U: &Updater{Current: "92ca87b", Key: &PublicKey{}, Repo: testRepo}}, "source build"},
		{"no key", &Service{U: &Updater{Current: vOld, Repo: testRepo}}, "signing key"},
		{"no repo", &Service{U: &Updater{Current: vOld, Key: &PublicKey{}}}, "repository"},
	}
	for _, c := range cases {
		v := c.svc.View()
		if v.CanApply || !strings.Contains(v.Reason, c.want) {
			t.Errorf("%s: can_apply %v reason %q", c.name, v.CanApply, v.Reason)
		}
	}
}

// TestServiceApplyRestarts: a successful apply asks for the restart
// into the new binary; the check result is visible in the view.
func TestServiceApplyRestarts(t *testing.T) {
	u, _ := setup(t, nil)
	restarted := make(chan struct{}, 1)
	svc := &Service{U: u, Interval: func() time.Duration { return time.Hour }, Restart: func() { restarted <- struct{}{} }}
	if v := svc.CheckNow(context.Background()); !v.Available || !v.CanApply || v.Latest != vNew {
		t.Fatalf("view after check: %+v", v)
	}
	if err := svc.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-restarted:
	case <-time.After(5 * time.Second):
		t.Fatal("no restart after apply")
	}
}

// TestServiceConfirmsHealthy: a pending upgrade to this version is
// confirmed after healthyAfter of running; one to another version (the
// old binary still running) is left for the guard.
func TestServiceConfirmsHealthy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		exe := install(t)
		dir := filepath.Dir(exe)
		if err := WriteMarker(dir, Marker{From: vOld, To: vNew, Attempts: 1}); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		svc := &Service{U: &Updater{Current: vNew, Exe: exe}}
		go svc.Run(ctx)
		time.Sleep(healthyAfter - time.Second)
		synctest.Wait()
		if _, ok, _ := ReadMarker(dir); !ok {
			t.Fatal("confirmed too early")
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if _, ok, _ := ReadMarker(dir); ok {
			t.Fatal("not confirmed after healthyAfter")
		}
		cancel()
	})
}

// TestServiceConfirmsOnHealth (install review G2): an upgrade made while
// the inverter was online is confirmed only once the new binary has the
// session back with fresh data (Healthy) — not on uptime alone; if that
// never happens it fails the process (Fail) at healthTimeout, so the
// guard counts a failed start and rolls back.
func TestServiceConfirmsOnHealth(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		exe := install(t)
		dir := filepath.Dir(exe)
		if err := WriteMarker(dir, Marker{From: vOld, To: vNew, Attempts: 1, Online: true}); err != nil {
			t.Fatal(err)
		}
		var healthy atomic.Bool
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		svc := &Service{U: &Updater{Current: vNew, Exe: exe}, Healthy: healthy.Load}
		go svc.Run(ctx)
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		if _, ok, _ := ReadMarker(dir); !ok {
			t.Fatal("confirmed on uptime while the inverter is not back")
		}
		healthy.Store(true)
		time.Sleep(time.Minute)
		synctest.Wait()
		if _, ok, _ := ReadMarker(dir); ok {
			t.Fatal("not confirmed once healthy")
		}
	})
}

// TestServiceFailsUnhealthyUpgrade: never healthy → Fail at healthTimeout,
// the marker left for the guard.
func TestServiceFailsUnhealthyUpgrade(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		exe := install(t)
		dir := filepath.Dir(exe)
		if err := WriteMarker(dir, Marker{From: vOld, To: vNew, Attempts: 1, Online: true}); err != nil {
			t.Fatal(err)
		}
		failed := make(chan string, 1)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		svc := &Service{
			U: &Updater{Current: vNew, Exe: exe}, Healthy: func() bool { return false },
			Fail: func(r string) { failed <- r },
		}
		go svc.Run(ctx)
		time.Sleep(healthTimeout - time.Minute)
		synctest.Wait()
		select {
		case r := <-failed:
			t.Fatalf("failed early: %s", r)
		default:
		}
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		select {
		case <-failed:
		default:
			t.Fatal("no Fail at healthTimeout")
		}
		if _, ok, _ := ReadMarker(dir); !ok {
			t.Fatal("the marker must stay for the guard")
		}
	})
}

// TestServiceConfirmsOfflineUpgrade: made while the inverter was offline,
// there is nothing to prove — confirmed after offlineConfirm.
func TestServiceConfirmsOfflineUpgrade(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		exe := install(t)
		dir := filepath.Dir(exe)
		if err := WriteMarker(dir, Marker{From: vOld, To: vNew, Attempts: 1}); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		svc := &Service{U: &Updater{Current: vNew, Exe: exe}, Healthy: func() bool { return false }}
		go svc.Run(ctx)
		time.Sleep(offlineConfirm - time.Minute)
		synctest.Wait()
		if _, ok, _ := ReadMarker(dir); !ok {
			t.Fatal("confirmed before offlineConfirm")
		}
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		if _, ok, _ := ReadMarker(dir); ok {
			t.Fatal("not confirmed after offlineConfirm")
		}
	})
}

// TestMarkerOnlineRoundTrip: the online flag survives the marker file.
func TestMarkerOnlineRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := WriteMarker(dir, Marker{From: "a", To: "b", Online: true}); err != nil {
		t.Fatal(err)
	}
	if m, ok, _ := ReadMarker(dir); !ok || !m.Online {
		t.Fatalf("marker %+v", m)
	}
}

// TestServiceRetriesConfirm (audit 2026-11-18): when the pending marker
// cannot be removed (the binary's directory became read-only, say), the
// confirmation is retried rather than dropped. Dropped, the marker
// stays, every later restart counts as a failed start, and after three
// the guard rolls back a healthy upgrade.
func TestServiceRetriesConfirm(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory's permissions")
	}
	synctest.Test(t, func(t *testing.T) {
		exe := install(t)
		dir := filepath.Dir(exe)
		if err := WriteMarker(dir, Marker{From: vOld, To: vNew, Attempts: 1}); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o500); err != nil { //nolint:gosec // G302: a read-only test directory
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // G302: restore for the temp-dir cleanup
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		svc := &Service{U: &Updater{Current: vNew, Exe: exe}, Healthy: func() bool { return true }}
		go svc.Run(ctx)
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		if _, ok, _ := ReadMarker(dir); !ok {
			t.Fatal("the marker went although the directory is read-only")
		}
		if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: writable again
			t.Fatal(err)
		}
		time.Sleep(healthEvery + time.Second)
		synctest.Wait()
		if _, ok, _ := ReadMarker(dir); ok {
			t.Fatal("not confirmed once the marker can be removed: the retry was dropped")
		}
	})
}

// TestServiceAutoApply (2026-11-19): with auto-apply on, a check that
// finds a new release applies it and records the attempt first; the
// same version is not tried again; off, nothing is applied.
func TestServiceAutoApply(t *testing.T) {
	u, _ := setup(t, nil)
	restarted := make(chan struct{}, 2)
	attempted := ""
	auto := false
	svc := &Service{
		U: u, Restart: func() { restarted <- struct{}{} },
		AutoApply: func() bool { return auto },
		Attempted: func() string { return attempted }, SetAttempted: func(tag string) { attempted = tag },
	}
	ctx := context.Background()
	svc.autoApply(ctx, svc.CheckNow(ctx))
	if attempted != "" {
		t.Fatal("applied with auto-apply off")
	}
	auto = true
	svc.autoApply(ctx, svc.CheckNow(ctx))
	if attempted != vNew {
		t.Fatalf("attempt not recorded: %q", attempted)
	}
	select {
	case <-restarted:
	case <-time.After(5 * time.Second): // the restart goroutine, not timing logic
		t.Fatal("no restart after the auto-apply")
	}
	u.Current = vOld // as if the new binary had rolled back without a marker
	svc.autoApply(ctx, svc.CheckNow(ctx))
	select {
	case <-restarted:
		t.Fatal("the same version was applied twice")
	case <-time.After(time.Second):
	}
}
