package proxconformance

import (
	"encoding/binary"
	"io"
	"testing"

	"github.com/nicois/prox/pkg/capture"
	"github.com/nicois/prox/pkg/hooks"

	"github.com/stretchr/testify/require"
)

// seqChunk is one sequence-numbered payload: 2-byte big-endian sequence
// plus seqPayload bytes.
const seqPayload = 10

func seqChunk(i int) []byte {
	b := make([]byte, 2+seqPayload)
	binary.BigEndian.PutUint16(b,
		uint16(i)) //nolint:gosec // G115: test sequence numbers stay below 2^16
	return b
}

// assertSeqOrdering checks that the hook's per-direction chunk view carries
// the chunks in send order, 0..n-1.
func assertSeqOrdering(t *testing.T, rec *recorder, id string, dir hooks.Dir, n int) {
	t.Helper()
	chunkLen := 2 + seqPayload
	require.Equal(t, n, rec.chunkCount(t, id, dir))
	buf := rec.concat(t, id, dir)
	require.Len(t, buf, n*chunkLen)
	for i := range n {
		got := binary.BigEndian.Uint16(buf[i*chunkLen : (i+1)*chunkLen])
		require.Equal(t, uint16(i), got, "%s sequence at position %d", dir, i)
	}
}

// TestR5_OrderPreservedPerDirection: sequence-numbered payloads in both
// directions, flowing concurrently — per-direction byte order is preserved
// exactly.
func TestR5_OrderPreservedPerDirection(t *testing.T) {
	const chunkCount = 64

	rec := newRecorder()
	f := startRawFlow(t, nil, rec)

	go func() {
		if _, err := io.ReadFull(f.upstream, make([]byte, chunkCount*(2+seqPayload))); err != nil {
			t.Errorf("upstream read: %v", err)
			return
		}
		for i := range chunkCount {
			if _, err := f.upstream.Write(seqChunk(i)); err != nil {
				t.Errorf("upstream write %d: %v", i, err)
			}
		}
	}()
	for i := range chunkCount {
		if _, err := f.client.Write(seqChunk(i)); err != nil {
			t.Fatalf("c2s write %d: %v", i, err)
		}
	}
	if _, err := io.ReadFull(f.client, make([]byte, chunkCount*(2+seqPayload))); err != nil {
		t.Fatalf("s2c read: %v", err)
	}
	_ = f.client.Close()
	_ = f.upstream.Close()
	require.NoError(t, <-f.done)

	id := rec.soleID(t)
	assertSeqOrdering(t, rec, id, hooks.DirC2S, chunkCount)
	assertSeqOrdering(t, rec, id, hooks.DirS2C, chunkCount)
}

// TestR5_CaptureBoundedNoSilentLoss: the capture Stream is bounded; fill
// it past maxChunks and the overflow is counted exactly in Evicted() —
// loss is a number, never silent.
func TestR5_CaptureBoundedNoSilentLoss(t *testing.T) {
	const maxChunks = 10
	const chunkCount = 25
	const evicted = chunkCount - maxChunks

	reg := capture.NewRegistry()
	reg.SetMaxChunks(maxChunks)
	var stream *capture.Stream
	reg.OnConnection(func(s *capture.Stream) { stream = s })
	f := startRawFlow(t, reg, newRecorder())

	// The upstream must confirm it received every byte before the legs
	// close, or the final write races the teardown.
	upGot := make(chan int, 1)
	go func() {
		buf := make([]byte, chunkCount)
		n, _ := io.ReadFull(f.upstream, buf)
		upGot <- n
	}()
	for i := range chunkCount {
		if _, err := f.client.Write([]byte{byte(i)}); err != nil {
			t.Fatalf("c2s write %d: %v", i, err)
		}
	}
	require.Equal(t, chunkCount, <-upGot)
	_ = f.client.Close()
	_ = f.upstream.Close()
	require.NoError(t, <-f.done)

	require.NotNil(t, stream, "OnConnection must deliver the record at open")
	<-stream.Wait()
	require.Len(t, stream.Chunks(), maxChunks)
	require.Equal(t, evicted, stream.Evicted())
}
