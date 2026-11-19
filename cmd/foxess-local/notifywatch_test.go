package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"foxesslocal/internal/api"
	"foxesslocal/internal/frame"
	"foxesslocal/internal/framesvc"
	"foxesslocal/internal/notify"
	"foxesslocal/internal/store"
	"foxesslocal/internal/update"
)

type noticeLog struct {
	mu sync.Mutex
	ns []notify.Notice
}

func (l *noticeLog) add(n notify.Notice) { l.mu.Lock(); l.ns = append(l.ns, n); l.mu.Unlock() }
func (l *noticeLog) events() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, n := range l.ns {
		out = append(out, n.Event)
	}
	return out
}

func conn(state string) api.Event {
	return api.Event{Kind: api.EventConnection, Data: api.ConnectionEvent{State: state}}
}

func sys(typ, detail string) api.Event {
	return api.Event{Kind: api.EventSystem, Data: api.SystemEvent{Type: typ, Detail: detail}}
}

// TestNoticeWatcherOffline (F2): offline is notified only after
// offlineAfter without readings (a reconnect blip stays quiet), and
// "back online" only follows an offline notice.
func TestNoticeWatcherOffline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ch := make(chan api.Event, 8)
		log := &noticeLog{}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { watchNotices(ctx, ch, log.add); close(done) }()

		ch <- conn("offline") // a blip
		time.Sleep(time.Minute)
		ch <- conn("online")
		synctest.Wait()
		if got := log.events(); len(got) != 0 {
			t.Fatalf("a blip notified: %v", got)
		}
		ch <- conn("offline")
		time.Sleep(offlineAfter + time.Second)
		synctest.Wait()
		ch <- conn("online")
		synctest.Wait()
		cancel()
		<-done
		got := log.events()
		if len(got) != 2 || got[0] != noticeOffline || got[1] != noticeOnline {
			t.Fatalf("notices %v", got)
		}
		if log.ns[0].Severity != notify.Critical || log.ns[1].Severity != notify.Info {
			t.Fatalf("severities %+v", log.ns)
		}
	})
}

// TestNoticeWatcherSystem: alerts, gate rule loss, a rolled-back
// upgrade, the maintenance window and forward-mode changes become
// notices; a window re-arm and the live snapshots do not.
func TestNoticeWatcherSystem(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ch := make(chan api.Event, 16)
		log := &noticeLog{}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { watchNotices(ctx, ch, log.add); close(done) }()
		ch <- api.Event{Kind: api.EventSnapshot, Data: api.SnapshotEvent{}}
		ch <- sys(api.SystemAlert, "command c1 failed: timeout")
		ch <- sys(api.SystemGateRuleLoss, "table missing; re-applied")
		ch <- sys(api.SystemUpgradeRolledBack, "v0.2.0 → v0.1.9")
		ch <- sys(api.SystemMaintenanceOpened, "opened 1h")
		ch <- sys(api.SystemMaintenanceExtended, "re-armed")
		ch <- sys(api.SystemMaintenanceClosed, "closed")
		ch <- sys(api.SystemForwardModeChanged, "off")
		ch <- sys(api.SystemFirmwareTask, "manager: blocked")
		synctest.Wait()
		cancel()
		<-done
		want := []string{"alert", "gate-rule-loss", "upgrade-rolled-back", "maintenance-opened", "maintenance-closed", "forward-mode-changed", "firmware-task"}
		got := log.events()
		if len(got) != len(want) {
			t.Fatalf("notices %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("notices %v, want %v", got, want)
			}
		}
		if log.ns[1].Severity != notify.Critical || log.ns[0].Message != "command c1 failed: timeout" {
			t.Fatalf("notices %+v", log.ns)
		}
		for _, n := range log.ns { // all at least high priority (owner, 2026-11-18)
			if n.Severity == notify.Info {
				t.Errorf("%s is info: only back-online may be lower", n.Event)
			}
		}
	})
}

// TestRollbackNotice (F2): a rolled-back upgrade is announced once —
// not again after a restart while the guard's marker stays (until the
// owner dismisses it in the UI).
func TestRollbackNotice(t *testing.T) {
	if _, _, ok := rollbackNotice(nil, ""); ok {
		t.Fatal("no rollback announced")
	}
	m := &update.Marker{From: "v0.1.9", To: "v0.2.0"}
	ev, key, ok := rollbackNotice(m, "")
	if !ok || ev.Type != api.SystemUpgradeRolledBack || !strings.Contains(ev.Detail, "v0.2.0") || !strings.Contains(ev.Detail, "v0.1.9") {
		t.Fatalf("first: %+v %v", ev, ok)
	}
	if _, _, ok := rollbackNotice(m, key); ok {
		t.Fatal("announced again")
	}
	if _, _, ok := rollbackNotice(&update.Marker{From: "v0.2.0", To: "v0.2.1"}, key); !ok {
		t.Fatal("a later rollback not announced")
	}
}

// TestSimAlertIsASystemEvent (audit 2026-11-18): the sim's alerts reach
// the bus as api.SystemEvent alerts. They were published as the sim's
// own struct under the system kind, which the web UI rendered (same
// JSON) but the notifier, matching on the type, silently skipped.
func TestSimAlertIsASystemEvent(t *testing.T) {
	bus := api.NewBus()
	sub := bus.Subscribe()
	defer sub.Close()
	simAlert(bus)("sim: min-SoC accepted locally")
	ev := <-sub.Events()
	se, ok := ev.Data.(api.SystemEvent)
	if ev.Kind != api.EventSystem || !ok || se.Type != api.SystemAlert || se.Detail == "" {
		t.Fatalf("event %+v", ev)
	}
	var c connWatch
	if n, ok := c.noticeFor(ev); !ok || n.Event != api.SystemAlert {
		t.Fatalf("the notifier skips it: %+v %v", n, ok)
	}
}

// TestGridWatch (owner, 2026-11-18): the inverter state (31027:
// 3 on grid, 4 off grid / EPS — foxess_modbus) becomes grid-lost and
// grid-restored events. A grid already down when the app starts is news
// (the power-cut scenario: the gateway host itself lost power with the
// grid); restored follows only a lost; other states change nothing.
func TestGridWatch(t *testing.T) {
	st := func(v int) *int { return &v }
	t0 := time.Unix(1_800_000_000, 0)
	var g gridWatch
	if _, ok := g.observe(st(3), t0); ok {
		t.Fatal("on grid at start is not news")
	}
	if _, ok := g.observe(nil, t0); ok {
		t.Fatal("no state, no news")
	}
	ev, ok := g.observe(st(4), t0.Add(time.Minute))
	if !ok || ev.Type != api.SystemGridLost {
		t.Fatalf("lost: %+v %v", ev, ok)
	}
	if _, ok := g.observe(st(4), t0.Add(2*time.Minute)); ok {
		t.Fatal("still lost is not news")
	}
	if _, ok := g.observe(st(1), t0.Add(3*time.Minute)); ok {
		t.Fatal("waiting is not news")
	}
	ev, ok = g.observe(st(3), t0.Add(31*time.Minute))
	if !ok || ev.Type != api.SystemGridRestored || !strings.Contains(ev.Detail, "30 min") {
		t.Fatalf("restored: %+v %v", ev, ok)
	}
}

// TestGridWatchAtStart: the app starts (after a power cut, say) with
// the grid already down: that is news; and the notices' severities.
func TestGridWatchAtStart(t *testing.T) {
	down := invStateOffGrid
	var fresh gridWatch
	if ev, ok := fresh.observe(&down, time.Unix(1_800_000_000, 0)); !ok || ev.Type != api.SystemGridLost || !strings.Contains(ev.Detail, "start") {
		t.Fatalf("down at start: %+v %v", ev, ok)
	}
	var c connWatch
	if n, ok := c.noticeFor(api.Event{Kind: api.EventSystem, Data: api.SystemEvent{Type: api.SystemGridLost}}); !ok || n.Severity != notify.Critical {
		t.Fatalf("grid-lost notice: %+v", n)
	}
	if n, ok := c.noticeFor(api.Event{Kind: api.EventSystem, Data: api.SystemEvent{Type: api.SystemGridRestored}}); !ok || n.Severity != notify.Info {
		t.Fatalf("grid-restored notice: %+v", n)
	}
}

// TestRestartNotice (owner, 2026-11-18): a start without a clean-stop
// marker after earlier data is an unexpected stop (a power cut, say),
// announced once per outage with the last reading's time; a clean stop,
// a first start and an announced outage are not news; a clock still
// behind the last reading (no RTC, NTP not yet synced) gives no
// duration.
func TestRestartNotice(t *testing.T) {
	last := time.Date(2026, 11, 18, 3, 10, 0, 0, time.UTC)
	now := last.Add(4 * time.Minute)
	if _, _, ok := restartNotice(true, last, "", now); ok {
		t.Fatal("a clean stop is not news")
	}
	if _, _, ok := restartNotice(false, time.Time{}, "", now); ok {
		t.Fatal("a first start is not news")
	}
	ev, key, ok := restartNotice(false, last, "", now)
	if !ok || ev.Type != api.SystemUncleanRestart || !strings.Contains(ev.Detail, "4 min") {
		t.Fatalf("unclean: %+v %v", ev, ok)
	}
	if _, _, ok := restartNotice(false, last, key, now.Add(time.Minute)); ok {
		t.Fatal("the same outage announced twice (a restart loop)")
	}
	ev, _, ok = restartNotice(false, last, "", last.Add(-50*365*24*time.Hour))
	if !ok || strings.Contains(ev.Detail, " min") || !strings.Contains(ev.Detail, "clock") {
		t.Fatalf("clock not set: %+v", ev)
	}
	var c connWatch
	if n, ok := c.noticeFor(api.Event{Kind: api.EventSystem, Data: ev}); !ok || n.Severity != notify.Warning {
		t.Fatalf("notice: %+v %v", n, ok)
	}
}

// TestFirmwareTaskEvent (owner, 2026-11-19): a cloud firmware update
// becomes a system event saying which component and file, and whether
// it was blocked or passed — never the download URL (notices leave the
// LAN, through ntfy or a webhook).
func TestFirmwareTaskEvent(t *testing.T) {
	task := &frame.FwTask{MCU: "A", Filename: "10-300-11057-37_K10_Manager_V1.71.bin", URL: "https://example.invalid/secret", Size: 764904, CRC: 59102}
	blocked := firmwareTaskEvent(framesvc.Event{Kind: framesvc.EventFirmwareTask, FwTask: task, Decision: store.DecisionDropped})
	passed := firmwareTaskEvent(framesvc.Event{Kind: framesvc.EventFirmwareTask, FwTask: task, Decision: store.DecisionForwarded})
	for _, ev := range []api.SystemEvent{blocked, passed} {
		if ev.Type != api.SystemFirmwareTask || !strings.Contains(ev.Detail, "manager") ||
			!strings.Contains(ev.Detail, "10-300-11057-37_K10_Manager_V1.71.bin") || strings.Contains(ev.Detail, "example.invalid") {
			t.Errorf("event %+v", ev)
		}
	}
	if !strings.Contains(blocked.Detail, "Blocked") || !strings.Contains(passed.Detail, "maintenance window") {
		t.Errorf("blocked %q / passed %q", blocked.Detail, passed.Detail)
	}
}
