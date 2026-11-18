package gateway

import (
	"fmt"
	"time"

	"foxesslocal/internal/frame"
)

// exceptionBit marks a Modbus exception response (fn | 0x80, then the
// exception code).
const exceptionBit = 0x80

// rtCommand is the runtime state of one submitted command.
type rtCommand struct {
	id      string
	cmd     Command
	enqAt   time.Time
	startAt time.Time // when it went in flight (the inFlightTTL base)
	status  Status
	detail  string
	reads   []readReq  // pending read-phase requests (FIFO)
	writes  []writeReq // pending write-phase ACKs
	current []RegisterValue
	// unchanged: acked without a write (the inverter already held the
	// values — WithSkipUnchanged)
	unchanged bool
	// done closes exactly once when the command reaches a terminal state
	// (Wait wakes on it; the status is re-read under the lock after wake).
	done chan struct{}
}

// Ack correlates one inverter-originated echo frame with the in-flight
// command (one call per echo frame — a coalesced chunk feeds several,
// in stream order). Shapes (profile §5):
//   - read response 01 03 <bc> <data>: the fn03 response carries no address,
//     so responses are matched to the oldest pending read (the inverter
//     answers sequentially; every capture shows in-order responses).
//     Responses accumulate into the command's current state; when the read
//     phase completes the write phase is built from the accumulated current
//     and injected.
//   - write ACK 01 06 <addr> <val> / 01 10 <addr> <cnt> (6 B, the standard
//     trailing 00 is omitted on KH10): marks its (addr[, cnt/value]) write
//     done.
//
// All writes done (and the read phase, for schedule) → the command is
// acked. A mismatched echo fails the command (reported, not stuck).
func (g *Gateway) Ack(cmdID string, echo frame.Frame) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	rt := g.rt
	if rt == nil || rt.id != cmdID {
		return fmt.Errorf("%w: %s", ErrUnknownCommand, cmdID)
	}
	if err := g.ackEchoLocked(rt, echo); err != nil {
		g.failCommandLocked(rt, err.Error())
		g.flushLocked()
		return err
	}
	if rt.status == StatusAcked {
		g.rt = nil
		g.acked = append(g.acked, rt)
		if len(g.acked) > ackRingCap {
			g.acked = g.acked[len(g.acked)-ackRingCap:]
		}
		g.flushLocked()
	}
	return nil
}

func (g *Gateway) ackEchoLocked(rt *rtCommand, echo frame.Frame) error {
	// inverter-originated frames carry the request device lead | 0x80 (0x91)
	if echo.Device[0]&0x80 == 0 {
		return fmt.Errorf("%w: device lead %#02x is not inverter-originated", ErrEchoMismatch, echo.Device[0])
	}
	p := echo.Payload
	if len(p) == 0 || p[0] != fnUnit {
		return fmt.Errorf("%w: payload %x does not start with unit 0x01", ErrEchoMismatch, p)
	}
	if p[1]&exceptionBit != 0 && len(p) >= 3 {
		return fmt.Errorf("%w %d (fn %#02x)", ErrModbusException, p[2], p[1]&^exceptionBit)
	}
	switch p[1] {
	case fnRead, fnInputRead:
		return g.ackReadLocked(rt, p)
	case fnWriteOne, fnWriteMulti:
		return g.ackWriteAckLocked(rt, p)
	default:
		return fmt.Errorf("%w: fn %#04x is not a Modbus response", ErrEchoMismatch, p[1])
	}
}

// ackWriteAckLocked consumes one write ACK: fn06 matches addr+value, fn10
// matches addr+count.
func (g *Gateway) ackWriteAckLocked(rt *rtCommand, p []byte) error {
	if len(p) != 6 {
		return fmt.Errorf("%w: fn%#04x ack length %d, want 6", ErrEchoMismatch, p[1], len(p))
	}
	fn, addr, field := p[1], int(p[2])<<byteShift|int(p[3]), int(p[4])<<byteShift|int(p[5])
	match := func(w writeReq) bool {
		if w.fn != fn || w.addr != addr {
			return false
		}
		if fn == fnWriteMulti {
			return w.cnt == field
		}
		return w.val == field
	}
	if !removeWrite(rt, match) {
		return fmt.Errorf("%w: fn%#04x ack @%d field %#04x matches no pending write",
			ErrEchoMismatch, fn, addr, field)
	}
	g.finishWritesLocked(rt)
	return nil
}

// ackReadLocked consumes one read response for the oldest pending read.
func (g *Gateway) ackReadLocked(rt *rtCommand, p []byte) error {
	if len(rt.reads) == 0 {
		return fmt.Errorf("%w: read response but the read phase is complete", ErrEchoMismatch)
	}
	req := rt.reads[0]
	wantFn := byte(fnRead)
	if rt.cmd.Kind == KindRead {
		//nolint:gosec // G115: fn validated to 3 or 4.
		wantFn = byte(rt.cmd.Params[ParamFn])
	}
	if p[1] != wantFn {
		return fmt.Errorf("%w: fn%#02x response to a fn%#02x read", ErrEchoMismatch, p[1], wantFn)
	}
	if len(p) < 3 {
		return fmt.Errorf("%w: short read response %x", ErrEchoMismatch, p)
	}
	bc := int(p[2])
	if len(p) != 3+bc || bc%2 != 0 || bc != 2*req.cnt {
		return fmt.Errorf("%w: read response bc=%d does not match pending read @%d x%d",
			ErrEchoMismatch, bc, req.addr, req.cnt)
	}
	rt.reads = rt.reads[1:]
	for i := range req.cnt {
		v := int(p[3+2*i])<<byteShift | int(p[4+2*i])
		//nolint:gosec // G115: register address within the schedule block set.
		rt.current = append(rt.current, RegisterValue{Addr: uint16(req.addr + i), Value: uint16(v)})
	}
	if len(rt.reads) > 0 {
		return nil
	}
	if rt.cmd.Kind == KindRead {
		g.finishWritesLocked(rt) // no write phase
		return nil
	}
	return g.startWritePhaseLocked(rt)
}

// startWritePhaseLocked builds and injects the write phase once the read
// phase is complete (skipped when the inverter already holds the values
// — WithSkipUnchanged).
func (g *Gateway) startWritePhaseLocked(rt *rtCommand) error {
	frs, wrs, err := writeFrames(rt.cmd, rt.current, g.envFn)
	if err != nil {
		return fmt.Errorf("gateway: build write phase: %w", err)
	}
	if g.skipUnchanged {
		frs, wrs = changedWrites(rt.cmd.Kind, frs, wrs, rt.current)
		if len(frs) == 0 {
			rt.unchanged = true // the inverter already holds it: no write
			g.finishWritesLocked(rt)
			return nil
		}
	}
	rt.writes = wrs
	for i := range frs {
		if ierr := g.inj.InjectS2C(g.flow, frs[i].Raw); ierr != nil {
			return fmt.Errorf("gateway: inject write phase: %w", ierr)
		}
		if i == 0 {
			g.noteWriteLocked(rt.cmd.Kind) // counted once a frame reached the session
		}
	}
	return nil
}

// removeWrite drops the first pending write matching match; reports whether
// one was removed.
func removeWrite(rt *rtCommand, match func(writeReq) bool) bool {
	for i := range rt.writes {
		if match(rt.writes[i]) {
			rt.writes = append(rt.writes[:i], rt.writes[i+1:]...)
			return true
		}
	}
	return false
}

// finishWritesLocked marks the command acked when nothing is pending.
func (g *Gateway) finishWritesLocked(rt *rtCommand) {
	if len(rt.writes) == 0 && len(rt.reads) == 0 {
		rt.status = StatusAcked
		close(rt.done)
		g.fireLocked(rt)
	}
}
