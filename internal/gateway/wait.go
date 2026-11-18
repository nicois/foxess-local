package gateway

import (
	"context"
	"fmt"
)

// ackRingCap bounds how many acked commands remain Wait-able after they
// leave the in-flight slot (Pending drops them; Wait consumes them once).
const ackRingCap = 256

// Result is the terminal outcome of one command.
type Result struct {
	ID     string
	Kind   CommandKind
	Origin string // the submitter (OriginPoll for the live-value poller)
	Status Status // StatusAcked, StatusFailed, or StatusExpired
	Detail string // set when failed
	// Registers holds a read's values (KindRead, acked), in address order.
	Registers []RegisterValue
	// Unchanged: acked without writing — the inverter already held the
	// values (WithSkipUnchanged).
	Unchanged bool
}

// Wait blocks until the command with the given id reaches a terminal
// state (acked, failed, or expired), or ctx is done. The caller sets the
// read-back timeout through ctx (context.WithTimeout). It returns the
// outcome, or an error wrapping ErrUnknownCommand when the id was never
// submitted (or was already consumed by an earlier Wait).
func (g *Gateway) Wait(ctx context.Context, id string) (Result, error) {
	for {
		g.mu.Lock()
		g.expireInFlightLocked()
		g.expireLocked()
		rt, fromRing := g.findCommand(id)
		if rt == nil {
			g.mu.Unlock()
			return Result{}, fmt.Errorf("%w: %s", ErrUnknownCommand, id)
		}
		if terminal(rt.status) {
			res := rt.result()
			if fromRing {
				g.dropAcked(id)
			}
			g.mu.Unlock()
			return res, nil
		}
		g.mu.Unlock()
		select {
		case <-rt.done:
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}
}

// findCommand locates the command in every lifecycle set: in flight,
// queued, terminal-visible (stale), or the acked ring.
func (g *Gateway) findCommand(id string) (*rtCommand, bool) {
	if g.rt != nil && g.rt.id == id {
		return g.rt, false
	}
	for _, rt := range g.queued {
		if rt.id == id {
			return rt, false
		}
	}
	for _, rt := range g.stale {
		if rt.id == id {
			return rt, false
		}
	}
	for _, rt := range g.acked {
		if rt.id == id {
			return rt, true
		}
	}
	return nil, false
}

// dropAcked removes a consumed acked command from the ring.
func (g *Gateway) dropAcked(id string) {
	for i, rt := range g.acked {
		if rt.id == id {
			g.acked = append(g.acked[:i], g.acked[i+1:]...)
			return
		}
	}
}

// terminal reports the command's terminal lifecycle states.
func terminal(s Status) bool {
	return s == StatusAcked || s == StatusFailed || s == StatusExpired
}

// result lifts the terminal outcome for Wait.
func (rt *rtCommand) result() Result {
	res := Result{ID: rt.id, Kind: rt.cmd.Kind, Origin: rt.cmd.Origin, Status: rt.status, Detail: rt.detail, Unchanged: rt.unchanged}
	if rt.cmd.Kind == KindRead && rt.status == StatusAcked {
		res.Registers = append([]RegisterValue(nil), rt.current...)
	}
	return res
}
