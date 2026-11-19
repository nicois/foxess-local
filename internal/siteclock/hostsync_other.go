//go:build !linux

package siteclock

// HostSynced is not known off Linux (development builds): true.
func HostSynced() bool { return true }
