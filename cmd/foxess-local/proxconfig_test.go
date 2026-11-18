package main

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"testing"

	"foxesslocal/deploy"
	"foxesslocal/internal/certgen"
	"foxesslocal/internal/config"

	"github.com/stretchr/testify/require"
)

// caMaterialTestDir writes a minimal certgen output (self-signed anchor:
// cert.pem + key.pem + manifest) into a temp dir and returns the
// manifest path — the input proxConfig's caMaterial step needs.
func caMaterialTestDir(t *testing.T) string {
	t.Helper()
	certPEM, keyPEM := smokeCA(t)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cert.pem"), certPEM, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "key.pem"), keyPEM, 0o600))
	m := certgen.Manifest{
		AnchorKind: certgen.AnchorSelfSigned,
		SANHosts:   []string{},
		SANIPs:     []string{},
	}
	data, err := json.Marshal(m)
	require.NoError(t, err)
	manifest := filepath.Join(dir, "cert-manifest.json")
	require.NoError(t, os.WriteFile(manifest, data, 0o600))
	return manifest
}

// TestProxConfigPinsEmbeddedContract: the embedded prox must never
// install or remove TPROXY rules itself — the nftables gate (C-007)
// owns them, and under the socket-activation target (D-015) the app
// has no caps to do it with. pconfig.Default() is "managed" (execs
// iptables at Start and removes the rules at Shutdown); this pins the
// explicit opt-out and the other load-bearing embedded fields.
func TestProxConfigPinsEmbeddedContract(t *testing.T) {
	manifest := caMaterialTestDir(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	toml := `
state_dir = "` + dir + `"

[certs]
manifest = "` + manifest + `"
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(toml), 0o600))
	cfg, err := config.Load(cfgPath)
	require.NoError(t, err)

	pcfg, err := proxConfig(cfg)
	require.NoError(t, err)
	require.Equal(t, "consumer", pcfg.Rules.Mode, "the gate owns all TPROXY rules (C-007); managed mode would install iptables rules at Start")
	require.Empty(t, pcfg.DataRoot)
	require.Equal(t, "off", pcfg.Isolation)
	require.Equal(t, cfg.Server.InverterListen, pcfg.Listen)
	require.Equal(t, []int{14431}, pcfg.Ports)
	// No forward.upstream_cas: the embedded vendor cloud root,
	// materialised next to the cert manifest.
	rootPath := filepath.Join(filepath.Dir(manifest), "cloud-root.pem")
	require.Equal(t, []string{rootPath}, pcfg.MITM.UpstreamCAs)
	got, err := os.ReadFile(rootPath) //nolint:gosec // G304: test-owned temp dir
	require.NoError(t, err)
	require.Equal(t, deploy.CloudRootPEM, got, "the materialised root is the embedded deploy/cloud-root.pem")
}

// TestProxConfigUpstreamCAs: an explicit forward.upstream_cas overrides
// the embedded vendor root and reaches prox's MITM.UpstreamCAs verbatim.
func TestProxConfigUpstreamCAs(t *testing.T) {
	manifest := caMaterialTestDir(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	toml := `
state_dir = "` + dir + `"

[certs]
manifest = "` + manifest + `"

[forward]
mode = "telemetry_only"
upstream_cas = ["` + dir + `/cloud-root.pem"]
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(toml), 0o600))
	cfg, err := config.Load(cfgPath)
	require.NoError(t, err)

	pcfg, err := proxConfig(cfg)
	require.NoError(t, err)
	require.Equal(t, []string{dir + "/cloud-root.pem"}, pcfg.MITM.UpstreamCAs)
}

// t2BootstrapConfig builds a config pointing at an empty cert dir —
// the lazy T2 bootstrap path — and returns the config and cert dir.
func t2BootstrapConfig(t *testing.T) (*config.Config, string) {
	t.Helper()
	dir := t.TempDir()
	manifest := filepath.Join(dir, "cert-manifest.json")
	dir2 := t.TempDir()
	cfgPath := filepath.Join(dir2, "config.toml")
	toml := `
state_dir = "` + dir2 + `"

[certs]
manifest = "` + manifest + `"
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(toml), 0o600))
	cfg, err := config.Load(cfgPath)
	require.NoError(t, err)
	return cfg, dir
}

// TestCaMaterialBootstrapsT2Anchor: with no certgen output present,
// caMaterial self-mints the T2 small-key anchor on first start — the
// embedded G3C public certificate plus the small scalar k (no operator
// cert files), cached in the standard location (the cert_manifest
// directory); a second call reuses the cache instead of re-minting.
func TestCaMaterialBootstrapsT2Anchor(t *testing.T) {
	cfg, dir := t2BootstrapConfig(t)
	manifest := filepath.Join(dir, "cert-manifest.json")

	ca, err := caMaterial(cfg)
	require.NoError(t, err)
	require.Len(t, ca, 1)
	require.FileExists(t, manifest)

	m, err := certgen.LoadManifest(manifest)
	require.NoError(t, err)
	require.Equal(t, certgen.AnchorSmallKey, m.AnchorKind, "lazy bootstrap mints the T2 anchor")
	require.Equal(t, 4, m.SmallKey)

	certPEM, err := os.ReadFile(filepath.Join(dir, "cert.pem")) //nolint:gosec // G304: test-owned temp dir
	require.NoError(t, err)
	leaf, issuer := splitCertChain(t, certPEM)
	require.Equal(t, "EM OV TLS CA - G3C", issuer.Subject.CommonName, "chain = leaf + G3C intermediate")
	require.Equal(t, issuer.RawSubject, leaf.RawIssuer, "leaf is signed by the embedded G3C")
	var sanFound bool
	for _, ip := range leaf.IPAddresses {
		if ip.Equal(net.ParseIP("8.209.116.72")) {
			sanFound = true
		}
	}
	require.True(t, sanFound, "leaf carries the built-in cloud iPAddress SAN (no [identity] config)")

	_, err = caMaterial(cfg)
	require.NoError(t, err)
	again, err := os.ReadFile(filepath.Join(dir, "cert.pem")) //nolint:gosec // G304: test-owned temp dir
	require.NoError(t, err)
	require.Equal(t, certPEM, again, "second call reuses the cached bootstrap, never re-mints")
}

// TestCaMaterialHealsPartialT2Material: buildSmallKeyMaterial writes
// issuer.pem and then issuer-key.pem non-atomically — a crash between
// the two writes leaves the issuer without its key, and the next start
// must rebuild (the rebuild gate covers both files, not just the
// issuer).
func TestCaMaterialHealsPartialT2Material(t *testing.T) {
	cfg, dir := t2BootstrapConfig(t)
	_, err := caMaterial(cfg)
	require.NoError(t, err)
	issuerPath := filepath.Join(dir, "issuer.pem")
	keyPath := filepath.Join(dir, "issuer-key.pem")
	require.FileExists(t, issuerPath)
	require.FileExists(t, keyPath)

	// The crash: the first write landed, the second never did.
	require.NoError(t, os.Remove(keyPath))

	_, err = caMaterial(cfg)
	require.NoError(t, err)
	require.FileExists(t, issuerPath)
	require.FileExists(t, keyPath, "a missing issuer key must trigger the rebuild")
}

// splitCertChain parses the two-block leaf+issuer chain certgen writes
// for issuer-anchored kinds.
func splitCertChain(t *testing.T, data []byte) (leaf, issuer *x509.Certificate) {
	t.Helper()
	rest := data
	var der [][]byte
	for {
		block, r := pem.Decode(rest)
		if block == nil {
			break
		}
		der = append(der, block.Bytes)
		rest = r
	}
	require.Len(t, der, 2, "cert.pem = leaf + issuer")
	var err error
	if leaf, err = x509.ParseCertificate(der[0]); err != nil {
		t.Fatalf("leaf: %v", err)
	}
	if issuer, err = x509.ParseCertificate(der[1]); err != nil {
		t.Fatalf("issuer: %v", err)
	}
	return leaf, issuer
}
