package main

import (
	"testing"

	"foxesslocal/internal/api"
	"foxesslocal/internal/gateway"
)

// TestGwObsReadsPublishNothing: reads change no configuration — an
// acked read (poll or installer probe) publishes no config event, and a
// failed poll raises no system alert (the poller retries next tick; an
// outage must not storm the event feed). A failed installer probe still
// alerts.
func TestGwObsReadsPublishNothing(t *testing.T) {
	h := &handle{bus: api.NewBus()}
	sub := h.bus.Subscribe()
	defer sub.Close()
	busLen := func(*api.Bus) int {
		n := 0
		for {
			select {
			case <-sub.Events():
				n++
			default:
				return n
			}
		}
	}
	h.gwObs(gateway.Result{ID: "p1", Kind: gateway.KindRead, Origin: gateway.OriginPoll, Status: gateway.StatusAcked})
	h.gwObs(gateway.Result{ID: "p2", Kind: gateway.KindRead, Origin: gateway.OriginPoll, Status: gateway.StatusFailed, Detail: "no response"})
	h.gwObs(gateway.Result{ID: "r1", Kind: gateway.KindRead, Origin: "api", Status: gateway.StatusAcked})
	if n := busLen(h.bus); n != 0 {
		t.Fatalf("reads published %d events, want 0", n)
	}
	h.gwObs(gateway.Result{ID: "r2", Kind: gateway.KindRead, Origin: "api", Status: gateway.StatusFailed, Detail: "modbus exception 2"})
	if n := busLen(h.bus); n != 1 {
		t.Fatalf("a failed probe published %d events, want 1 alert", n)
	}
}
