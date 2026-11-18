package framesvc

import (
	"testing"

	"github.com/stretchr/testify/require"

	"foxesslocal/internal/frame"
)

// reasm: a stream of two frames fed in 7 B pieces decodes in order with
// the right stream offsets and leaves no residue.
func TestReasmSplitsFramesInOrder(t *testing.T) {
	f1 := mkFrame(t, startC2S, devInv, fnData, []byte{1, 2, 3, 4, 5})
	f2 := mkFrame(t, startC2S, devInv, fnData, []byte{9, 9, 9, 9, 9, 9, 9, 9})
	stream := append(append([]byte{}, f1...), f2...)

	r := newReasm()
	var got []frame.Frame
	for len(stream) > 0 {
		n := min(7, len(stream))
		got = append(got, r.Feed(stream[:n])...)
		stream = stream[n:]
	}
	require.Len(t, got, 2)
	require.Equal(t, f1, got[0].Raw)
	require.Equal(t, f2, got[1].Raw)
	require.Equal(t, int64(0), got[0].Off)
	require.Equal(t, int64(18), got[1].Off)
	res, off := r.Residue()
	require.Empty(t, res)
	require.Equal(t, int64(39), off)
}

// reasm: a retained tail (here a complete frame with a bad CRC) does not
// wedge the reassembler — Split resynchronizes from the next start
// marker on the next chunk, and the offsets stay stream-absolute.
func TestReasmRetainsTailAndResynchronizes(t *testing.T) {
	f1 := mkFrame(t, startC2S, devInv, fnData, []byte{1, 2, 3, 4, 5})
	bad := mkFrame(t, startC2S, devInv, fnData, []byte{0x42, 0x42})
	bad[10] ^= 0xff // corrupt the CRC
	f2 := mkFrame(t, startC2S, devInv, fnData, []byte{7, 7})

	r := newReasm()
	stream := append(append([]byte{}, f1...), bad...)
	frs := r.Feed(stream)
	require.Len(t, frs, 1)
	require.Equal(t, f1, frs[0].Raw)
	res, off := r.Residue()
	require.Equal(t, bad, res, "the bad-CRC frame is retained as the tail")
	require.Equal(t, int64(18), off)

	frs2 := r.Feed(f2)
	require.Len(t, frs2, 1)
	require.Equal(t, f2, frs2[0].Raw)
	require.Equal(t, int64(33), frs2[0].Off, "offsets are stream-absolute")
	res2, off2 := r.Residue()
	require.Empty(t, res2)
	require.Equal(t, int64(48), off2)
}

// TestReasmSettled: bytes before the first possible start marker cannot
// belong to a frame still to complete; from the marker on they might.
func TestReasmSettled(t *testing.T) {
	for name, tc := range map[string]struct {
		chunk []byte
		want  int64
	}{
		"junk only":            {[]byte{1, 2, 3}, 3},
		"junk then marker":     {[]byte{1, 2, 0x7f, 0x7f, 0, 0}, 2},
		"lone marker at end":   {[]byte{1, 2, 0x7e}, 2},
		"lone marker mid-junk": {[]byte{1, 0x7e, 2}, 3},
		"empty":                {nil, 0},
	} {
		r := newReasm()
		r.Feed(tc.chunk)
		if got := r.Settled(); got != tc.want {
			t.Errorf("%s: Settled %d, want %d", name, got, tc.want)
		}
	}
}
