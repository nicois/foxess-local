package main

import (
	"testing"

	"foxesslocal/internal/forward"
	"foxesslocal/internal/policy"
)

// TestWindowReconnect: a maintenance-window transition changes the
// serve decision (a cloud leg or none) only in off mode — there the live
// session must end so the inverter reconnects under it: opening gives
// the cloud its leg now, closing or expiry takes it away now. In
// telemetry_only / full every session is mirrored already (the window
// acts per frame), and a re-arm changes nothing.
func TestWindowReconnect(t *testing.T) {
	cases := []struct {
		event string
		mode  policy.ForwardMode
		want  bool
	}{
		{forward.EventMaintenanceOpened, policy.ModeOff, true},
		{forward.EventMaintenanceClosed, policy.ModeOff, true},
		{forward.EventMaintenanceExpired, policy.ModeOff, true},
		{forward.EventMaintenanceExtended, policy.ModeOff, false},
		{forward.EventMaintenanceOpened, policy.ModeTelemetryOnly, false},
		{forward.EventMaintenanceClosed, policy.ModeTelemetryOnly, false},
		{forward.EventMaintenanceOpened, policy.ModeFull, false},
		{forward.EventMaintenanceExpired, policy.ModeFull, false},
	}
	for _, c := range cases {
		reason, got := windowReconnect(c.event, c.mode)
		if got != c.want || (got && reason == "") {
			t.Errorf("%s in %s: reconnect %v (%q), want %v", c.event, c.mode, got, reason, c.want)
		}
	}
}
