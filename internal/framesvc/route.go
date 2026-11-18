package framesvc

import (
	"time"

	"foxesslocal/internal/frame"
)

// Request routing (owner, 2026-11-17): in a mirrored flow the inverter
// answers two askers on one stream — the cloud (its reads in
// telemetry_only; everything in full) and this app (the gateway's
// commands and polls, the trigger reads). Each answer goes back to
// whoever asked: the cloud's to the cloud, ours to the gateway (and never
// to the cloud, which did not ask).
//
// The match: every request reaching the inverter is recorded in order
// with its device tail; the inverter echoes the tail in its answer (11-05
// writes and 11-15 reads, every frame — profile §5) and answers within
// ~50 ms. An answer takes the oldest request with its tail and a matching
// answer shape; failing that, the oldest request of that shape (the
// behaviour before tails were tracked); failing that, it is unsolicited.

// asker is who asked.
type asker int

const (
	askerNone  asker = iota // unsolicited (or nothing outstanding)
	askerCloud              // the real cloud's request, relayed
	askerLocal              // this app's (gateway, poller, trigger reads)
)

// requestTimeout forgets an unanswered request (answers take ~50 ms).
const requestTimeout = 10 * time.Second

// Modbus PDU layout for the answer shapes.
const (
	pduFn       = 1
	pduCountHi  = 4
	pduCountLo  = 5
	pduHead     = 3 // unit, fn, byte count
	echoPDULen  = 6 // a write's echo: unit, fn, address, count / value
	fnRead03    = 0x03
	fnRead04    = 0x04
	fnWrite06   = 0x06
	fnWrite10   = 0x10
	byteShiftHi = 8
)

// pendingRequest is one request awaiting its answer.
type pendingRequest struct {
	tail  [3]byte
	shape func(p []byte) bool // reports whether p is this request's answer
	asker asker
	at    time.Time
}

// requests is a flow's outstanding requests, oldest first (callers hold
// the flow lock).
type requests struct {
	q []pendingRequest
}

func tailOf(f frame.Frame) [3]byte { return [3]byte{f.Device[1], f.Device[2], f.Device[3]} }

// add records a request as it reaches the inverter (a frame that is not
// a recognisable Modbus request is ignored: nothing will answer it).
func (r *requests) add(f frame.Frame, o asker, now time.Time) {
	shape := answerShape(f.Payload)
	if shape == nil {
		return
	}
	r.expire(now)
	r.q = append(r.q, pendingRequest{tail: tailOf(f), shape: shape, asker: o, at: now})
}

// route matches an answer to its request (removing it) and reports who
// asked.
func (r *requests) route(f frame.Frame, now time.Time) asker {
	r.expire(now)
	tail := tailOf(f)
	pick := -1
	for i := range r.q {
		if r.q[i].shape(f.Payload) && r.q[i].tail == tail {
			pick = i
			break
		}
	}
	if pick < 0 {
		for i := range r.q {
			if r.q[i].shape(f.Payload) {
				pick = i
				break
			}
		}
	}
	if pick < 0 {
		return askerNone
	}
	o := r.q[pick].asker
	r.q = append(r.q[:pick], r.q[pick+1:]...)
	return o
}

// expire drops requests older than requestTimeout.
func (r *requests) expire(now time.Time) {
	keep := r.q[:0]
	for _, p := range r.q {
		if now.Sub(p.at) <= requestTimeout {
			keep = append(keep, p)
		}
	}
	r.q = keep
}

// answerShape returns the test for a request's answer: a read of n
// registers is answered by unit, fn, 2n, then 2n data bytes; a write by
// its six-byte echo (fn06: the request PDU itself; fn10: unit, fn,
// address, count). nil: not a request this app tracks.
func answerShape(req []byte) func([]byte) bool {
	if len(req) < echoPDULen {
		return nil
	}
	fn := req[pduFn]
	switch fn {
	case fnRead03, fnRead04:
		n := int(req[pduCountHi])<<byteShiftHi | int(req[pduCountLo])
		return func(p []byte) bool {
			return len(p) == pduHead+2*n && p[0] == req[0] && p[pduFn] == fn && int(p[2]) == 2*n
		}
	case fnWrite06, fnWrite10:
		echo := append([]byte(nil), req[:echoPDULen]...)
		return func(p []byte) bool {
			if len(p) != echoPDULen {
				return false
			}
			for i := range echo {
				if p[i] != echo[i] {
					return false
				}
			}
			return true
		}
	}
	return nil
}
