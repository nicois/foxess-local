package main

import (
	"path/filepath"
	"testing"
	"time"

	"foxesslocal/internal/api"
	"foxesslocal/internal/config"
	"foxesslocal/internal/notify"
	"foxesslocal/internal/socalert"
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
	h.observeSoc(19, now) // below the default 20 %
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
