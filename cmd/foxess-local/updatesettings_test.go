package main

import (
	"path/filepath"
	"testing"
	"time"

	"foxesslocal/internal/config"
	"foxesslocal/internal/settings"
	"foxesslocal/internal/store"
)

// TestUpdateFollowsSettings (2026-11-19): the update service reads the
// installer's channel, interval and auto-apply live, and keeps the last
// auto-applied version in the store.
func TestUpdateFollowsSettings(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"), "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := &config.Config{}
	set, _, err := buildSettings(st, cfg, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	h := &handle{st: st, set: set, upd: buildUpdate(cfg)}
	h.useUpdateSettings()
	if v := h.upd.View(); v.Prerelease || v.AutoApply {
		t.Fatalf("defaults: %+v", v)
	}
	for k, v := range map[string]string{settings.KeyUpdatePrerelease: "true", settings.KeyUpdateAutoApply: "true", settings.KeyUpdateInterval: "6"} {
		if err := set.Set(k, v, "installer"); err != nil {
			t.Fatal(err)
		}
	}
	if !h.upd.U.Prerelease() || !h.upd.AutoApply() || h.upd.Interval() != 6*time.Hour {
		t.Fatal("the settings did not reach the update service")
	}
	h.upd.SetAttempted("v0.2.0-rc.1")
	if got := h.upd.Attempted(); got != "v0.2.0-rc.1" {
		t.Fatalf("attempted %q", got)
	}
}
