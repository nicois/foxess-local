//go:build linux

package siteclock

import "golang.org/x/sys/unix"

// HostSynced reports whether this machine's clock is synchronised (the
// kernel's NTP state, as timedatectl shows it): a stick that booted
// with a 1970 clock and no network yet must never set the inverter's.
func HostSynced() bool {
	var tx unix.Timex
	state, err := unix.Adjtimex(&tx) // modes 0: read only
	return err == nil && state != unix.TIME_ERROR && tx.Status&unix.STA_UNSYNC == 0
}
