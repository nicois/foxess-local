package settings

import (
	"os"
	"path/filepath"
	"testing"
	"time"

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
