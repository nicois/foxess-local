package main

import (
	"encoding/json"
	"math"
	"path/filepath"
	"testing"
	"time"

	"foxesslocal/internal/api"
	"foxesslocal/internal/config"
	"foxesslocal/internal/frame"
	"foxesslocal/internal/notify"
	"foxesslocal/internal/socalert"
	"foxesslocal/internal/socest"
	"foxesslocal/internal/store"
)

// TestSocWatchPublishes (owner, 2026-11-18): live readings go through the
// battery alerts with the current settings; an alert becomes a system
// event, and a warning notice.
func TestSocWatchPublishes(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"), "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	set, _, err := buildSettings(st, &config.Config{}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	h := &handle{bus: api.NewBus(), set: set}
	sub := h.bus.Subscribe()
	defer sub.Close()
	now := time.Date(2026, 11, 19, 1, 0, 0, 0, time.UTC)
	h.observeSoc(19, 0, 0, now) // below the default 20 %
	select {
	case ev := <-sub.Events():
		se, ok := ev.Data.(api.SystemEvent)
		if !ok || se.Type != socalert.KindLow {
			t.Fatalf("event %+v", ev)
		}
		var c connWatch
		if n, ok := c.noticeFor(ev); !ok || n.Severity != notify.Warning || n.Title == "" {
			t.Fatalf("notice %+v %v", n, ok)
		}
	default: // the bus delivers on publish (buffered): no wall-clock wait
		t.Fatal("no event")
	}
	var c connWatch
	if n, ok := c.noticeFor(api.Event{Kind: api.EventSystem, Data: api.SystemEvent{Type: socalert.KindForecastLow}}); !ok || n.Severity != notify.Warning {
		t.Fatalf("forecast notice %+v %v", n, ok)
	}
}

// TestSocWatchEstimate (owner, 2026-11-20; D-028): each reading moves
// the SoC estimate with the battery power; the host returns it (for the
// snapshot event) and keeps it for /state. No capacity, no estimate.
func TestSocWatchEstimate(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"), "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	newHandle := func(capWh int) *handle {
		set, _, err := buildSettings(st, &config.Config{Battery: config.Battery{CapacityWh: capWh}}, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		return &handle{bus: api.NewBus(), set: set}
	}
	now := time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC)
	h := newHandle(41930)
	var est *float64
	for i := range 121 { // 10 min at 500 W discharging: −0.199 %
		est = h.observeSoc(42, -500, 0, now.Add(time.Duration(i)*5*time.Second))
	}
	if est == nil || *est != 41.8 {
		t.Fatalf("estimate %v, want 41.80", est)
	}
	if r, ok := h.soc.est.Latest(); !ok || r.Raw != 42 {
		t.Fatalf("Latest %+v %v", r, ok)
	}
	if est := newHandle(0).observeSoc(42, -500, 0, now); est != nil {
		t.Fatalf("no capacity: %v", *est)
	}
}

// TestSocLearnedPersists (owner, 2026-11-20; D-028): the energy per
// percent learnt survives a restart — restored at start, saved (off the
// telemetry hook) whenever it changes.
func TestSocLearnedPersists(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"), "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	set, _, err := buildSettings(st, &config.Config{Battery: config.Battery{CapacityWh: 41930}}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(socLearnedKey, `{"charging":{"wh_per_pct":0,"n":0},"discharging":{"wh_per_pct":377.37,"n":5}}`); err != nil {
		t.Fatal(err)
	}
	h := &handle{bus: api.NewBus(), set: set, st: st}
	h.restoreSocLearned()
	if l := h.soc.est.Learned(); l.Discharging.N != 5 || l.Discharging.WhPerPct != 377.37 {
		t.Fatalf("restored %+v", l)
	}
	// one clean hour: 42 → 41 at 419 W out → 40
	now := time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC)
	for i := range 722 {
		raw := byte(41)
		switch i {
		case 0:
			raw = 42
		case 721:
			raw = 40
		}
		h.observeSoc(raw, -419, 0, now.Add(time.Duration(i)*5*time.Second))
	}
	h.soc.saves.Wait()
	v, ok, err := st.Setting(socLearnedKey)
	if err != nil || !ok {
		t.Fatalf("saved %q %v %v", v, ok, err)
	}
	var got socest.Learned
	if err := json.Unmarshal([]byte(v), &got); err != nil || got != h.soc.est.Learned() || got.Discharging.N != 6 {
		t.Fatalf("saved %+v, learnt %+v (%v)", got, h.soc.est.Learned(), err)
	}
}

// TestSocWatchAnchorAndStore (D-028, 2026-11-20): a snapshot's BMS SoC
// anchors the estimate; each sample's estimate is stored on its row
// (0.01 %) for the history.
func TestSocWatchAnchorAndStore(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"), "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	set, _, err := buildSettings(st, &config.Config{Battery: config.Battery{CapacityWh: 41930}}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	h := &handle{bus: api.NewBus(), set: set, st: st}
	now := time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC)
	sample := func(ts time.Time) *float64 { // the poller stores the row, then observes
		st.RecordTelemetry("c1", ts, frame.Tele{Soc: 68})
		return h.socSample("c1", ts, frame.Tele{Soc: 68})
	}
	sample(now)
	h.anchorSoc(now.Add(time.Second), 669)
	est := sample(now.Add(5 * time.Second))
	if est == nil || *est != 66.9 {
		t.Fatalf("anchored estimate %v, want 66.90", est)
	}
	st.Flush()
	raw, err := st.HistoryRaw(now, now.Add(time.Minute))
	if err != nil || len(raw) != 1 || !raw[0].SocEst.Valid || raw[0].SocEst.Int64 != 6690 {
		t.Fatalf("stored %+v %v", raw, err)
	}
}

// TestSocWatchForceDischarge (owner, 2026-11-20): the forecast takes the
// running force discharge into account — a fast forced fall that ends
// well above the alert level, with only the house's own load after it,
// does not warn; the same fall outside a session does.
func TestSocWatchForceDischarge(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"), "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if got := forcedFall(t, st, false); len(got) != 1 || got[0].Type != socalert.KindForecastLow {
		t.Fatalf("no session: %+v", got)
	}
	if got := forcedFall(t, st, true); len(got) != 0 {
		t.Fatalf("in a force discharge ending at 55 %%: %+v", got)
	}
}

// forcedFall feeds 30 min of a 3 kW discharge from 70 % (30 %/h on
// 10 kWh; the house uses 300 W) — in a force discharge ending after
// 30 min with a 20 % cut-off, or not — and returns the system events.
func forcedFall(t *testing.T, st *store.Store, forced bool) []api.SystemEvent {
	t.Helper()
	set, _, err := buildSettings(st, &config.Config{Battery: config.Battery{CapacityWh: 10000}}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	h := &handle{bus: api.NewBus(), set: set}
	start := time.Date(2026, 11, 20, 7, 0, 0, 0, time.UTC)
	cur := start // the reading being fed
	if forced {
		fd := func() (time.Duration, int, bool) { return start.Add(30 * time.Minute).Sub(cur), 20, true }
		h.soc.fd.Store(&fd)
	}
	sub := h.bus.Subscribe()
	defer sub.Close()
	for i := range 30 * 12 {
		cur = start.Add(time.Duration(i) * 5 * time.Second)
		raw := byte(math.Round(70 - 30*cur.Sub(start).Hours()))
		h.observeSoc(raw, -3000, -300, cur)
	}
	var got []api.SystemEvent
	for {
		select {
		case ev := <-sub.Events():
			if se, ok := ev.Data.(api.SystemEvent); ok {
				got = append(got, se)
			}
		default:
			return got
		}
	}
}
