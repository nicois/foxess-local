package emudhra

import (
	"crypto/x509"
	_ "embed"
	"encoding/pem"
	"fmt"
)

// g3cPem is the public certificate of the eMudhra "OV TLS CA - G3C"
// intermediate (the T2 anchor). Public material only: the
// intermediate's private key is the small scalar k (SmallKeyFor),
// reconstructed on demand with KeyFromSmallK — never embedded, never
// staged at install time. The derived key PEM is cached by
// caMaterial next to the manifest at first start; it is
// reconstructable from the manifest's recorded k alone. Embedding the
// public cert is what lets the app self-bootstrap its leaf on first
// start with zero operator cert files.
//
//go:embed em-ov-tls-ca-g3c.pem
var g3cPem []byte

// G3CCert parses the embedded T2 anchor certificate.
func G3CCert() (*x509.Certificate, error) {
	block, _ := pem.Decode(g3cPem) // no error return; nil block = not PEM
	if block == nil {
		return nil, fmt.Errorf("emudhra: embedded G3C pem: no PEM block")
	}
	return x509.ParseCertificate(block.Bytes)
}
