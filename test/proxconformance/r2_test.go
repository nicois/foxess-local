package proxconformance

import (
	"bytes"
	"io"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nicois/prox/pkg/chanpipe"
	"github.com/nicois/prox/pkg/hooks"

	"github.com/stretchr/testify/require"
)

// TestR2a_Transform: a hook replaces a c2s chunk; the upstream receives the
// replacement, not the original bytes.
func TestR2a_Transform(t *testing.T) {
	const token = "TOKEN"
	const replacement = "***"
	const sent = "alpha-TOKEN-omega"
	const wanted = "alpha-***-omega"

	upGot := make(chan string, 1)
	p := newPolicy("transformer", func(_ *hooks.Conn, dir hooks.Dir, data []byte) hooks.Result {
		if dir == hooks.DirC2S && bytes.Contains(data, []byte(token)) {
			return hooks.Result{Data: bytes.ReplaceAll(data, []byte(token), []byte(replacement))}
		}
		return hooks.Result{}
	})
	f := startRawFlow(t, nil, p)

	go func() {
		buf := make([]byte, len(sent))
		if n, err := f.upstream.Read(buf); err == nil {
			upGot <- string(buf[:n])
		}
	}()
	_, err := f.client.Write([]byte(sent))
	require.NoError(t, err)
	require.Equal(t, wanted, <-upGot)
}

// TestR2b_Drop: Result{Data: []byte{}} removes the chunk; the peer never
// sees it. The client sends a first byte (the inverter always uplinks
// first; the ladder peeks the client leg before dialing the upstream).
func TestR2b_Drop(t *testing.T) {
	const kick = "c2s-kick"
	const keep1 = "keep-1"
	const dropped = "DROP-ME"
	const keep2 = "keep-2"

	p := newPolicy("dropper", func(_ *hooks.Conn, dir hooks.Dir, data []byte) hooks.Result {
		if dir == hooks.DirS2C && bytes.Contains(data, []byte(dropped)) {
			return hooks.Result{Data: []byte{}, Reason: "policy-test"}
		}
		return hooks.Result{}
	})
	f := startRawFlow(t, nil, p)

	go func() {
		buf := make([]byte, len(kick))
		if _, err := io.ReadFull(f.upstream, buf); err != nil {
			t.Errorf("upstream read: %v", err)
			return
		}
		for _, msg := range []string{keep1, dropped, keep2} {
			if _, err := f.upstream.Write([]byte(msg)); err != nil {
				t.Errorf("upstream write: %v", err)
			}
		}
	}()
	_, err := f.client.Write([]byte(kick))
	require.NoError(t, err)

	buf := make([]byte, len(keep1)+len(keep2))
	_, err = io.ReadFull(f.client, buf)
	require.NoError(t, err)
	require.Equal(t, keep1+keep2, string(buf))
}

// TestR2c_Inject: HookConn.WriteS2C at an arbitrary moment — after Ready(),
// after a byte was already received, and not triggered by any received
// byte — lands on the client leg.
func TestR2c_InjectAtArbitraryMoment(t *testing.T) {
	const kick = "c2s-kick"
	const greeting = "hello"
	const injected = "INJECTED-9"

	inj := newInjector("bootstrapper", hooks.DirS2C, []byte(injected))
	f := startRawFlow(t, nil, inj)

	go func() {
		buf := make([]byte, len(kick))
		if _, err := io.ReadFull(f.upstream, buf); err != nil {
			t.Errorf("upstream read: %v", err)
			return
		}
		if _, err := f.upstream.Write([]byte(greeting)); err != nil {
			t.Errorf("upstream write: %v", err)
		}
	}()
	_, err := f.client.Write([]byte(kick))
	require.NoError(t, err)

	buf := make([]byte, len(greeting))
	_, err = io.ReadFull(f.client, buf)
	require.NoError(t, err)
	require.Equal(t, greeting, string(buf))

	inj.fire()
	buf = make([]byte, len(injected))
	_, err = io.ReadFull(f.client, buf)
	require.NoError(t, err)
	require.Equal(t, injected, string(buf))

	_ = f.client.Close()
	_ = f.upstream.Close()
	if err := <-f.done; err != nil {
		t.Logf("relay ended: %v (expected: both ends were just closed)", err)
	}
	<-inj.exited
}

// starvationWindow is how long the s2c direction is held (R2d).
const starvationWindow = 300 * time.Millisecond

// pingCadence is the client's c2s cadence during the starvation (R2d).
const pingCadence = 50 * time.Millisecond

// lateSleep is the upstream's extra wait before its passing chunk (R2d):
// the three held chunks land at t=0/50/100 ms, so LATE lands at t=350 ms,
// past the 300 ms window.
const lateSleep = 250 * time.Millisecond

// TestR2d_Starvation: a hook drops ALL s2c chunks for N virtual seconds
// (synctest) while the client keeps sending c2s — the connection stays
// open and c2s keeps flowing; the first chunk after the window lands.
func TestR2d_Starvation(t *testing.T) {
	const ping = "PING"
	const pingCount = 8
	const late = "LATE"

	synctest.Test(t, func(t *testing.T) {
		// The window is anchored to the flow's open (the hook records it):
		// the first three s2c chunks (t=50/100/150 ms) are dropped, the
		// one at t=400 ms lands. The virtual clock only advances on
		// sleeps, so all t=0 work happens at the same virtual instant.
		var start time.Time
		p := newPolicy("starver", starveDecider(&start))
		p.onOpen = func(now time.Time) { start = now }
		rec := newRecorder()
		l := newLadder(t, nil, p, rec)
		upClientEnd, upServerEnd := chanpipe.New()
		l.Dial = func(net.Addr) (net.Conn, error) { return upClientEnd, nil }
		f := startFlowOn(t, l, upServerEnd)

		// Cloud side, writer: three chunks inside the window (all
		// dropped), one after it (lands).
		starveUpstreamWriter(t, f.upstream, late)

		// Cloud side, reader: drains the pings off the upstream leg so
		// the relay's c2s copy never blocks, and counts what arrived.
		pings := make(chan int, 1)
		starveUpstreamDrainer(t, f.upstream, len(ping), pings)

		// Inverter side: pings immediately (the relay peeks the client
		// leg first) and keeps sending c2s through the whole window.
		starveClientPinger(t, f.client, ping, pingCount)

		// The client sees only the post-window chunk — and only after the
		// window, so the connection stayed up the whole time.
		buf := make([]byte, len(late))
		_, err := io.ReadFull(f.client, buf)
		require.NoError(t, err)
		require.Equal(t, late, string(buf))
		require.GreaterOrEqual(t, time.Since(start), starvationWindow)

		// End the flow: client first (clean c2s EOF), then the upstream.
		_ = f.client.Close()
		_ = f.upstream.Close()
		require.NoError(t, <-f.done)
		reasons := rec.waitCloses(t, 1)
		require.Empty(t, reasons[0])

		// c2s kept flowing: all pings reached the relay (recorder) and
		// the upstream leg (drainer) during the starvation.
		id := rec.soleID(t)
		require.Equal(t, pingCount, rec.chunkCount(t, id, hooks.DirC2S))
		require.Len(t, rec.concat(t, id, hooks.DirC2S), pingCount*len(ping))
		require.Equal(t, pingCount, <-pings)
	})
}

// starveDecider is the R2d decider: drop s2c within starvationWindow of
// the flow's open instant (recorded in start by the hook's OnOpen). A
// zero start (flow not open yet) never drops.
func starveDecider(start *time.Time) func(*hooks.Conn, hooks.Dir, []byte) hooks.Result {
	return func(_ *hooks.Conn, dir hooks.Dir, _ []byte) hooks.Result {
		if dir == hooks.DirS2C && time.Since(*start) < starvationWindow {
			return hooks.Result{Data: []byte{}, Reason: "starvation-test"}
		}
		return hooks.Result{}
	}
}

// starveUpstreamWriter writes the three in-window s2c chunks (dropped)
// and the post-window one (R2d).
func starveUpstreamWriter(t *testing.T, c net.Conn, late string) {
	t.Helper()
	go func() {
		time.Sleep(pingCadence)
		for range 3 {
			if _, err := c.Write([]byte("s2c-held")); err != nil {
				t.Errorf("upstream write: %v", err)
				return
			}
			time.Sleep(pingCadence)
		}
		time.Sleep(lateSleep)
		if _, err := c.Write([]byte(late)); err != nil {
			t.Errorf("upstream write: %v", err)
		}
	}()
}

// starveUpstreamDrainer drains the pings off the upstream leg (so the
// relay's c2s copy never blocks) and reports the count (R2d).
func starveUpstreamDrainer(t *testing.T, c net.Conn, size int, out chan<- int) {
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
		}
	}()
}

// starveClientPinger sends the c2s pings on the client leg (R2d).
func starveClientPinger(t *testing.T, c net.Conn, ping string, count int) {
	t.Helper()
	go func() {
		for range count {
			if _, err := c.Write([]byte(ping)); err != nil {
				t.Errorf("c2s write: %v", err)
				return
			}
			time.Sleep(pingCadence)
		}
	}()
}
