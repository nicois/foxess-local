package proxconformance

import (
	"crypto/x509"
	"net"
	"testing"
	"time"

	"github.com/nicois/prox/pkg/capture"

	"github.com/stretchr/testify/require"
)

// TestR7a_IPSNIMinting: IP-SNI minting works on the pin, in the two
// levels the test seams allow:
//
//  1. full flow: a client dialing the cloud endpoint by raw IP (no SNI in
//     the ClientHello — Go's stdlib client suppresses the SNI extension
//     for IP-literal ServerNames, crypto/tls hostnameInSNI; the real
//     inverter, an OpenSSL client, does send SNI=IP) completes the MITM:
//     the forged leaf is minted from the original destination and carries
//     the dst IP as an iPAddress SAN (the client verified it — otherwise
//     the handshake would have failed), and the capture record carries
//     TLSInfo with result "success";
//  2. minting seam: the leaf minter given an IP-literal SNI (the inverter's
//     dial shape, including the Aliyun failover IP set) mints a leaf whose
//     SANs include that IP.
//
// sniff's SNI parse is byte-generic (no hostname assumption), so the
// hostname-SNI plumbing is exercised end-to-end in TestR1.
func TestR7a_IPSNIMinting(t *testing.T) {
	const c2sPayload = "R7-c2s"
	const s2cPayload = "R7-s2c"

	reg := capture.NewRegistry()
	var stream *capture.Stream
	reg.OnConnection(func(s *capture.Stream) { stream = s })
	h := newTLSHarness(t, reg, dstIP)
	state, err := runTLSFlow(t, h, c2sPayload, s2cPayload)
	require.NoError(t, err)

	// Level 1: the forged leaf for the IP dial carries the IP SAN.
	require.NotEmpty(t, state.PeerCertificates)
	leaf, err := x509.ParseCertificate(state.PeerCertificates[0].Raw)
	require.NoError(t, err)
	require.Contains(t, leaf.IPAddresses, net.ParseIP(dstIP).To4(),
		"forged leaf must carry the dst IP as an iPAddress SAN")

	require.NotNil(t, stream)
	<-stream.Wait()
	info := stream.Info()
	require.NotNil(t, info.TLS, "the TLS outcome must be attached to the record")
	require.Equal(t, "success", info.TLS.Result)
	require.NotZero(t, info.TLS.Version)

	// Level 2: SNI = IP literal (a distinct failover-set IP, so the mint
	// is fresh, not the cached leaf from level 1) mints an iPAddress SAN.
	cert, err := h.ladder.Leaves.Leaf(failoverIP, failoverIP, time.Now())
	require.NoError(t, err)
	leaf2, err := x509.ParseCertificate(cert.Certificate[0])
	require.NoError(t, err)
	require.Contains(t, leaf2.IPAddresses, net.ParseIP(failoverIP).To4(),
		"IP-literal SNI must mint an iPAddress SAN")
}

// TestR7b_CertSelectionCallback is a tracked-fail: a certificate-selection
// callback that receives the client's offer (SNI included) and selects
// which CA/leaf to mint ships in prox Phase 2 (the leaf selector), which
// is not merged into the pinned version. Today the ladder mints with its
// round-robined CA and no consumer callback.
//
// When the pin moves past the Phase-2 merge, replace the skip with: a
// selector driven by three flows whose SNI is one of three distinct
// failover-set IPs; assert each flow's forged leaf is signed by the CA
// the selector chose for that SNI, and that the selector received the
// client's SNI offer (including the IP-literal form).
func TestR7b_CertSelectionCallback(t *testing.T) {
	t.Skipf("tracked: Phase-2 leaf selector is in pinned version %s; check body pending (documented in test comment)", pinnedShort)
}
