package proxconformance

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"

	"github.com/nicois/prox/pkg/chanpipe"
	"github.com/nicois/prox/pkg/hooks"

	"github.com/stretchr/testify/require"
)

// echoFlow runs one raw flow to completion: the client sends n chunks of
// size bytes; the scripted cloud echoes each chunk back. It returns an
// error instead of failing so it can run in a goroutine (vet: no
// t.Fatal from non-test goroutines).
func echoFlow(t *testing.T, f *rawFlow, tag byte, n, size int) error {
	t.Helper()
	go func() {
		buf := make([]byte, size)
		for range n {
			if _, err := io.ReadFull(f.upstream, buf); err != nil {
				return
			}
			if _, err := f.upstream.Write(buf); err != nil {
				return
			}
		}
	}()
	// The read side runs in its own goroutine: the relay's s2c copy can
	// buffer only one pending chunk, so writing every c2s chunk before
	// reading would stall the relay.
	readErr := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(f.client, make([]byte, n*size))
		readErr <- err
	}()
	payload := bytes.Repeat([]byte{tag}, size)
	for range n {
		if _, err := f.client.Write(payload); err != nil {
			return fmt.Errorf("c2s write: %w", err)
		}
	}
	if err := <-readErr; err != nil {
		return fmt.Errorf("s2c read: %w", err)
	}
	_ = f.client.Close()
	_ = f.upstream.Close()
	return <-f.done
}

// TestR4_OrderedChainBothSeeEveryChunk: two ordered hooks shared by two
// concurrent flows. Both hooks see every chunk in both directions exactly
// once (per-hook per-flow chunk and byte totals equal what was sent), and
// each shared hook's per-Conn-ID state never crosses flows.
func TestR4_OrderedChainBothSeeEveryChunk(t *testing.T) {
	const chunkCount = 20
	const chunkSize = 16

	first := newPolicy("first", nil)
	second := newPolicy("second", nil)
	rec := newRecorder()
	l := newLadder(t, nil, first, second, rec)

	// The relay dials per flow in open order. The dial channel is
	// unbuffered: a push completes only when that flow's dial takes the
	// conn, so each flow gets exactly its own upstream. echoFlow writes the
	// client leg first (the relay peeks the client before it opens), which
	// also overlaps the two flows.
	dials := make(chan net.Conn)
	l.Dial = func(net.Addr) (net.Conn, error) { return <-dials, nil }
	upClient1, upServer1 := chanpipe.New()
	upClient2, upServer2 := chanpipe.New()

	f1 := startFlowOn(t, l, upServer1)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := echoFlow(t, f1, 1, chunkCount, chunkSize); err != nil {
			t.Errorf("flow 1: %v", err)
		}
	}()
	rec.waitOpens(t, 1)
	dials <- upClient1
	f2 := startFlowOn(t, l, upServer2)
	go func() {
		defer wg.Done()
		if err := echoFlow(t, f2, 2, chunkCount, chunkSize); err != nil {
			t.Errorf("flow 2: %v", err)
		}
	}()
	rec.waitOpens(t, 2)
	dials <- upClient2

	wg.Wait()
	rec.waitCloses(t, 2)

	assertChainSawAll(t, first, rec, chunkCount, chunkSize)
	assertChainSawAll(t, second, rec, chunkCount, chunkSize)
}

// assertChainSawAll asserts the hook saw, on each of the two flows, every
// chunk in both directions exactly once: totals equal what was sent, and
// the per-connection state covers exactly the two flow IDs (no crossing).
func assertChainSawAll(t *testing.T, h *policy, rec *recorder, chunks, size int) {
	t.Helper()
	rec.mu.Lock()
	ids := make([]string, 0, len(rec.conns))
	for id := range rec.conns {
		ids = append(ids, id)
	}
	rec.mu.Unlock()
	require.Len(t, ids, 2)
	for _, id := range ids {
		gotChunks, gotBytes := h.saw(id, hooks.DirC2S)
		require.Equal(t, chunks, gotChunks, "hook %s c2s chunks on %s", h.name, id)
		require.Equal(t, chunks*size, gotBytes, "hook %s c2s bytes on %s", h.name, id)
		gotChunks, gotBytes = h.saw(id, hooks.DirS2C)
		require.Equal(t, chunks, gotChunks, "hook %s s2c chunks on %s", h.name, id)
		require.Equal(t, chunks*size, gotBytes, "hook %s s2c bytes on %s", h.name, id)
	}
}

// TestR4_ShortCircuit: a drop in the first hook terminates the chain for
// that chunk — the second hook never sees it, and the upstream never does.
func TestR4_ShortCircuit(t *testing.T) {
	const chunkCount = 20
	const chunkSize = 16
	const droppedChunks = 3 // chunks with index % 7 == 3: 3, 10, 17

	dropper := newPolicy("dropper", scDropDecider('X'))
	passer := newPolicy("passer", nil)
	rec := newRecorder()
	f := startRawFlow(t, nil, dropper, passer, rec)

	passed := make(chan int, 1)
	scUpstreamDrainer(t, f.upstream, chunkSize, passed)
	// The read side runs in its own goroutine: a single main goroutine
	// that writes every c2s chunk before reading would ring-block the
	// relay's single-slot legs (write loop → c2s copy → drainer → s2c
	// copy → back to the unread client leg).
	keep := chunkCount - droppedChunks
	readErr := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(f.client, make([]byte, keep*chunkSize))
		readErr <- err
	}()
	for i := range chunkCount {
		payload := bytes.Repeat([]byte{byte(i)}, chunkSize)
		if i%7 == 3 {
			payload[0] = 'X'
		}
		if _, err := f.client.Write(payload); err != nil {
			t.Fatalf("c2s write %d: %v", i, err)
		}
	}
	require.NoError(t, <-readErr)
	_ = f.client.Close()
	_ = f.upstream.Close()
	require.NoError(t, <-f.done)
	require.Equal(t, keep, <-passed)

	id := rec.soleID(t)
	gotChunks, gotBytes := dropper.saw(id, hooks.DirC2S)
	require.Equal(t, chunkCount, gotChunks, "dropper must see every chunk")
	require.Equal(t, chunkCount*chunkSize, gotBytes, "dropper must see every chunk")
	gotChunks, gotBytes = passer.saw(id, hooks.DirC2S)
	require.Equal(t, keep, gotChunks, "passer must never see a dropped chunk")
	require.Equal(t, keep*chunkSize, gotBytes, "passer must never see a dropped chunk")
	gotChunks, gotBytes = passer.saw(id, hooks.DirS2C)
	require.Equal(t, keep, gotChunks, "echoed s2c chunks")
	require.Equal(t, keep*chunkSize, gotBytes, "echoed s2c bytes")
}

// scDropDecider drops c2s chunks containing the marker byte (R4
// short-circuit check).
func scDropDecider(marker byte) func(*hooks.Conn, hooks.Dir, []byte) hooks.Result {
	return func(_ *hooks.Conn, dir hooks.Dir, data []byte) hooks.Result {
		if dir == hooks.DirC2S && bytes.Contains(data, []byte{marker}) {
			return hooks.Result{Data: []byte{}, Reason: "sc-test"}
		}
		return hooks.Result{}
	}
}

// scUpstreamDrainer echoes complete upstream chunks back until the leg
// closes and reports how many it echoed (R4 short-circuit check).
func scUpstreamDrainer(t *testing.T, c net.Conn, size int, out chan<- int) {
	t.Helper()
	go func() {
		buf := make([]byte, size)
		n := 0
		for {
			if _, err := io.ReadFull(c, buf); err != nil {
				out <- n
				return
			}
			n++
			if _, err := c.Write(buf); err != nil {
				out <- n
				return
			}
		}
	}()
}
