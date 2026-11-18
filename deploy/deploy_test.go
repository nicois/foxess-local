package deploy

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"strings"
	"testing"
)

// decodeCloudRoot reads cloud-root.pem and returns its single
// CERTIFICATE block, failing the test on any deviation.
func decodeCloudRoot(t *testing.T) *x509.Certificate {
	t.Helper()
	data := CloudRootPEM // the embedded copy — what the binary trusts
	var blocks []*pem.Block
	rest := data
	for len(rest) > 0 {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			break
		}
		blocks = append(blocks, b)
	}
	if len(blocks) != 1 || blocks[0].Type != "CERTIFICATE" {
		t.Fatalf("cloud-root.pem: expected exactly one CERTIFICATE block, got %d", len(blocks))
	}
	cert, err := x509.ParseCertificate(blocks[0].Bytes)
	if err != nil {
		t.Fatalf("cloud-root.pem: parse: %v", err)
	}
	return cert
}

// TestCloudRootPEM pins the FoxESS vendor self-signed upstream cert —
// the real cloud (8.209.116.72:14431, the inverter's cloud-channel
// endpoint) presents it and no system store trusts it (captured
// 2026-11-15; 100-year validity, 2024–2124). It is embedded in the
// binary (deploy.CloudRootPEM) and trusted on the upstream leg unless
// forward.upstream_cas overrides it. A replaced file (vendor reissue or
// tampering) fails this test — re-pin deliberately.
func TestCloudRootPEM(t *testing.T) {
	cert := decodeCloudRoot(t)
	if cert.Subject.String() != cert.Issuer.String() {
		t.Fatalf("expected self-signed: subject %q issuer %q", cert.Subject, cert.Issuer)
	}
	// SHA-256 of the DER, the openssl -fingerprint -sha256 form.
	wantFingerprint := "0F:F6:D2:D0:B5:48:F0:A0:3D:CE:D3:1C:E7:62:1A:8C:94:97:BD:C0:74:D7:23:BC:15:20:94:E4:D2:99:C1:B7"
	sum := sha256.Sum256(cert.Raw)
	var got strings.Builder
	for i, b := range sum {
		if i > 0 {
			got.WriteByte(':')
		}
		got.WriteString(hex.EncodeToString([]byte{b}))
	}
	if strings.ToUpper(got.String()) != wantFingerprint {
		t.Fatalf("fingerprint mismatch: got %s want %s (deliberate re-pin?)", got.String(), wantFingerprint)
	}
	// The IP SAN set is the observed failover-endpoint set (profile
	// §8): a replaced cert with a different endpoint set fails here.
	wantIPs := []string{
		"8.209.116.72", "8.209.79.219", "8.209.80.124", "8.211.20.96",
		"47.91.86.144", "47.254.142.98", "47.254.159.190", "47.102.203.29",
		"139.224.232.119", "139.196.46.234", "101.133.233.208",
	}
	gotSet := map[string]bool{}
	for _, ip := range cert.IPAddresses {
		gotSet[ip.String()] = true
	}
	if len(gotSet) != len(wantIPs) {
		t.Fatalf("IP SAN count: got %d want %d", len(gotSet), len(wantIPs))
	}
	for _, ip := range wantIPs {
		if !gotSet[ip] {
			t.Errorf("IP SAN missing: %s", ip)
		}
	}
}
