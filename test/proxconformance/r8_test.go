package proxconformance

import (
	"io"
	"net"
	"os"
	"strconv"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nicois/prox/pkg/chanpipe"
	"github.com/nicois/prox/pkg/proxy"

	"github.com/stretchr/testify/require"
)

// TestR8_EmbeddedModeNoFiles: embedded mode (DataRoot == "") leaves zero
// filesystem side effects — no dataroot, no index.db, no data doc — after
// a full flow. The ladder is wired exactly like the facade wires it for an
// empty DataRoot (no-op sink, no store); the flow runs from a temp working
// directory so any stray write lands somewhere we can see.
func TestR8_EmbeddedModeNoFiles(t *testing.T) {
	const payload = "R8-flow"

	dir := t.TempDir()
	t.Chdir(dir)

	f := startRawFlow(t, nil)
	go func() {
		buf := make([]byte, len(payload))
		if _, err := io.ReadFull(f.upstream, buf); err == nil {
			_, _ = f.upstream.Write(buf)
		}
	}()
	_, err := f.client.Write([]byte(payload))
	require.NoError(t, err)
	buf := make([]byte, len(payload))
	_, err = io.ReadFull(f.client, buf)
	require.NoError(t, err)
	require.Equal(t, payload, string(buf))
	_ = f.client.Close()
	_ = f.upstream.Close()
	require.NoError(t, <-f.done)

	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	require.Empty(t, entries, "embedded mode must write zero files from the working dir")
}

// drainBuf sizes the drainers' read buffer (R8).
const drainBuf = 16

// settleBudget is the virtual time the drain gets to settle (R8).
const settleBudget = 500 * time.Millisecond

// TestR8_DrainNoGoroutineLeak: in-flight flows are drained — the pinned
// version's drain path at its test seam (ladder level: closing the legs;
// the facade's Run/Shutdown needs CAP_NET_ADMIN for the TPROXY listener
// and nftables and is not drivable unprivileged). Every flow gets its
// OnClose, and the process returns to its pre-test goroutine baseline.
func TestR8_DrainNoGoroutineLeak(t *testing.T) {
	const flows = 3

	synctest.Test(t, func(t *testing.T) {
		rec := newRecorder()
		l := newLadder(t, nil, rec)
		dials := make(chan net.Conn)
		l.Dial = func(net.Addr) (net.Conn, error) { return <-dials, nil }

		legs := make([]*drainLeg, 0, flows)
		for i := range flows {
			legs = append(legs, openDrainFlow(t, l, i, dials, rec))
		}

		// Every in-flight c2s byte must be drained (and the relay's c2s
		// copy parked on the client read, not on a write) before any
		// leg closes.
		for _, lg := range legs {
			<-lg.drained
		}
		// Drain: close every leg of every in-flight flow — clients
		// first (clean c2s EOF), then the upstreams; each flow's Serve
		// is done once both its legs are closed.
		for _, lg := range legs {
			_ = lg.client.Close()
		}
		for i, lg := range legs {
			_ = lg.upstream.Close()
			require.NoError(t, <-lg.done, "flow %d", i)
		}
		reasons := rec.waitCloses(t, flows)
		for _, r := range reasons {
			require.Empty(t, r, "drained flows close cleanly")
		}

		// No goroutine may outlive the drain: synctest.Test fails if any
		// goroutine of the bubble is still blocked when this function
		// returns (runtime.NumGoroutine counts the whole process, so
		// goroutines outside the bubble made a count-based check flaky
		// under -race).
		time.Sleep(settleBudget)
		synctest.Wait()
	})
}

// drainLeg is one in-flight flow under the R8 drain check.
type drainLeg struct {
	client   net.Conn
	upstream net.Conn
	done     chan error
	drained  chan struct{}
}

// drainUpstreamLeg reads the upstream leg until it closes, closing
// drained after the first successful read (the test's pre-close barrier:
// the relay's c2s write parks until a reader appears, and a parked write
// would turn the teardown close into a Serve error).
func drainUpstreamLeg(c net.Conn, drained chan<- struct{}) {
	buf := make([]byte, drainBuf)
	first := true
	for {
		n, err := c.Read(buf)
		if first {
			first = false
			if n > 0 || err != nil {
				close(drained)
			}
		}
		if err != nil {
			return
		}
	}
}

// openDrainFlow starts one in-flight flow with a draining upstream leg
// and pushes that leg to the ladder's dial. The relay peeks the client
// leg before it opens: the first client byte must land before waitOpens.
func openDrainFlow(t *testing.T, l *proxy.Ladder, i int, dials chan<- net.Conn, rec *recorder) *drainLeg {
	t.Helper()
	upClientEnd, upServerEnd := chanpipe.New()
	f := startFlowOn(t, l, upServerEnd)
	drained := make(chan struct{})
	go drainUpstreamLeg(f.upstream, drained)
	_, err := f.client.Write([]byte("up" + strconv.Itoa(i)))
	require.NoError(t, err)
	rec.waitOpens(t, i+1)
	dials <- upClientEnd
	return &drainLeg{client: f.client, upstream: f.upstream, done: f.done, drained: drained}
}
