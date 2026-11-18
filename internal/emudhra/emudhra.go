// Package emudhra identifies eMudhra intermediate CAs whose private keys were
// disclosed in October 2026 to be trivially small P-384 scalars.
package emudhra

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"math/big"
)

// SmallKeys are the reported private-key values of the eight compromised
// eMudhra intermediate CAs (spec §5, T2).
var SmallKeys = []int{4, 56, 66, 115, 155, 156, 230, 237}

// p384 is the curve used by every eMudhra intermediate CA.
var p384 = elliptic.P384()

// p384ScalarBytes is the byte length of a fixed-width P-384 scalar.
const p384ScalarBytes = 48

// SmallKeyFor reports whether cert's P-384 public key equals k*G for any
// candidate k, returning k if so. Non-P-384 or non-ECDSA certs return (0, false).
func SmallKeyFor(cert *x509.Certificate, candidates []int) (int, bool) {
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || pub.Curve != p384 {
		return 0, false
	}
	for _, k := range candidates {
		point, ok := smallKeyPoint(k)
		if !ok {
			continue // out-of-domain scalar: cannot be a small key
		}
		if pub.Equal(point) {
			return k, true
		}
	}
	return 0, false
}

// KeyFromSmallK reconstructs the P-384 private key holding scalar k.
func KeyFromSmallK(k int) *ecdsa.PrivateKey {
	priv, err := ecdsa.ParseRawPrivateKey(p384, scalarBytes(k))
	if err != nil {
		// Unreachable: the SmallKeys set holds only valid P-384 scalars.
		panic(err)
	}
	return priv
}

// smallKeyPoint returns the public key k*G on P-384. Out-of-domain scalars
// (k <= 0 or k >= the curve order) return ok=false; they cannot be small keys.
func smallKeyPoint(k int) (*ecdsa.PublicKey, bool) {
	priv, err := ecdsa.ParseRawPrivateKey(p384, scalarBytes(k))
	if err != nil {
		return nil, false
	}
	return &priv.PublicKey, true
}

// scalarBytes encodes k as a fixed-length big-endian P-384 scalar.
func scalarBytes(k int) []byte {
	b := make([]byte, p384ScalarBytes)
	big.NewInt(int64(k)).FillBytes(b)
	return b
}
