package proxconformance

import (
	"testing"

	"github.com/nicois/prox/pkg/capture"
	"github.com/nicois/prox/pkg/hooks"

	"github.com/stretchr/testify/require"
)

// TestR1_TwoDirectionInterception: a TLS flow carrying distinct payloads in
// both directions — the hook sees every chunk in each direction tagged with
// the right Dir; the Conn carries the four-tuple and the IP-literal SNI;
// the TLSObserver fires exactly once with a result in the documented
// vocabulary ("success" | "client_rejected" | "mint_failed").
func TestR1_TwoDirectionInterception(t *testing.T) {
	const c2sPayload = "R1-c2s-payload"
	const s2cPayload = "R1-s2c-payload"

	rec := newRecorder()
	obs := newTLSObserver()
	h := newTLSHarness(t, capture.NewRegistry(), testSNI, rec, obs)
	_, err := runTLSFlow(t, h, c2sPayload, s2cPayload)
	require.NoError(t, err)

	rec.waitCloses(t, 1)
	id := rec.soleID(t)
	c := rec.conn(t, id)

	// Flow identity: the four-tuple, the SNI, the TLS kind.
	require.Equal(t, clientIP, c.Src)
	require.Equal(t, clientPort, c.SrcPort)
	require.Equal(t, dstIP, c.Dst)
	require.Equal(t, dstPort, c.DPort)
	require.Equal(t, testSNI, c.SNI)
	require.Equal(t, "tls", c.Kind)

	// Both directions: every byte, direction-tagged, in order.
	require.Equal(t, c2sPayload, string(rec.concat(t, id, hooks.DirC2S)))
	require.Equal(t, s2cPayload, string(rec.concat(t, id, hooks.DirS2C)))
	require.GreaterOrEqual(t, rec.chunkCount(t, id, hooks.DirC2S), 1)
	require.GreaterOrEqual(t, rec.chunkCount(t, id, hooks.DirS2C), 1)

	// The observer fired exactly once, with the documented vocabulary.
	require.Len(t, obs.results, 1)
	r := <-obs.results
	require.Equal(t, "success", r.Result)
	require.Equal(t, testSNI, r.Host)
	require.NotZero(t, r.Version)
}
