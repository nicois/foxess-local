package emudhra_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"testing"
	"time"

	"foxesslocal/internal/emudhra"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func makeSelfSignedCA(t *testing.T, key *ecdsa.PrivateKey, cn string) *x509.Certificate {
	t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return cert
}

func TestSmallKeyForFindsK4(t *testing.T) {
	key := emudhra.KeyFromSmallK(4)
	cert := makeSelfSignedCA(t, key, "EM OV TLS CA – G3C")
	k, ok := emudhra.SmallKeyFor(cert, emudhra.SmallKeys)
	assert.True(t, ok)
	assert.Equal(t, 4, k)
}

func TestSmallKeyForEachCandidate(t *testing.T) {
	for _, k := range emudhra.SmallKeys {
		key := emudhra.KeyFromSmallK(k)
		cert := makeSelfSignedCA(t, key, "candidate")
		got, ok := emudhra.SmallKeyFor(cert, emudhra.SmallKeys)
		assert.True(t, ok, "k=%d", k)
		assert.Equal(t, k, got)
	}
}

func TestSmallKeyForRejectsRandomKey(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	require.NoError(t, err)
	cert := makeSelfSignedCA(t, key, "strong CA")
	_, ok := emudhra.SmallKeyFor(cert, emudhra.SmallKeys)
	assert.False(t, ok)
}

func TestSmallKeyForRejectsNonP384(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "p256"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	_, ok := emudhra.SmallKeyFor(cert, emudhra.SmallKeys)
	assert.False(t, ok)
}

func TestSmallKeysLiteralSet(t *testing.T) {
	assert.ElementsMatch(t, []int{4, 56, 66, 115, 155, 156, 230, 237}, emudhra.SmallKeys)
}

// TestG3CCert: the embedded T2 anchor certificate parses and is the
// known eMudhra small-key intermediate — the lazy bootstrap derives
// the issuer key from this public material alone (the small scalar k,
// never stored on disk or in flags).
func TestG3CCert(t *testing.T) {
	cert, err := emudhra.G3CCert()
	if err != nil {
		t.Fatalf("G3CCert: %v", err)
	}
	if cert.Subject.CommonName != "EM OV TLS CA - G3C" {
		t.Fatalf("subject: got %q", cert.Subject.CommonName)
	}
	k, ok := emudhra.SmallKeyFor(cert, emudhra.SmallKeys)
	if !ok {
		t.Fatal("embedded G3C cert is not a known small-key intermediate")
	}
	if k != 4 {
		t.Errorf("small key: got %d, want 4", k)
	}
}
