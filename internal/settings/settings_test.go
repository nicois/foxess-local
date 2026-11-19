package settings

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"foxesslocal/internal/quirks"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"foxesslocal/internal/config"
	"foxesslocal/internal/policy"
	"foxesslocal/internal/store"
)

var testNow = time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)

func clock() time.Time { return testNow }

func setup(t *testing.T, cfgBody string) (*store.Store, *config.Config) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "c.toml")
	require.NoError(t, os.WriteFile(p, []byte(cfgBody), 0o600))
	cfg, err := config.Load(p)
	require.NoError(t, err)
	dbPath := filepath.Join(dir, "s.db")
	st, err := store.Open(dbPath, "test")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return st, cfg
}

// TestSources (F12): built-in default, then the file, then the UI —
// each value says where it came from.
func TestSources(t *testing.T) {
	st, cfg := setup(t, "[access]\npublic_all_pages = true\n")
	s, err := New(st, cfg, clock)
	require.NoError(t, err)
	v := s.Get()
	assert.Equal(t, 0, v.CapacityWh)
	assert.True(t, v.PublicReadOnly)
	assert.True(t, v.PublicAllPages)
	assert.Equal(t, policy.ModeOff, v.ForwardMode)
	src := s.Sources()
	assert.Equal(t, SourceDefault, src[KeyCapacityWh])
	assert.Equal(t, SourceFile, src[KeyPublicAllPages])

	require.NoError(t, s.Set(KeyCapacityWh, "41930", "installer"))
	assert.Equal(t, 41930, s.Get().CapacityWh)
	assert.Equal(t, SourceUI, s.Sources()[KeyCapacityWh])
}

// TestPersistsAndWins: a UI value survives a restart and beats the file;
// Reset returns to the file's value.
func TestPersistsAndWins(t *testing.T) {
	st, cfg := setup(t, "[battery]\ncapacity_wh = 20720\n")
	s, err := New(st, cfg, clock)
	require.NoError(t, err)
	require.NoError(t, s.Set(KeyCapacityWh, "41930", "installer"))
	require.NoError(t, s.Set(KeyPublicReadOnly, "false", "installer"))

	again, err := New(st, cfg, clock)
	require.NoError(t, err)
	assert.Equal(t, 41930, again.Get().CapacityWh)
	assert.False(t, again.Get().PublicReadOnly)

	require.NoError(t, again.Reset(KeyCapacityWh, "installer"))
	assert.Equal(t, 20720, again.Get().CapacityWh)
	assert.Equal(t, SourceFile, again.Sources()[KeyCapacityWh])
}

// TestValidation: values outside the allowed shape are refused and
// change nothing.
func TestValidation(t *testing.T) {
	st, cfg := setup(t, "")
	s, err := New(st, cfg, clock)
	require.NoError(t, err)
	for k, v := range map[string]string{
		KeyCapacityWh: "0", KeyPublicReadOnly: "maybe", KeyForwardMode: "turbo", "nonsense": "1",
	} {
		require.Error(t, s.Set(k, v, "installer"), "%s=%s", k, v)
	}
	assert.Equal(t, 0, s.Get().CapacityWh)
	assert.Error(t, s.Set(KeyCapacityWh, "999999", "installer"), "an implausible pack")
}

// TestForwardModePersisted: the forward mode set in the UI survives a
// restart (it was held in memory only, so a restart reverted it).
func TestForwardModePersisted(t *testing.T) {
	st, cfg := setup(t, "[forward]\nmode = \"telemetry_only\"\n")
	s, err := New(st, cfg, clock)
	require.NoError(t, err)
	assert.Equal(t, policy.ModeTelemetryOnly, s.Get().ForwardMode)
	require.NoError(t, s.Set(KeyForwardMode, "full", "installer"))
	again, err := New(st, cfg, clock)
	require.NoError(t, err)
	assert.Equal(t, policy.ModeFull, again.Get().ForwardMode)
}

// TestSetupCompleted: the wizard's completion is recorded once.
func TestSetupCompleted(t *testing.T) {
	st, cfg := setup(t, "")
	s, err := New(st, cfg, clock)
	require.NoError(t, err)
	assert.False(t, s.SetupCompleted())
	require.NoError(t, s.CompleteSetup("installer"))
	again, err := New(st, cfg, clock)
	require.NoError(t, err)
	assert.True(t, again.SetupCompleted())
}

// TestTuningDefaults (owner, 2026-11-18): the write delay and the
// battery-alert knobs have defaults, take the file's values, and are
// range-checked; they are the expert-level settings.
func TestTuningDefaults(t *testing.T) {
	st, cfg := setup(t, "[alerts]\nsoc_threshold_pct = 15\n")
	s, err := New(st, cfg, clock)
	require.NoError(t, err)
	v := s.Get()
	assert.Equal(t, 60*time.Second, v.WriteDelay)
	assert.Equal(t, 15, v.SocThresholdPct)
	assert.Equal(t, 120*time.Minute, v.SocHorizon)
	assert.InDelta(t, 0.05, v.SocPValue, 1e-9)
	assert.Equal(t, 30*time.Minute, v.SocWindow)
	assert.Equal(t, SourceFile, s.Sources()[KeySocThreshold])

	for k, bad := range map[string]string{
		KeyWriteDelay: "901", KeySocThreshold: "101", KeySocHorizon: "5",
		KeySocPValue: "0.6", KeySocWindow: "1",
	} {
		require.Error(t, s.Set(k, bad, "expert"), "%s=%s", k, bad)
	}
	require.NoError(t, s.Set(KeyWriteDelay, "900", "expert"))
	require.NoError(t, s.Set(KeySocPValue, "0.01", "expert"))
	require.NoError(t, s.Set(KeySocThreshold, "0", "expert")) // 0 = off
	v = s.Get()
	assert.Equal(t, 15*time.Minute, v.WriteDelay)
	assert.InDelta(t, 0.01, v.SocPValue, 1e-9)
	assert.Equal(t, 0, v.SocThresholdPct)

	assert.True(t, Expert(KeySocWindow))
	assert.True(t, Expert(KeyWriteDelay))
	assert.False(t, Expert(KeyNotifyNtfy))
	assert.False(t, Expert(KeyCapacityWh))
}

// TestOnChange: a change (or a reset) tells the host the new values —
// the write delay applies at once, without a restart.
func TestOnChange(t *testing.T) {
	st, cfg := setup(t, "")
	s, err := New(st, cfg, clock)
	require.NoError(t, err)
	var got []time.Duration
	s.OnChange(func(v Values) { got = append(got, v.WriteDelay) })
	require.NoError(t, s.Set(KeyWriteDelay, "900", "expert"))
	require.NoError(t, s.Set(KeyWriteDelay, "900", "expert")) // no change: no call
	require.NoError(t, s.Reset(KeyWriteDelay, "expert"))
	assert.Equal(t, []time.Duration{15 * time.Minute, time.Minute}, got)
}

// TestUpdateSettings (2026-11-19): the update channel, the check interval
// (default a week; 0 = off; the old [update] check = false still means
// off) and auto-apply — installer settings, not expert ones.
func TestUpdateSettings(t *testing.T) {
	st, cfg := setup(t, "")
	s, err := New(st, cfg, clock)
	require.NoError(t, err)
	v := s.Get()
	assert.Equal(t, 7*24*time.Hour, v.UpdateInterval)
	assert.False(t, v.UpdatePrerelease)
	assert.False(t, v.UpdateAutoApply)
	require.NoError(t, s.Set(KeyUpdateInterval, "6", "installer"))
	require.NoError(t, s.Set(KeyUpdatePrerelease, "true", "installer"))
	require.NoError(t, s.Set(KeyUpdateAutoApply, "true", "installer"))
	v = s.Get()
	assert.Equal(t, 6*time.Hour, v.UpdateInterval)
	assert.True(t, v.UpdatePrerelease && v.UpdateAutoApply)
	require.Error(t, s.Set(KeyUpdateInterval, "721", "installer"))
	assert.False(t, Expert(KeyUpdateInterval), "an installer setting")

	st2, cfgOff := setup(t, "[update]\ncheck = false\n")
	off, err := New(st2, cfgOff, clock)
	require.NoError(t, err)
	assert.Equal(t, time.Duration(0), off.Get().UpdateInterval)
}

// TestQuirkSettings (2026-11-19): a quirk is off by default, the file
// can turn it on, the UI can override either way, and an expert may set
// it (it changes only how values are shown).
func TestQuirkSettings(t *testing.T) {
	key := quirks.Key(quirks.EPSDropouts)
	st, cfg := setup(t, "")
	s, err := New(st, cfg, clock)
	require.NoError(t, err)
	assert.False(t, s.Get().Quirks[quirks.EPSDropouts])
	assert.Equal(t, SourceDefault, s.Sources()[key])
	require.NoError(t, s.Set(key, "true", "expert"))
	assert.True(t, s.Get().Quirks[quirks.EPSDropouts])
	assert.Equal(t, SourceUI, s.Sources()[key])
	require.Error(t, s.Set(key, "maybe", "expert"))
	assert.True(t, Expert(key), "an expert setting")

	st2, cfgOn := setup(t, "[quirks]\neps_dropouts = true\n")
	on, err := New(st2, cfgOn, clock)
	require.NoError(t, err)
	assert.True(t, on.Get().Quirks[quirks.EPSDropouts])
	assert.Equal(t, SourceFile, on.Sources()[key])
}

// TestSiteTimeZone (owner, 2026-11-19): the site's zone is its own
// setting, unset until chosen — the host's zone is never assumed. From
// the config file ([site] time_zone) or the UI; an IANA name only.
func TestSiteTimeZone(t *testing.T) {
	st, cfg := setup(t, "")
	s, err := New(st, cfg, clock)
	require.NoError(t, err)
	assert.Nil(t, s.Get().SiteZone, "unset by default")
	assert.False(t, Expert(KeySiteTimeZone), "an installer choice")
	for _, bad := range []string{"", "Local", "Australia/Atlantis", "../etc/passwd"} {
		require.Error(t, s.Set(KeySiteTimeZone, bad, "installer"), "%q", bad)
	}
	require.NoError(t, s.Set(KeySiteTimeZone, "Australia/Melbourne", "installer"))
	require.NotNil(t, s.Get().SiteZone)
	assert.Equal(t, "Australia/Melbourne", s.Get().SiteZone.String())

	st2, cfg2 := setup(t, "[site]\ntime_zone = \"Europe/London\"\n")
	s2, err := New(st2, cfg2, clock)
	require.NoError(t, err)
	assert.Equal(t, "Europe/London", s2.Get().SiteZone.String())
	assert.Equal(t, SourceFile, s2.Sources()[KeySiteTimeZone])
}
