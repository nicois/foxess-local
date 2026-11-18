package frame

import (
	_ "embed"
	"testing"

	"github.com/stretchr/testify/require"
)

//go:embed testdata/wrapper_t2.bin
var wrapperFixture []byte

// Provenance: T2 corpus session 20261104/1793772365132-1, c2s.bin
// bytes [224:675) — the 451 B payload of the c2s frame at off=215
// (mon150: wrapper dev=37007bd4 fn=a4, inner snap02). The file is a
// byte-for-byte corpus extract; refresh it with:
//
//	dd if=/var/lib/prox/conns/20261104/1793772365132-1/c2s.bin \
//	  bs=1 skip=224 count=451 of=internal/frame/testdata/wrapper_t2.bin
//
// Never retype this payload as hex in source — hand-copied hex has
// corrupted this fixture twice (a 444 B literal, then a zero-run).

func TestParseWrapperDecodesCorpusFrame(t *testing.T) {
	p := wrapperFixture
	require.Len(t, p, wrapperLen)

	// The outer frame classifies as the wrapper on plen + dev0 — the
	// envelope fn is not a discriminator (0x64, 0x83, 0xa4 observed).
	for _, fn := range []byte{0x64, 0x83, 0xa4} {
		kind, payload := Decode(Frame{Device: [4]byte{wrapperDev0, 0x00, 0x7b, 0xd4}, Fn: fn, Payload: p})
		require.Equal(t, KindWrapper, kind, "fn 0x%02x", fn)
		require.Equal(t, p, payload)
	}

	w, err := ParseWrapper(p)
	require.NoError(t, err)
	require.Equal(t, [4]byte{0x02, SessionDev1, 0xeb, 0x65}, w.InnerDev)
	require.Equal(t, byte(0xc7), w.InnerFn)
	require.Equal(t, 425, w.InnerPlen)
	require.Equal(t, SnapKind(0x02), w.InnerClass, "the inner frame is a snap02")
}

func TestParseWrapperRejectsBrokenPayloads(t *testing.T) {
	p := wrapperFixture

	_, err := ParseWrapper(p[:wrapperLen-1])
	require.Error(t, err, "short payload")

	crcCorrupt := append([]byte(nil), p...)
	crcCorrupt[wrapperPrefix+9+425] ^= 0xff // the inner CRC's low byte
	_, err = ParseWrapper(crcCorrupt)
	require.Error(t, err, "corrupt inner crc")

	termCorrupt := append([]byte(nil), p...)
	termCorrupt[len(termCorrupt)-2] ^= 0xff // the inner f7f7 terminator
	_, err = ParseWrapper(termCorrupt)
	require.Error(t, err, "bad inner terminator")
}
