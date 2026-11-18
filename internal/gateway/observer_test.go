package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// newObsGW builds a gateway with a recording observer (the observer
// fires under the gateway lock; the test reads the slice after Wait
// returns, which happens-after the terminal close(done)).
func newObsGW(t *testing.T, clk *vclock, obs func(Result)) *Gateway {
	t.Helper()
	inj := &spyInjector{}
	gw := New(inj, clk.get, WithObserver(obs))
	return gw
}

// ---------- terminal state exactly once, per path ----------

func TestObserverAckedOnce(t *testing.T) {
	clk := &vclock{now: fixedNow()}
	var got []Result
	gw := newObsGW(t, clk, func(r Result) { got = append(got, r) })
	gw.SetFlow("c1")

	id, err := gw.Submit(context.Background(), Command{Kind: KindWorkMode, Params: map[Param]int{pMode: 1}, Origin: "t"})
	require.NoError(t, err)
	require.NoError(t, ack(gw, id, []byte{0x01, 0x06, 0xa0, 0x28, 0x00, 0x01}))

	require.Len(t, got, 1)
	require.Equal(t, id, got[0].ID)
	require.Equal(t, KindWorkMode, got[0].Kind)
	require.Equal(t, StatusAcked, got[0].Status)

	// No further fire for a repeated Wait (the event is terminal-once,
	// not wait-once).
	_, err = gw.Wait(context.Background(), id)
	require.NoError(t, err)
	require.Len(t, got, 1)
}

func TestObserverFailedOnce(t *testing.T) {
	clk := &vclock{now: fixedNow()}
	var got []Result
	gw := newObsGW(t, clk, func(r Result) { got = append(got, r) })
	gw.SetFlow("c1")

	id, err := gw.Submit(context.Background(), Command{Kind: KindWorkMode, Params: map[Param]int{pMode: 1}, Origin: "t"})
	require.NoError(t, err)
	// A wrong value in the echo fails the command.
	require.Error(t, ack(gw, id, []byte{0x01, 0x06, 0xa0, 0x28, 0x00, 0x00}))

	require.Len(t, got, 1)
	require.Equal(t, id, got[0].ID)
	require.Equal(t, StatusFailed, got[0].Status)
	require.NotEmpty(t, got[0].Detail)
}

func TestObserverExpiredOnce(t *testing.T) {
	clk := &vclock{now: fixedNow()}
	var got []Result
	gw := newObsGW(t, clk, func(r Result) { got = append(got, r) })
	// No flow: the command queues and outlives its TTL.
	id, err := gw.Submit(context.Background(), Command{Kind: KindWorkMode, Params: map[Param]int{pMode: 1}, Origin: "t"})
	require.NoError(t, err)
	clk.advance(defaultQueueTTL + time.Second)

	res, err := gw.Wait(context.Background(), id)
	require.NoError(t, err)
	require.Equal(t, StatusExpired, res.Status)

	require.Len(t, got, 1)
	require.Equal(t, id, got[0].ID)
	require.Equal(t, StatusExpired, got[0].Status)
}

// A gateway without an observer behaves exactly as before (nil-safe).
func TestNoObserverNoChange(t *testing.T) {
	clk := &vclock{now: fixedNow()}
	gw, inj := newTestGW(t, clk)
	gw.SetFlow("c1")

	id, err := gw.Submit(context.Background(), Command{Kind: KindWorkMode, Params: map[Param]int{pMode: 1}, Origin: "t"})
	require.NoError(t, err)
	require.NoError(t, ack(gw, id, []byte{0x01, 0x06, 0xa0, 0x28, 0x00, 0x01}))
	require.Len(t, inj.injected(), 1)
}
