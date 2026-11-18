package certgen_test

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"foxesslocal/internal/certgen"
	"foxesslocal/internal/emudhra"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var now = func() time.Time { return time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC) }

// testHost is the SAN hostname used throughout the certgen tests.
const testHost = "mqtt.foxesscloud.com"

func buildChain(t *testing.T, interKey *ecdsa.PrivateKey) (root *x509.Certificate, inter *x509.Certificate) {
	t.Helper()
	rootKey, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	require.NoError(t, err)
	mk := func(subject string, tmplParent *x509.Certificate, signer *ecdsa.PrivateKey, pub crypto.PublicKey) *x509.Certificate {
		tmpl := &x509.Certificate{
			SerialNumber:          big.NewInt(1),
			Subject:               pkix.Name{CommonName: subject},
			NotBefore:             now().Add(-time.Hour),
			NotAfter:              now().Add(10 * 365 * 24 * time.Hour),
			IsCA:                  true,
			KeyUsage:              x509.KeyUsageCertSign,
			BasicConstraintsValid: true,
		}
		parent := tmplParent
		if parent == nil { // self-signed root: sign with the template itself
			parent = tmpl
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, pub, signer)
		require.NoError(t, err)
		cert, err := x509.ParseCertificate(der)
		require.NoError(t, err)
		return cert
	}
	root = mk("eMudhra Test Root", nil, rootKey, &rootKey.PublicKey)
	inter = mk("EM OV TLS CA – G3C (test)", root, rootKey, interKey.Public())
	return root, inter
}

func parseLeaf(t *testing.T, certPEM []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(certPEM)
	require.NotNil(t, block)
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	return cert
}

func TestGenerateSelfSigned(t *testing.T) {
	certPEM, _, m, err := certgen.Generate(certgen.Options{
		Kind:     certgen.AnchorSelfSigned,
		SANHosts: []string{testHost},
		SANIPs:   []netip.Addr{netip.MustParseAddr("192.168.1.50")},
	}, now)
	require.NoError(t, err)
	leaf := parseLeaf(t, certPEM)
	assert.Equal(t, []string{testHost}, leaf.DNSNames)
	_, rest := pem.Decode(certPEM)
	assert.Empty(t, rest, "selfsigned cert.pem must be a single leaf block")
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	_, err = leaf.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	require.NoError(t, err)
	assert.Equal(t, certgen.AnchorSelfSigned, m.AnchorKind)
	assert.GreaterOrEqual(t, m.NotAfter.Sub(m.NotBefore), 5*365*24*time.Hour)
}

func TestGenerateSmallKeyAnchor(t *testing.T) {
	root, inter := buildChain(t, emudhra.KeyFromSmallK(4))
	certPEM, _, m, err := certgen.Generate(certgen.Options{
		Kind:       certgen.AnchorSmallKey,
		IssuerCert: inter,
		IssuerKey:  emudhra.KeyFromSmallK(4),
		SANHosts:   []string{testHost},
	}, now)
	require.NoError(t, err)
	leaf := parseLeaf(t, certPEM)
	// spec §5 T2: cert.pem must present the leaf followed by its issuer.
	block, rest := pem.Decode(certPEM)
	require.NotNil(t, block)
	issuerBlock, rest2 := pem.Decode(rest)
	require.NotNil(t, issuerBlock, "cert.pem must carry the issuer certificate after the leaf")
	assert.Empty(t, rest2, "cert.pem must carry exactly two certificates")
	issued, err := x509.ParseCertificate(issuerBlock.Bytes)
	require.NoError(t, err)
	assert.Equal(t, inter.Raw, issued.Raw)
	// Each link checked directly. Not leaf.Verify: since Go 1.27.3 the
	// chain builder refuses any chain through a small-scalar key (a
	// strong-key chain from the same code verifies) — the inverter, the
	// only verifier that matters here, is not Go.
	require.NoError(t, leaf.CheckSignatureFrom(issued), "leaf signed by the small-key intermediate")
	require.NoError(t, issued.CheckSignatureFrom(root), "intermediate signed by the root")
	assert.Equal(t, issued.Subject.String(), leaf.Issuer.String())
	assert.Equal(t, issued.SubjectKeyId, leaf.AuthorityKeyId)
	assert.Contains(t, leaf.ExtKeyUsage, x509.ExtKeyUsageServerAuth)
	require.NoError(t, leaf.VerifyHostname(testHost))
	assert.Equal(t, 4, m.SmallKey)
}

func TestGenerateSmallKeyAnchorRejectsStrongIssuer(t *testing.T) {
	strong := mustP384(t)
	_, inter := buildChain(t, strong)
	_, _, _, err := certgen.Generate(certgen.Options{
		Kind:       certgen.AnchorSmallKey,
		IssuerCert: inter,
		IssuerKey:  strong,
	}, now)
	assert.Error(t, err)
}

func mustP384(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	require.NoError(t, err)
	return k
}

func TestManifestRoundTrip(t *testing.T) {
	m := certgen.Manifest{
		AnchorKind: certgen.AnchorSmallKey,
		SmallKey:   4,
		SANHosts:   []string{testHost},
		NotBefore:  now(),
		NotAfter:   now().Add(5 * 365 * 24 * time.Hour),
	}
	p := filepath.Join(t.TempDir(), "cert-manifest.json")
	require.NoError(t, certgen.WriteManifest(p, m))
	got, err := certgen.LoadManifest(p)
	require.NoError(t, err)
	assert.Equal(t, m, got)
}
