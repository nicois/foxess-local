package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foxesslocal/internal/api"
	"foxesslocal/internal/config"

	"github.com/stretchr/testify/require"
)

// Flag names and the values most mints below share (goconst: each
// literal may occur once outside its definition).
const (
	flagConfig = "-config"
	flagName   = "-name"
	flagRole   = "-role"
	nameInst   = "installer"
	roleInst   = "installer"
)

// createArgs builds one tokensCreateRun arg set; an empty name or role
// omits the flag (the usage test).
func createArgs(cfgPath, name, role string) []string {
	args := []string{flagConfig, cfgPath}
	if name != "" {
		args = append(args, flagName, name)
	}
	if role != "" {
		args = append(args, flagRole, role)
	}
	return args
}

// bootstrapConfig writes the minimal config — state_dir only; every
// other setting takes its default — and returns it plus its path.
func bootstrapConfig(t *testing.T) (*config.Config, string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := dir + "/config.toml"
	require.NoError(t, os.WriteFile(cfgPath, []byte(`state_dir = "`+dir+`"`+"\n"), 0o600))
	cfg, err := config.Load(cfgPath)
	require.NoError(t, err)
	return cfg, cfgPath
}

// captureStdout runs fn with os.Stdout pointed at a pipe and returns
// whatever it printed.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	old := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = old }()
	fn()
	require.NoError(t, w.Close())
	b, err := io.ReadAll(r)
	require.NoError(t, err)
	return string(b)
}

// TestRunRefusesStartWithoutInstallerToken: a persisted token store with
// no installer-role token is a lockout (token minting is
// installer-only), so run must fail before the heavy setup, naming the
// recovery command. The positive case is pinned by the smoke (it seeds
// the installer before run).
func TestRunRefusesStartWithoutInstallerToken(t *testing.T) {
	cfg, _ := bootstrapConfig(t)
	_, err := run(cfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "tokens create")
	require.Contains(t, err.Error(), "-role installer")
}

// TestTokensCreateMintsAndPersists: the CLI mints one token into the
// configured store file (0600), prints the plaintext exactly once, and
// the minted value authenticates against a fresh store load.
func TestTokensCreateMintsAndPersists(t *testing.T) {
	cfg, cfgPath := bootstrapConfig(t)
	var code int
	out := captureStdout(t, func() {
		code = tokensCreateRun(createArgs(cfgPath, nameInst, roleInst))
	})
	require.Equal(t, 0, code, "stdout: %s", out)
	plain := strings.Fields(out)[0]
	require.True(t, strings.HasPrefix(plain, "fl_"))
	ts, err := api.NewTokenStore(cfg.Tokens.Path, time.Now)
	require.NoError(t, err)
	tk, ok := ts.Lookup(plain)
	require.True(t, ok)
	require.Equal(t, api.RoleInstaller, tk.Role)
	fi, err := os.Stat(cfg.Tokens.Path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
}

// TestTokensCreateDuplicateName: the store's unique-name rule surfaces
// as a failed mint, not a second token.
func TestTokensCreateDuplicateName(t *testing.T) {
	_, cfgPath := bootstrapConfig(t)
	var first, second int
	captureStdout(t, func() {
		first = tokensCreateRun(createArgs(cfgPath, nameInst, roleInst))
		second = tokensCreateRun(createArgs(cfgPath, nameInst, "standard"))
	})
	require.Equal(t, 0, first)
	require.Equal(t, exitTokensFailed, second)
}

// TestTokensCreateRequiresFlags: -name and -role are both mandatory
// (no silent installer default), and a missing subcommand is usage.
func TestTokensCreateRequiresFlags(t *testing.T) {
	_, cfgPath := bootstrapConfig(t)
	var code int
	captureStdout(t, func() {
		code = tokensCreateRun(createArgs(cfgPath, "", roleInst))
	})
	require.Equal(t, exitUsage, code)
	require.Equal(t, exitUsage, tokensCmd(nil))
	require.Equal(t, exitUsage, tokensCmd([]string{"bogus"}))
}

// TestTokensCreateUnknownRole: an unparseable role is a failed mint.
func TestTokensCreateUnknownRole(t *testing.T) {
	_, cfgPath := bootstrapConfig(t)
	var code int
	captureStdout(t, func() {
		code = tokensCreateRun(createArgs(cfgPath, "x", "bogus"))
	})
	require.Equal(t, exitTokensFailed, code)
}

// TestTokensCreateDefaultsToStateDir: with no [tokens] key the CLI
// mints into <state_dir>/tokens.json — the same file run reads.
func TestTokensCreateDefaultsToStateDir(t *testing.T) {
	cfg, cfgPath := bootstrapConfig(t)
	var code int
	captureStdout(t, func() {
		code = tokensCreateRun(createArgs(cfgPath, "x", roleInst))
	})
	require.Equal(t, 0, code)
	require.Equal(t, filepath.Join(cfg.StateDir, "tokens.json"), cfg.Tokens.Path)
	require.FileExists(t, cfg.Tokens.Path)
}
