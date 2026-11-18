package main

import (
	"strings"
	"testing"

	"foxesslocal/internal/api"
)

// procTCP is /proc/net/tcp's shape: 14431 = 0x385F listening (state 0A)
// on 0.0.0.0, 18080 = 0x46A0 established (01) only.
const procTCP = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:385F 00000000:0000 0A 00000000:00000000 00:00000000 00000000   999        0 12345 1 0000000000000000 100 0 0 10 0
   1: 0100007F:46A0 0100007F:C350 01 00000000:00000000 00:00000000 00000000   999        0 12346 1 0000000000000000 20 4 30 10 -1
`

// procTCP6 listens on [::]:22 (0x0016) only.
const procTCP6 = `  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000000000000:0016 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 222 1 0000000000000000 100 0 0 10 0
`

// TestListeningPort (F3): a port counts as listening only in state 0A,
// in either table; a malformed line is skipped.
func TestListeningPort(t *testing.T) {
	cases := []struct {
		port int
		want bool
	}{{14431, true}, {18080, false}, {22, true}, {80, false}}
	for _, c := range cases {
		got := listeningIn(c.port, strings.NewReader(procTCP), strings.NewReader(procTCP6+"garbage\n"))
		if got != c.want {
			t.Errorf("port %d: %v, want %v", c.port, got, c.want)
		}
	}
}

// TestListenerCheck: the readiness line for the inverter listener — ok
// when bound, fail with the fix otherwise; an unparsable listen address
// is a config failure, not a crash.
func TestListenerCheck(t *testing.T) {
	up := func(int) bool { return true }
	down := func(int) bool { return false }
	if c := listenerCheck("0.0.0.0:14431", up); c.Status != api.CheckOK || c.ID != "listener" {
		t.Fatalf("up: %+v", c)
	}
	if c := listenerCheck("0.0.0.0:14431", down); c.Status != api.CheckFail || c.Fix == "" {
		t.Fatalf("down: %+v", c)
	}
	if c := listenerCheck("nonsense", up); c.Status != api.CheckFail {
		t.Fatalf("bad address: %+v", c)
	}
}

// TestDiskCheck: free space above the guard's level is ok; below it a
// warning (the guard is discarding old raw data); an unreadable
// filesystem a warning too, never a crash.
func TestDiskCheck(t *testing.T) {
	const mb = 1 << 20
	if c := diskCheck(500*mb, 10_000*mb, 200*mb, nil); c.Status != api.CheckOK {
		t.Fatalf("plenty: %+v", c)
	}
	if c := diskCheck(150*mb, 10_000*mb, 200*mb, nil); c.Status != api.CheckWarn || c.Fix == "" {
		t.Fatalf("low: %+v", c)
	}
	if c := diskCheck(0, 0, 200*mb, errFake); c.Status != api.CheckWarn {
		t.Fatalf("unreadable: %+v", c)
	}
}

var errFake = fakeErr("statfs failed")

type fakeErr string

func (e fakeErr) Error() string { return string(e) }
