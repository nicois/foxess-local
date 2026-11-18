package proxconformance

import (
	"io"
	"net"
	"testing"
	"testing/synctest"

	"github.com/nicois/prox/pkg/chanpipe"
	"github.com/nicois/prox/pkg/hooks"

	"github.com/stretchr/testify/require"
)

// TestR9_InMemoryTransportSynctest is the named proof of R9: pkg/chanpipe
// is importable and synctest-safe — a full exchange through the real relay
// runs inside a synctest bubble. It is green by construction: it is the
// harness every other check in this package uses.
func TestR9_InMemoryTransportSynctest(t *testing.T) {
	const sent = "R9-out"
	const received = "R9-in"

	synctest.Test(t, func(t *testing.T) {
		rec := newRecorder()
		l := newLadder(t, nil, rec)
		upClientEnd, upServerEnd := chanpipe.New()
		l.Dial = func(net.Addr) (net.Conn, error) { return upClientEnd, nil }
		f := startFlowOn(t, l, upServerEnd)

		go func() {
			buf := make([]byte, len(sent))
			if _, err := io.ReadFull(upServerEnd, buf); err != nil {
				t.Errorf("upstream read: %v", err)
				return
			}
			if _, err := upServerEnd.Write([]byte(received)); err != nil {
				t.Errorf("upstream write: %v", err)
			}
		}()
		// The relay peeks the client leg before it opens: the first
		// client byte must land before waitOpens.
		_, err := f.client.Write([]byte(sent))
		require.NoError(t, err)
		rec.waitOpens(t, 1)
		buf := make([]byte, len(received))
		_, err = io.ReadFull(f.client, buf)
		require.NoError(t, err)
		require.Equal(t, received, string(buf))

		_ = f.client.Close()
		_ = f.upstream.Close()
		require.NoError(t, <-f.done)
		rec.waitCloses(t, 1)

		id := rec.soleID(t)
		require.Equal(t, sent, string(rec.concat(t, id, hooks.DirC2S)))
		require.Equal(t, received, string(rec.concat(t, id, hooks.DirS2C)))
	})
}
