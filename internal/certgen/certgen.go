// Package certgen mints the foxess-local broker certificate from a chosen
// trust anchor (spec §5, T1–T4) and records the result in a manifest.
package certgen

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/netip"
	"time"

	"foxesslocal/internal/emudhra"
)

// AnchorKind selects the trust anchor the broker certificate is minted from.
type AnchorKind string

// The four trust anchor kinds (spec §5, T1–T4).
const (
	AnchorSelfSigned AnchorKind = "selfsigned"   // T1: inverter does not validate.
	AnchorSmallKey   AnchorKind = "small-key"    // T2: compromised eMudhra intermediate.
	AnchorExtracted  AnchorKind = "extracted-ca" // T3: CA material extracted from firmware.
	AnchorOwnCA      AnchorKind = "own-ca"       // T4: custom CA installed in the inverter.
)

// IsValidAnchor reports whether k is one of the four supported anchor kinds.
func IsValidAnchor(k AnchorKind) bool {
	switch k {
	case AnchorSelfSigned, AnchorSmallKey, AnchorExtracted, AnchorOwnCA:
		return true
	}
	return false
}

// DefaultValidity is 5 years (spec §5: embedded clients dislike short certs).
const DefaultValidity = 5 * 365 * 24 * time.Hour

// serialMaxBits bounds the certificate serial number to 20 bytes (RFC 5280).
const serialMaxBits = 159

// Options selects the anchor and SAN material for Generate.
type Options struct {
	Kind       AnchorKind
	IssuerCert *x509.Certificate // nil for selfsigned
	IssuerKey  crypto.Signer     // for small-key: *ecdsa.PrivateKey holding a SmallKey value
	SANHosts   []string
	SANIPs     []netip.Addr
	Validity   time.Duration // 0 → DefaultValidity
}

// Generate mints the broker leaf certificate and returns PEM blobs plus the
// manifest describing the anchor actually used. For issuer-anchored kinds
// certPEM carries the leaf followed by the issuer certificate so the broker
// serves the chain (spec §5: T2 and later present leaf + intermediate).
func Generate(opts Options, now func() time.Time) (certPEM, keyPEM []byte, m Manifest, err error) {
	if opts.Validity <= 0 {
		opts.Validity = DefaultValidity
	}
	if err := validateOpts(opts); err != nil {
		return nil, nil, m, err
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		return nil, nil, m, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), serialMaxBits))
	if err != nil {
		return nil, nil, m, err
	}
	leaf := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "foxess-local broker"},
		NotBefore:    now().Add(-time.Hour), // embedded-client clock skew
		NotAfter:     now().Add(opts.Validity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     opts.SANHosts,
		IPAddresses:  addrs(opts.SANIPs),
	}

	signerCert, signerKey := opts.IssuerCert, opts.IssuerKey
	if opts.Kind == AnchorSelfSigned {
		signerCert, signerKey = leaf, leafKey
	}
	if opts.Kind == AnchorSmallKey {
		k, ok := emudhra.SmallKeyFor(opts.IssuerCert, emudhra.SmallKeys)
		if !ok {
			return nil, nil, m, fmt.Errorf("issuer %q is not a known eMudhra small-key intermediate", opts.IssuerCert.Subject.CommonName)
		}
		m.SmallKey = k
	}

	der, err := x509.CreateCertificate(rand.Reader, leaf, signerCert, &leafKey.PublicKey, signerKey)
	if err != nil {
		return nil, nil, m, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if opts.IssuerCert != nil {
		certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{
			Type:  "CERTIFICATE",
			Bytes: opts.IssuerCert.Raw,
		})...)
	}
	keyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		return nil, nil, m, err
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	m = Manifest{
		AnchorKind: opts.Kind,
		SmallKey:   m.SmallKey,
		SANHosts:   opts.SANHosts,
		SANIPs:     ipStrings(opts.SANIPs),
		NotBefore:  leaf.NotBefore,
		NotAfter:   leaf.NotAfter,
		CreatedAt:  now(),
	}
	if opts.IssuerCert != nil {
		m.IssuerSubject = opts.IssuerCert.Subject.CommonName
	}
	return certPEM, keyPEM, m, nil
}

// validateOpts checks the anchor and SAN options before minting.
func validateOpts(opts Options) error {
	if opts.Kind == AnchorSelfSigned && opts.IssuerCert != nil {
		return errors.New("selfsigned anchor must not set IssuerCert")
	}
	if opts.Kind != AnchorSelfSigned && (opts.IssuerCert == nil || opts.IssuerKey == nil) {
		return fmt.Errorf("anchor %q requires IssuerCert and IssuerKey", opts.Kind)
	}
	if len(opts.SANHosts) == 0 && len(opts.SANIPs) == 0 {
		return errors.New("at least one SAN host or IP is required")
	}
	return nil
}

func addrs(in []netip.Addr) []net.IP {
	out := make([]net.IP, 0, len(in))
	for _, a := range in {
		out = append(out, a.AsSlice())
	}
	return out
}

func ipStrings(in []netip.Addr) []string {
	out := make([]string, 0, len(in))
	for _, a := range in {
		out = append(out, a.String())
	}
	return out
}
