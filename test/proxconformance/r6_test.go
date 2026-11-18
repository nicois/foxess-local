package proxconformance

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"testing"

	"github.com/nicois/prox/pkg/capture"
	"github.com/nicois/prox/pkg/hooks"

	"github.com/stretchr/testify/require"
)

// TestR6_CapturePostDecisionView: for a TLS flow with a scripted drop
// (reason "policy-test") and an injection, the capture Stream records the
// dropped chunk with Decision==Dropped + the reason, the injected chunk
// with Decision==Injected + the owning hook's origin, the forwarded
// chunks, and a complete CloseInfo after Finish.
func TestR6_CapturePostDecisionView(t *testing.T) {
	const c2sPayload = "R6-c2s"
	const greeting = "hello\n"
	const dropped = "DROP-ME-CHUNK"
	const world = "world\n"
	const injected = "INJ-BYTES"

	reg := capture.NewRegistry()
	var stream *capture.Stream
	reg.OnConnection(func(s *capture.Stream) { stream = s })

	dropper := newPolicy("policy", func(_ *hooks.Conn, dir hooks.Dir, data []byte) hooks.Result {
		if dir == hooks.DirS2C && bytes.Contains(data, []byte(dropped)) {
			return hooks.Result{Data: []byte{}, Reason: "policy-test"}
		}
		return hooks.Result{}
	})
	inj := newInjector("r6-injector", hooks.DirS2C, []byte(injected))
	h := newTLSHarness(t, reg, testSNI, dropper, inj)

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- h.ladder.Serve(&addrShim{Conn: h.serverEnd, remote: clientAddr()}, dstAddr())
	}()
	// The upstream must hold the leg open while the injection is in
	// flight: when the upstream stops sending, the relay closes the
	// client leg (TLS cannot half-close) and hook writes stop.
	release := make(chan struct{})
	upErr := make(chan error, 1)
	go func() { upErr <- driveUpstreamR6(t, h, c2sPayload, greeting, dropped, world, release) }()

	if err := driveClientR6(t, h, c2sPayload, greeting, world, injected, inj); err != nil {
		t.Fatalf("client: %v", err)
	}
	close(release)
	require.NoError(t, <-upErr)
	require.NoError(t, <-serveErr)
	<-inj.exited

	require.NotNil(t, stream, "OnConnection must deliver the record at open")
	<-stream.Wait()

	assertCaptureDecisions(t, stream, dropped, injected)
	assertCloseInfo(t, stream)
}

// driveUpstreamR6 is the scripted cloud side: it reads the c2s payload and
// sends greeting, the drop-target chunk, and world — then holds the leg
// open (the relay closes the client leg when the upstream stops sending,
// which would cut off the injection) until release is closed.
func driveUpstreamR6(t *testing.T, h *tlsHarness, c2s, greeting, dropped, world string,
	release <-chan struct{},
) error {
	t.Helper()
	up := tls.Server(h.upServerEnd, &tls.Config{Certificates: []tls.Certificate{h.upLeaf}})
	defer func() { _ = up.Close() }()
	if err := up.Handshake(); err != nil {
		return err
	}
	buf := make([]byte, len(c2s))
	if _, err := io.ReadFull(up, buf); err != nil {
		return err
	}
	if string(buf) != c2s {
		return fmt.Errorf("upstream got %q, want %q", buf, c2s)
	}
	for _, msg := range []string{greeting, dropped, world} {
		if _, err := up.Write([]byte(msg)); err != nil {
			return err
		}
	}
	<-release
	return nil
}

// driveClientR6 is the inverter side: it sends the c2s payload, reads the
// greeting, fires the injection at that (arbitrary) moment, and verifies
// it receives world + the injected bytes but never the dropped chunk.
func driveClientR6(t *testing.T, h *tlsHarness, c2s, greeting, world, injected string,
	inj *injector,
) error {
	t.Helper()
	cli := tls.Client(h.clientEnd, h.client)
	defer func() { _ = cli.Close() }()
	if err := cli.Handshake(); err != nil {
		return err
	}
	if _, err := cli.Write([]byte(c2s)); err != nil {
		return err
	}
	buf := make([]byte, len(greeting))
	if _, err := io.ReadFull(cli, buf); err != nil {
		return err
	}
	if string(buf) != greeting {
		return fmt.Errorf("client got %q, want %q", buf, greeting)
	}
	inj.fire()
	rest := make([]byte, len(world)+len(injected))
	if _, err := io.ReadFull(cli, rest); err != nil {
		return err
	}
	// Hook writes are serialized with the relay's writes to the same
	// direction, but their order relative to in-flight relay bytes is not
	// contractually fixed — both orders are correct.
	if string(rest) != world+injected && string(rest) != injected+world {
		return fmt.Errorf("client got %q, want %q or %q", rest,
			world+injected, injected+world)
	}
	return nil
}

// assertCaptureDecisions checks the post-decision chunk view: one dropped
// chunk with the hook's reason, one injected chunk with the hook's origin,
// and exactly the three forwarded chunks (c2s, greeting, world).
func assertCaptureDecisions(t *testing.T, stream *capture.Stream, dropped, injected string) {
	t.Helper()
	droppedRec, injectedRec, forwarded := 0, 0, 0
	for _, ch := range stream.Chunks() {
		switch ch.Decision {
		case capture.Dropped:
			require.Equal(t, hooks.DirS2C, ch.Dir)
			require.Equal(t, "policy-test", ch.Reason)
			require.NotZero(t, ch.TS)
			// Len is the chunk as the dropping hook received it —
			// consistent with every other record path (pin 8d68f0e).
			require.Equal(t, len(dropped), ch.Len)
			droppedRec++
		case capture.Injected:
			require.Equal(t, hooks.DirS2C, ch.Dir)
			require.Equal(t, "r6-injector", ch.Origin)
			require.Equal(t, len(injected), ch.Len)
			injectedRec++
		case capture.Forwarded:
			forwarded++
		case capture.Held:
			// Phase 1 emits no Held records (the verdict is reserved);
			// a hold would have to be recorded, never swallowed.
			t.Fatalf("unexpected Held record in Phase-1 pin")
		}
	}
	require.Equal(t, 1, droppedRec, "the drop must be recorded with its reason")
	require.Equal(t, 1, injectedRec, "the injection must be recorded with its origin")
	require.Equal(t, 3, forwarded, "c2s + greeting + world forwarded")
}

// assertCloseInfo checks the connection metadata that closes the record.
func assertCloseInfo(t *testing.T, stream *capture.Stream) {
	t.Helper()
	info := stream.Info()
	require.NotEmpty(t, info.ID)
	require.Equal(t, "tls", info.Kind)
	require.Equal(t, testSNI, info.SNI)
	require.Equal(t, net.JoinHostPort(clientIP, "44723"), info.Client)
	require.Equal(t, net.JoinHostPort(dstIP, "14431"), info.Dst)
	require.False(t, info.Opened.IsZero())
	require.False(t, info.Closed.IsZero())
	require.True(t, info.Closed.After(info.Opened))
	require.Empty(t, info.Cause, "clean end")
	require.NotNil(t, info.TLS)
	require.Equal(t, "success", info.TLS.Result)
	require.Equal(t, testSNI, info.TLS.Host)
}
