package main

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCertgenOptsSANFlags: certgen's SANs come from -ip/-host (no config
// file); the default is the built-in cloud endpoint, lists are
// comma-separated, and a non-IP fails before minting.
func TestCertgenOptsSANFlags(t *testing.T) {
	opts, err := certgenOpts(defaultLeafIP.String(), "", "selfsigned", "", "", 0)
	require.NoError(t, err)
	require.Equal(t, []netip.Addr{defaultLeafIP}, opts.SANIPs)
	require.Empty(t, opts.SANHosts)

	opts, err = certgenOpts("10.0.0.1, 10.0.0.2", "a.example,b.example", "selfsigned", "", "", 0)
	require.NoError(t, err)
	require.Len(t, opts.SANIPs, 2)
	require.Equal(t, []string{"a.example", "b.example"}, opts.SANHosts)

	_, err = certgenOpts("not-an-ip", "", "selfsigned", "", "", 0)
	require.ErrorContains(t, err, `-ip: "not-an-ip"`)
}
