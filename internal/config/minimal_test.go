package config_test

import (
	"path/filepath"
	"testing"

	"foxesslocal/internal/config"
	"foxesslocal/internal/policy"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoadEmptyFileDefaults: an empty config is a complete, valid
// config — every setting has a default matching the installer layout
// and the gate's fixed handoff port. Nothing is required.
func TestLoadEmptyFileDefaults(t *testing.T) {
	cfg, err := config.Load(writeCfg(t, t.TempDir(), ""))
	require.NoError(t, err)
	assert.Equal(t, "/var/lib/foxess-local", cfg.StateDir)
	assert.Equal(t, "0.0.0.0:14431", cfg.Server.InverterListen)
	assert.Equal(t, "0.0.0.0:18080", cfg.Server.HTTPListen)
	assert.Equal(t, "/var/lib/foxess-local/foxess.db", cfg.Store.Path)
	assert.Equal(t, "/var/lib/foxess-local/tokens.json", cfg.Tokens.Path)
	assert.Equal(t, "/var/lib/foxess-local/certs/cert-manifest.json", cfg.Certs.Manifest)
	assert.Equal(t, policy.ModeOff, cfg.Forward.Mode) // nothing reaches the cloud without a choice (install review I1)
	assert.Equal(t, config.Retention{RawHours: 72, BucketDays: 365, MinFreeMB: config.DefaultMinFreeMB, CaptureMaxMB: config.DefaultCaptureMaxMB}, cfg.Retention)
	assert.True(t, cfg.Access.PublicReadOnly)
	assert.False(t, cfg.Access.PublicAllPages)
	assert.False(t, cfg.Net.AllowPublic)
	assert.Empty(t, cfg.Warnings)
}

// TestLoadStateDirDerivesPaths: one state_dir moves every derived path.
func TestLoadStateDirDerivesPaths(t *testing.T) {
	cfg, err := config.Load(writeCfg(t, t.TempDir(), `state_dir = "/srv/fl"`))
	require.NoError(t, err)
	assert.Equal(t, "/srv/fl/foxess.db", cfg.Store.Path)
	assert.Equal(t, "/srv/fl/tokens.json", cfg.Tokens.Path)
	assert.Equal(t, "/srv/fl/certs/cert-manifest.json", cfg.Certs.Manifest)
}

// TestLoadExplicitPathsWin: an explicit per-path key overrides the
// state_dir derivation.
func TestLoadExplicitPathsWin(t *testing.T) {
	cfg, err := config.Load(writeCfg(t, t.TempDir(), `
state_dir = "/srv/fl"

[store]
path = "/data/x.db"

[tokens]
path = "/data/t.json"

[broker]
cert_manifest = "/data/certs/m.json"
`))
	require.NoError(t, err)
	assert.Equal(t, "/data/x.db", cfg.Store.Path)
	assert.Equal(t, "/data/t.json", cfg.Tokens.Path)
	assert.Equal(t, "/data/certs/m.json", cfg.Certs.Manifest)
}

// TestLoadRejectsRelativeStateDir: a relative state_dir would resolve
// against the service's working directory — refuse it.
func TestLoadRejectsRelativeStateDir(t *testing.T) {
	_, err := config.Load(writeCfg(t, t.TempDir(), `state_dir = "var/lib/x"`))
	assert.ErrorContains(t, err, "state_dir")
}

// TestLoadRejectsUnknownKey: a typo fails loudly instead of being
// silently ignored (and silently defaulted).
func TestLoadRejectsUnknownKey(t *testing.T) {
	_, err := config.Load(writeCfg(t, t.TempDir(), "[forward]\nmdoe = \"full\"\n"))
	assert.ErrorContains(t, err, "forward.mdoe")
}

// TestLoadRetiredKeysWarn: the keys retired by the 2026-11-16 overhaul
// still load (the live config carries them) but each is reported once
// in Warnings, and none of them changes behaviour.
func TestLoadRetiredKeysWarn(t *testing.T) {
	cfg, err := config.Load(writeCfg(t, t.TempDir(), `
[server]
inverter_listen = "0.0.0.0:14431"
http_listen = "0.0.0.0:18080"
lan_listen = ":1883"

[identity]
ips = ["192.168.4.20"]
hostnames = ["x"]

[broker]
data_dir = "/var/lib/foxess-local/broker"
capture = "/tmp/cap"
cert_manifest = "/var/lib/foxess-local/certs/cert-manifest.json"
`))
	require.NoError(t, err)
	require.Len(t, cfg.Warnings, 5) // four retired keys + the renamed cert_manifest
	joined := filepath.Join(cfg.Warnings...)
	for _, k := range []string{"server.lan_listen", "identity", "broker.data_dir", "broker.capture"} {
		assert.Contains(t, joined, k)
	}
}

// TestExampleConfigIsTheDefaults: the shipped config.example.toml (what
// the installer seeds) loads without warnings and is exactly the
// empty-file config — it documents defaults, it does not set them.
func TestExampleConfigIsTheDefaults(t *testing.T) {
	ex, err := config.Load(filepath.Join("..", "..", "config.example.toml"))
	require.NoError(t, err)
	assert.Empty(t, ex.Warnings)
	empty, err := config.Load(writeCfg(t, t.TempDir(), ""))
	require.NoError(t, err)
	assert.Equal(t, empty, ex)
}

// TestLoadBatteryCapacity: the usable capacity has no built-in default
// (0 = unknown: the setup wizard asks — audit M6); a configured pack is
// kept; a negative value is refused.
func TestLoadBatteryCapacity(t *testing.T) {
	for body, want := range map[string]int{"": 0, "[battery]\ncapacity_wh = 20720\n": 20720} {
		cfg, err := config.Load(writeCfg(t, t.TempDir(), body))
		require.NoError(t, err)
		assert.Equal(t, want, cfg.Battery.CapacityWh, body)
	}
	_, err := config.Load(writeCfg(t, t.TempDir(), "[battery]\ncapacity_wh = -1\n"))
	assert.ErrorContains(t, err, "battery.capacity_wh")
}

// TestInFile (F12): the config reports which UI-settable keys the file
// sets, so Settings can show a value's source (file or built-in default).
func TestInFile(t *testing.T) {
	cfg, err := config.Load(writeCfg(t, t.TempDir(), "[battery]\ncapacity_wh = 20720\n[access]\npublic_read_only = false\n"))
	require.NoError(t, err)
	assert.True(t, cfg.InFile("battery.capacity_wh"))
	assert.True(t, cfg.InFile("access.public_read_only"))
	assert.False(t, cfg.InFile("access.public_all_pages"))
	assert.False(t, cfg.InFile("forward.mode"))
}

// TestCertsManifest (audit M8): the certgen manifest location is
// [certs] manifest; the historical [broker] cert_manifest still works,
// with a warning naming the new key; both at once is ambiguous.
func TestCertsManifest(t *testing.T) {
	cfg, err := config.Load(writeCfg(t, t.TempDir(), "[certs]\nmanifest = \"/x/m.json\"\n"))
	require.NoError(t, err)
	assert.Equal(t, "/x/m.json", cfg.Certs.Manifest)
	assert.Empty(t, cfg.Warnings)

	cfg, err = config.Load(writeCfg(t, t.TempDir(), "[broker]\ncert_manifest = \"/y/m.json\"\n"))
	require.NoError(t, err)
	assert.Equal(t, "/y/m.json", cfg.Certs.Manifest)
	require.Len(t, cfg.Warnings, 1)
	assert.Contains(t, cfg.Warnings[0], "certs.manifest")

	_, err = config.Load(writeCfg(t, t.TempDir(), "[certs]\nmanifest = \"/x\"\n[broker]\ncert_manifest = \"/y\"\n"))
	require.ErrorContains(t, err, "certs.manifest")

	cfg, err = config.Load(writeCfg(t, t.TempDir(), ""))
	require.NoError(t, err)
	assert.Equal(t, "/var/lib/foxess-local/certs/cert-manifest.json", cfg.Certs.Manifest)
}
