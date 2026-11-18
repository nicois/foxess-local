package main

import (
	"path/filepath"
	"testing"
	"time"

	"foxesslocal/internal/config"
	"foxesslocal/internal/settings"
	"foxesslocal/internal/store"
)

// TestWriteDelayApplied (owner, 2026-11-18): the store takes the write
// delay from the settings at start, and again whenever it changes.
func TestWriteDelayApplied(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"), "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	d := 300
	cfg := &config.Config{Store: config.Store{WriteDelayS: &d}}
	set, _, err := buildSettings(st, cfg, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.WriteDelay(); got != 5*time.Minute {
		t.Fatalf("at start: %v", got)
	}
	if err := set.Set(settings.KeyWriteDelay, "900", "expert"); err != nil {
		t.Fatal(err)
	}
	if got := st.WriteDelay(); got != 15*time.Minute {
		t.Fatalf("after the change: %v", got)
	}
}
