package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foxesslocal/internal/config"
	"foxesslocal/internal/policy"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const sample = `
[server]
inverter_listen = ":8883"
http_listen = ":8080"

[retention]
raw_hours = 72
buckets_days = 365

[store]
path = "/data/foxess-local.db"
`

func TestLoadValid(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.toml")
	require.NoError(t, os.WriteFile(p, []byte(sample), 0o600))
	cfg, err := config.Load(p)
	require.NoError(t, err)
	assert.Equal(t, ":8883", cfg.Server.InverterListen)
	assert.Equal(t, ":8080", cfg.Server.HTTPListen)
	assert.Equal(t, "/data/foxess-local.db", cfg.Store.Path)
	assert.Equal(t, config.Retention{RawHours: 72, BucketDays: 365, MinFreeMB: config.DefaultMinFreeMB, CaptureMaxMB: config.DefaultCaptureMaxMB}, cfg.Retention)
}

func TestLoadDefaultsRetention(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.toml")
	// Zero values must take the defaults: 72 h raw, 365 d rollup.
	body := strings.Replace(sample, "raw_hours = 72", "raw_hours = 0", 1)
	body = strings.Replace(body, "buckets_days = 365", "buckets_days = 0", 1)
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	cfg, err := config.Load(p)
	require.NoError(t, err)
	assert.Equal(t, config.Retention{RawHours: 72, BucketDays: 365, MinFreeMB: config.DefaultMinFreeMB, CaptureMaxMB: config.DefaultCaptureMaxMB}, cfg.Retention)
}

func TestLoadChunkBytesRetention(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.toml")
	body := strings.Replace(sample, "buckets_days = 365", "buckets_days = 365\nchunk_bytes_hours = 48", 1)
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	cfg, err := config.Load(p)
	require.NoError(t, err)
	assert.Equal(t, 48, cfg.Retention.ChunkBytesHours)
}

func TestLoadChunkBytesDefaultOff(t *testing.T) {
	// No chunk_bytes_hours key: 0 = off (byte capture is a development /
	// trace-contribution opt-in, not a default).
	cfg, err := config.Load(writeCfg(t, t.TempDir(), sample))
	require.NoError(t, err)
	assert.Equal(t, 0, cfg.Retention.ChunkBytesHours)
}

func TestLoadRejectsNegativeChunkBytes(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.toml")
	body := strings.Replace(sample, "raw_hours = 72", "raw_hours = 72\nchunk_bytes_hours = -1", 1)
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	_, err := config.Load(p)
	require.ErrorContains(t, err, "chunk_bytes_hours")
}

func TestLoadAccessDefault(t *testing.T) {
	// No [access] table: public read-only defaults on (spec §5.5).
	cfg, err := config.Load(writeCfg(t, t.TempDir(), sample))
	require.NoError(t, err)
	assert.True(t, cfg.Access.PublicReadOnly)
	assert.False(t, cfg.Access.PublicAllPages, "tokenless sessions see the dashboard only by default")

	// Explicit off wins.
	body := strings.Replace(sample, `path = "/data/foxess-local.db"`,
		`path = "/data/foxess-local.db"

[access]
public_read_only = false`, 1)
	cfg, err = config.Load(writeCfg(t, t.TempDir(), body))
	require.NoError(t, err)
	assert.False(t, cfg.Access.PublicReadOnly)

	body = strings.Replace(sample, `path = "/data/foxess-local.db"`,
		`path = "/data/foxess-local.db"

[access]
public_all_pages = true`, 1)
	cfg, err = config.Load(writeCfg(t, t.TempDir(), body))
	require.NoError(t, err)
	assert.True(t, cfg.Access.PublicAllPages)
}

func TestLoadMissingFile(t *testing.T) {
	_, err := config.Load(filepath.Join(t.TempDir(), "nope.toml"))
	assert.ErrorContains(t, err, "load config")
}

func writeCfg(t *testing.T, dir, body string) string {
	t.Helper()
	p := filepath.Join(dir, "c.toml")
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	return p
}

// ---------- task 10: the plan-2 sections ----------

func TestLoadPlan2SectionDefaults(t *testing.T) {
	// The sample has no [forward]/[sim]/[tokens]/
	// [scheduler]: every default must land.
	p := writeCfg(t, t.TempDir(), sample)
	cfg, err := config.Load(p)
	require.NoError(t, err)
	assert.Equal(t, policy.ModeOff, cfg.Forward.Mode)
	// sim's min-SoC defaults (10/11) live in sim.New (task 8): the
	// config leaves zero to the sim.
	assert.Zero(t, cfg.Sim.MinSoc)
	assert.Zero(t, cfg.Sim.MinSocOnGrid)
}

func TestLoadBadForwardMode(t *testing.T) {
	body := strings.Replace(sample, `path = "/data/foxess-local.db"`, `path = "/data/foxess-local.db"

[forward]
mode = "bogus"`, 1)
	p := writeCfg(t, t.TempDir(), body)
	_, err := config.Load(p)
	assert.ErrorContains(t, err, `forward.mode: "bogus"`)
}

func TestLoadForwardModeValid(t *testing.T) {
	for _, mode := range []string{"off", "telemetry_only", "full"} {
		body := strings.Replace(sample, `path = "/data/foxess-local.db"`,
			`path = "/data/foxess-local.db"

[forward]
mode = "`+mode+`"`, 1)
		p := writeCfg(t, t.TempDir(), body)
		cfg, err := config.Load(p)
		require.NoError(t, err, mode)
		assert.Equal(t, policy.ForwardMode(mode), cfg.Forward.Mode)
	}
}

func TestLoadForwardUpstreamCAs(t *testing.T) {
	// Absent: empty — upstream-leg verification uses the system
	// roots only.
	cfg, err := config.Load(writeCfg(t, t.TempDir(), sample))
	require.NoError(t, err)
	assert.Empty(t, cfg.Forward.UpstreamCAs)

	body := strings.Replace(sample, `path = "/data/foxess-local.db"`,
		`path = "/data/foxess-local.db"

[forward]
mode = "telemetry_only"
upstream_cas = ["/etc/foxess-local/cloud-root.pem", "/etc/foxess-local/extra.pem"]`, 1)
	cfg, err = config.Load(writeCfg(t, t.TempDir(), body))
	require.NoError(t, err)
	assert.Equal(t, []string{"/etc/foxess-local/cloud-root.pem", "/etc/foxess-local/extra.pem"}, cfg.Forward.UpstreamCAs)
}

func TestLoadSimEnabledRequires(t *testing.T) {
	cases := []struct {
		name string
		sim  string
		want string
	}{
		{"no api key", "enabled = true", "sim.api_key"},
		{"no station", "enabled = true\napi_key = \"k\"", "sim.station_id"},
		{"no capacity", "enabled = true\napi_key = \"k\"\nstation_id = \"s1\"", "sim.capacity_kw"},
		{"zero capacity", "enabled = true\napi_key = \"k\"\nstation_id = \"s1\"\ncapacity_kw = 0", "sim.capacity_kw"},
		{"bad min soc", "enabled = true\napi_key = \"k\"\nstation_id = \"s1\"\ncapacity_kw = 5\nmin_soc = 101", "min_soc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := sample + "\n[sim]\n" + tc.sim + "\n"
			p := writeCfg(t, t.TempDir(), body)
			_, err := config.Load(p)
			assert.ErrorContains(t, err, tc.want)
		})
	}
	t.Run("valid sim", func(t *testing.T) {
		body := sample + "\n[sim]\nenabled = true\napi_key = \"k\"\nstation_id = \"s1\"\ncapacity_kw = 5\nmin_soc = 15\nmin_soc_on_grid = 16\n"
		p := writeCfg(t, t.TempDir(), body)
		cfg, err := config.Load(p)
		require.NoError(t, err)
		assert.True(t, cfg.Sim.Enabled)
		assert.Equal(t, 15, cfg.Sim.MinSoc)
		assert.Equal(t, 16, cfg.Sim.MinSocOnGrid)
	})
}

func TestLoadSchedulerWindows(t *testing.T) {
	// One valid window is accepted.
	body := sample + `
[[scheduler.windows]]
name = "morning"
slot = 1
start = 1536
end = 2304
fd_soc = 30
fd_pwr = 3000
`
	p := writeCfg(t, t.TempDir(), body)
	cfg, err := config.Load(p)
	require.NoError(t, err)
	require.Len(t, cfg.Scheduler.Windows, 1)
	assert.Equal(t, "morning", cfg.Scheduler.Windows[0].Name)

	cases := []struct {
		name string
		wins string
		want string
	}{
		{"slot out of range", `
[[scheduler.windows]]
name = "bad"
slot = 9
start = 1536
end = 2304
`, "slot 9"},
		{"end before start", `
[[scheduler.windows]]
name = "bad"
slot = 1
start = 2304
end = 1536
`, "end"},
		{"same-slot overlap", `
[[scheduler.windows]]
name = "a"
slot = 1
start = 1536
end = 2304
fd_soc = 90

[[scheduler.windows]]
name = "b"
slot = 1
start = 1792
end = 2560
fd_soc = 90
`, "overlap"},
		{"fd_soc below the installer floor", `
[[scheduler.windows]]
name = "low"
slot = 1
start = 1536
end = 2304
fd_soc = 5
`, "fd_soc 5 outside 10..100"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := writeCfg(t, t.TempDir(), sample+tc.wins)
			_, err := config.Load(p)
			assert.ErrorContains(t, err, tc.want)
		})
	}
}

// ---------- task 5b: the [net] exposure guard ----------

func TestLoadNet(t *testing.T) {
	// Defaults: allow_public false, no extra ranges.
	cfg, err := config.Load(writeCfg(t, t.TempDir(), sample))
	require.NoError(t, err)
	assert.False(t, cfg.Net.AllowPublic)
	assert.Empty(t, cfg.Net.ExtraAllowed)

	// A valid extra_allowed parses.
	body := sample + `
[net]
allow_public = true
extra_allowed = ["100.64.0.0/10"]
`
	cfg, err = config.Load(writeCfg(t, t.TempDir(), body))
	require.NoError(t, err)
	assert.True(t, cfg.Net.AllowPublic)
	assert.Equal(t, []string{"100.64.0.0/10"}, cfg.Net.ExtraAllowed)

	// An invalid CIDR is a hard error.
	bad := sample + `
[net]
extra_allowed = ["not-a-cidr"]
`
	_, err = config.Load(writeCfg(t, t.TempDir(), bad))
	assert.ErrorContains(t, err, `net.extra_allowed: "not-a-cidr" is not a CIDR`)
}

// TestUpdateSection: the release check is on unless disabled; the repo
// override is read as given.
func TestUpdateSection(t *testing.T) {
	p := writeCfg(t, t.TempDir(), sample)
	cfg, err := config.Load(p)
	require.NoError(t, err)
	assert.True(t, cfg.Update.CheckEnabled())
	assert.Empty(t, cfg.Update.Repo)

	p = writeCfg(t, t.TempDir(), sample+"\n[update]\ncheck = false\nrepo = \"someone/fork\"\n")
	cfg, err = config.Load(p)
	require.NoError(t, err)
	assert.False(t, cfg.Update.CheckEnabled())
	assert.Equal(t, "someone/fork", cfg.Update.Repo)
}

// TestDebugPprof: profiling is off by default and only ever on a
// loopback address (profiles expose internals; reach it over SSH).
func TestDebugPprof(t *testing.T) {
	cfg, err := config.Load(writeCfg(t, t.TempDir(), ""))
	require.NoError(t, err)
	assert.Empty(t, cfg.Debug.PprofListen)

	for _, ok := range []string{"127.0.0.1:6060", "[::1]:6060", "localhost:6060"} {
		cfg, err := config.Load(writeCfg(t, t.TempDir(), "[debug]\npprof_listen = \""+ok+"\"\n"))
		require.NoError(t, err, ok)
		assert.Equal(t, ok, cfg.Debug.PprofListen)
	}
	for _, bad := range []string{"0.0.0.0:6060", ":6060", "192.168.1.5:6060", "6060"} {
		_, err := config.Load(writeCfg(t, t.TempDir(), "[debug]\npprof_listen = \""+bad+"\"\n"))
		require.ErrorContains(t, err, "debug.pprof_listen", bad)
	}
}

// TestMinFree: the disk guard's critical level defaults to 200 MB; an
// explicit value wins; a negative one is refused.
func TestMinFree(t *testing.T) {
	cfg, err := config.Load(writeCfg(t, t.TempDir(), ""))
	require.NoError(t, err)
	assert.Equal(t, 200, cfg.Retention.MinFreeMB)

	cfg, err = config.Load(writeCfg(t, t.TempDir(), "[retention]\nmin_free_mb = 64\n"))
	require.NoError(t, err)
	assert.Equal(t, 64, cfg.Retention.MinFreeMB)

	_, err = config.Load(writeCfg(t, t.TempDir(), "[retention]\nmin_free_mb = -1\n"))
	require.ErrorContains(t, err, "retention.min_free_mb")
}

// TestNetHostNames: extra host names the web UI may be reached by (the
// Host check, audit S6) — none by default; each must be a plain name.
func TestNetHostNames(t *testing.T) {
	cfg, err := config.Load(writeCfg(t, t.TempDir(), ""))
	require.NoError(t, err)
	assert.Empty(t, cfg.Net.HostNames)
	cfg, err = config.Load(writeCfg(t, t.TempDir(), "[net]\nhost_names = [\"solar.example.org\"]\n"))
	require.NoError(t, err)
	assert.Equal(t, []string{"solar.example.org"}, cfg.Net.HostNames)
	_, err = config.Load(writeCfg(t, t.TempDir(), "[net]\nhost_names = [\"http://x/\"]\n"))
	require.ErrorContains(t, err, "net.host_names")
}

// TestQuirksTable (2026-11-19): [quirks] turns a known quirk on or off
// (the source Settings shows is the file); an unknown name is an error
// like any other unknown key.
func TestQuirksTable(t *testing.T) {
	cfg, err := config.Load(writeCfg(t, t.TempDir(), "[quirks]\neps_dropouts = true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Quirks["eps_dropouts"] || !cfg.InFile("quirks.eps_dropouts") {
		t.Fatalf("quirks %v, in file %v", cfg.Quirks, cfg.InFile("quirks.eps_dropouts"))
	}
	if _, err := config.Load(writeCfg(t, t.TempDir(), "[quirks]\nno_such_quirk = true\n")); err == nil {
		t.Fatal("an unknown quirk loaded")
	}
}

// TestSiteTimeZoneChecked: a config file's site zone must be a zone.
func TestSiteTimeZoneChecked(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.toml")
	require.NoError(t, os.WriteFile(p, []byte("[site]\ntime_zone = \"Mars/Olympus\"\n"), 0o600))
	_, err := config.Load(p)
	require.Error(t, err)
}
