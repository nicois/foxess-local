package framesvc

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"foxesslocal/internal/frame"
)

var routeT0 = time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)

// reqFrame / respFrame build a request (dev0 0x11 / 0x12) and the
// inverter's answer (dev0 | 0x80, same tail) — the 11-05 and 11-15
// captures: every answer echoes its request's tail.
func reqFrame(tail [3]byte, pdu []byte) frame.Frame {
	return frame.Frame{Device: [4]byte{0x11, tail[0], tail[1], tail[2]}, Payload: pdu}
}

func respFrame(tail [3]byte, pdu []byte) frame.Frame {
	return frame.Frame{Device: [4]byte{0x91, tail[0], tail[1], tail[2]}, Payload: pdu}
}

func readPDU(fn byte, addr, n int) []byte {
	//nolint:gosec // G115: test values fit a byte
	return []byte{0x01, fn, byte(addr >> 8), byte(addr), byte(n >> 8), byte(n)}
}

func readResp(fn byte, n int) []byte {
	//nolint:gosec // G115: test values fit a byte
	return append([]byte{0x01, fn, byte(2 * n)}, make([]byte, 2*n)...)
}

// TestRouteByTail: answers go to whoever asked — matched by the echoed
// device tail, oldest first.
func TestRouteByTail(t *testing.T) {
	var tr requests
	cloudTail, ourTail := [3]byte{0xc2, 0xbd, 0x0b}, [3]byte{0x6a, 0xf9, 0x65}
	tr.add(reqFrame(cloudTail, readPDU(0x03, 48000, 10)), askerCloud, routeT0)
	tr.add(reqFrame(ourTail, readPDU(0x03, 31000, 10)), askerLocal, routeT0)

	// the inverter answers ours first: the tail decides, not the order
	require.Equal(t, askerLocal, tr.route(respFrame(ourTail, readResp(0x03, 10)), routeT0))
	require.Equal(t, askerCloud, tr.route(respFrame(cloudTail, readResp(0x03, 10)), routeT0))
	require.Equal(t, askerNone, tr.route(respFrame(cloudTail, readResp(0x03, 10)), routeT0), "answered once only")
}

// TestRouteByShapeFallback: if the inverter did not echo the tail, the
// oldest request whose answer has this shape takes it (the behaviour
// before tails were tracked); a different shape never matches.
func TestRouteByShapeFallback(t *testing.T) {
	var tr requests
	tr.add(reqFrame([3]byte{1, 2, 3}, readPDU(0x03, 48000, 10)), askerCloud, routeT0)
	tr.add(reqFrame([3]byte{4, 5, 6}, readPDU(0x04, 31000, 29)), askerLocal, routeT0)
	require.Equal(t, askerLocal, tr.route(respFrame([3]byte{9, 9, 9}, readResp(0x04, 29)), routeT0))
	require.Equal(t, askerNone, tr.route(respFrame([3]byte{9, 9, 9}, readResp(0x03, 11)), routeT0), "no request of this shape")
	require.Equal(t, askerCloud, tr.route(respFrame([3]byte{9, 9, 9}, readResp(0x03, 10)), routeT0))
}

// TestRouteWriteEcho: a write is answered by its six-byte echo (fn06:
// the request PDU; fn10: unit, fn, address, count).
func TestRouteWriteEcho(t *testing.T) {
	var tr requests
	tail := [3]byte{0x1a, 0x4f, 0x2e}
	fn10 := []byte{0x01, 0x10, 0xbb, 0x8a, 0x00, 0x02, 0x04, 0, 1, 0, 2}
	tr.add(frame.Frame{Device: [4]byte{0x12, tail[0], tail[1], tail[2]}, Payload: fn10}, askerLocal, routeT0)
	echo := frame.Frame{Device: [4]byte{0x92, tail[0], tail[1], tail[2]}, Payload: fn10[:6]}
	require.Equal(t, askerLocal, tr.route(echo, routeT0))
}

// TestRouteExpiry: an unanswered request is forgotten after
// requestTimeout, so a late or unsolicited frame cannot match it.
func TestRouteExpiry(t *testing.T) {
	var tr requests
	tail := [3]byte{1, 2, 3}
	tr.add(reqFrame(tail, readPDU(0x03, 48000, 10)), askerCloud, routeT0)
	require.Equal(t, askerNone, tr.route(respFrame(tail, readResp(0x03, 10)), routeT0.Add(requestTimeout+time.Second)))
}

// TestRouteUnsolicited: with nothing outstanding an answer-shaped frame
// is unsolicited (it goes to the cloud as before, and acks nothing).
func TestRouteUnsolicited(t *testing.T) {
	var tr requests
	require.Equal(t, askerNone, tr.route(respFrame([3]byte{1, 2, 3}, readResp(0x04, 28)), routeT0))
}
