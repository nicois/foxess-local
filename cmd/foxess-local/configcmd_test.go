package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"foxesslocal/internal/config"
)

// TestConfigCmd: `foxess-local config` validates a config without
// starting anything — the pre-restart check after an upgrade. It prints
// the effective settings (defaults resolved) to stdout, retired-key
// warnings to stderr, and fails on an invalid file.
func TestConfigCmd(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		p := filepath.Join(dir, "c.toml")
		require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
		return p
	}

	var out, errOut bytes.Buffer
	code := configCmd([]string{flagConfig, write("[identity]\nips = [\"1.2.3.4\"]\n")}, &out, &errOut)
	require.Equal(t, 0, code)
	require.Contains(t, out.String(), `state_dir = "/var/lib/foxess-local"`)
	require.Contains(t, out.String(), `inverter_listen = "0.0.0.0:14431"`)
	require.Contains(t, out.String(), `mode = "off"`)
	require.Contains(t, errOut.String(), "identity")

	out.Reset()
	errOut.Reset()
	code = configCmd([]string{flagConfig, write("[forward]\nmdoe = \"full\"\n")}, &out, &errOut)
	require.Equal(t, exitConfigInvalid, code)
	require.Contains(t, errOut.String(), "forward.mdoe")
}

// TestIssueURL (F8): problem reports go to the release repository's
// new-issue page — the configured [update] repo, else the stamped one;
// none in a source build.
func TestIssueURL(t *testing.T) {
	cfg := &config.Config{}
	if got := issueURL(cfg); got != "" && releaseRepo == "" {
		t.Fatalf("source build: %q", got)
	}
	cfg.Update.Repo = "owner/foxess-local"
	if got := issueURL(cfg); got != "https://github.com/owner/foxess-local/issues/new" {
		t.Fatalf("configured repo: %q", got)
	}
}
